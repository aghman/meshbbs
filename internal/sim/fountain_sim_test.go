package sim

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
)

// This file is the harness earning its keep: a real subsystem driven across the
// simulated medium rather than against a synthetic loss model.
//
// The fountain package's own tests drop symbols from a slice. That verifies the
// algebra but not the claim §7.2 actually makes, which is about a *broadcast*:
// one transmission, many receivers, each losing an independent subset, each
// decoding as soon as it individually has enough. That claim is the entire
// reason the design chose a fountain code over ARQ, and it cannot be tested
// without a network.

const symSize = 200 // under the 233-byte MTU, leaving room for the 8-byte header

// broadcastBundle sends one bundle as a fountain transmission and reports how
// many of the receivers decoded it correctly.
func broadcastBundle(t *testing.T, seed uint64, receivers int, loss float64, payload []byte) (decoded int, spent time.Duration) {
	t.Helper()

	cfg := DefaultConfig(seed)
	cfg.LossRate = loss
	net := New(cfg)

	senderID := nodeID(1)
	sender := net.NewLink(senderID, 0)

	const bundleID = 0x5EED
	enc, err := fountain.NewEncoder(senderID, bundleID, payload, symSize)
	if err != nil {
		t.Fatal(err)
	}

	// Each receiver runs its own decoder and never talks back. No feedback at
	// all is the point: on a mesh a per-receiver NAK storm costs more airtime
	// than the data (§7.2).
	decs := make([]*fountain.Decoder, receivers)
	recv := make([]*Link, receivers)
	for i := 0; i < receivers; i++ {
		i := i
		recv[i] = net.NewLink(nodeID(byte(i+2)), 0)
		d, err := fountain.NewDecoder(senderID, bundleID, enc.K(), symSize, enc.OrigLen())
		if err != nil {
			t.Fatal(err)
		}
		decs[i] = d
		net.Every(time.Second, func() {
			recv[i].Pump(func(dg link.Datagram) {
				s, err := fountain.DecodeSymbol(dg.Data)
				if err != nil {
					t.Errorf("receiver %d got an undecodable symbol: %v", i, err)
					return
				}
				if _, err := decs[i].Add(s); err != nil {
					t.Errorf("receiver %d rejected symbol %d: %v", i, s.Index, err)
				}
			})
		})
	}

	// Transmit K systematic symbols plus the governor's repair count.
	symbols := enc.Transmission(fountain.RepairCount(enc.K(), loss))
	for i, s := range symbols {
		wire := s.Encode()
		net.After(time.Duration(i)*5*time.Second, func() {
			if err := sender.Send(context.Background(), link.Broadcast, wire); err != nil {
				t.Errorf("send failed: %v", err)
			}
		})
	}

	net.Run(time.Duration(len(symbols)+30) * 5 * time.Second)

	for i, d := range decs {
		if !d.Done() {
			continue
		}
		got, err := d.Payload()
		if err != nil {
			t.Errorf("receiver %d reported done but failed to produce a payload: %v", i, err)
			continue
		}
		if !bytes.Equal(got, payload) {
			t.Errorf("receiver %d decoded %d bytes that do not match the original", i, len(got))
			continue
		}
		decoded++
	}
	return decoded, sender.Spent()
}

// The headline property: one broadcast, every receiver decodes, nobody
// acknowledges anything. Under ARQ this would cost one retransmission round per
// receiver that missed a different packet.
func TestFountainBroadcastReachesEveryReceiver(t *testing.T) {
	payload := make([]byte, 3000) // K=15
	for i := range payload {
		payload[i] = byte(i * 7)
	}

	for _, loss := range []float64{0, 0.05, 0.20, 0.40} {
		loss := loss
		t.Run(fmt.Sprintf("loss=%.0f%%", loss*100), func(t *testing.T) {
			const receivers = 12
			// Several seeds, because a single seed says nothing about a
			// probabilistic property — it says one draw went well.
			const trials = 8
			total, failed := 0, 0
			for seed := uint64(0); seed < trials; seed++ {
				got, _ := broadcastBundle(t, seed, receivers, loss, payload)
				total += got
				failed += receivers - got
			}

			want := receivers * trials
			t.Logf("%d/%d receiver-decodes at %.0f%% loss", total, want, loss*100)

			// RepairCount sizes to about two standard deviations, so a small
			// number of failures is expected rather than a bug. Demanding
			// perfection here would be demanding the governor over-provision
			// airtime it does not have.
			if failed > want/20 {
				t.Errorf("%d of %d receivers failed to decode at %.0f%% loss (over the 5%% allowance)",
					failed, want, loss*100)
			}
		})
	}
}

// The cost side of the same claim. A fountain broadcast must not cost
// meaningfully more airtime than the payload itself on a clean link — that is
// what "systematic" buys, and it is why symbols 0..K-1 are the originals.
func TestSystematicCodeIsNearlyFreeOnACleanLink(t *testing.T) {
	payload := make([]byte, 3000)
	for i := range payload {
		payload[i] = byte(i)
	}

	decoded, spent := broadcastBundle(t, 1, 6, 0, payload)
	if decoded != 6 {
		t.Fatalf("only %d of 6 receivers decoded on a lossless link", decoded)
	}

	cfg := DefaultConfig(1)
	// What the payload alone would cost, ignoring coding entirely.
	floor := time.Duration(float64(len(payload))*float64(cfg.AirtimePerByte)*cfg.FloodMultiplier) +
		0 // no framing

	overhead := float64(spent)/float64(floor) - 1
	t.Logf("payload floor %s, actual %s, overhead %.1f%%", floor, spent, overhead*100)

	// The margin covers the 8-byte header per symbol, the zero padding in the
	// final symbol, and the single spare repair symbol a clean link still gets.
	if overhead > 0.20 {
		t.Errorf("a systematic code on a clean link cost %.1f%% over the payload; "+
			"the systematic prefix is not being used", overhead*100)
	}
}

// Airtime is the binding constraint (§1.1), so the harness has to be able to
// state the cost of a decision in seconds of channel time. This is not an
// assertion so much as a measurement the anti-entropy work will need.
func TestReportBroadcastAirtimeCost(t *testing.T) {
	payload := make([]byte, 3000)
	cfg := DefaultConfig(1)

	t.Logf("a %d-byte bundle to 12 receivers, R=%.0f:", len(payload), cfg.FloodMultiplier)
	for _, loss := range []float64{0, 0.10, 0.30, 0.50} {
		k := (len(payload) + symSize - 1) / symSize
		repair := fountain.RepairCount(k, loss)
		_, spent := broadcastBundle(t, 42, 12, loss, payload)
		t.Logf("  %2.0f%% loss: K=%d + %d repair = %d symbols, %s of channel time (%.2fx the payload)",
			loss*100, k, repair, k+repair, spent.Round(time.Millisecond),
			float64(spent)/(float64(len(payload))*float64(cfg.AirtimePerByte)*cfg.FloodMultiplier))
	}
}

// Duplicates are routine on a flooding mesh. A duplicate symbol carries no new
// information, and the decoder must neither be confused by it nor count it
// towards progress.
func TestDuplicateSymbolsDoNotCorruptDecoding(t *testing.T) {
	cfg := DefaultConfig(77)
	cfg.LossRate = 0.15
	cfg.DuplicateRate = 0.60 // absurd, deliberately
	net := New(cfg)

	senderID := nodeID(1)
	sender := net.NewLink(senderID, 0)

	payload := make([]byte, 2000)
	for i := range payload {
		payload[i] = byte(i * 3)
	}

	enc, err := fountain.NewEncoder(senderID, 0xABCD, payload, symSize)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := fountain.NewDecoder(senderID, 0xABCD, enc.K(), symSize, enc.OrigLen())
	if err != nil {
		t.Fatal(err)
	}

	r := net.NewLink(nodeID(2), 0)
	arrivals := 0
	net.Every(time.Second, func() {
		r.Pump(func(dg link.Datagram) {
			arrivals++
			s, err := fountain.DecodeSymbol(dg.Data)
			if err != nil {
				t.Errorf("undecodable symbol: %v", err)
				return
			}
			if _, err := dec.Add(s); err != nil {
				t.Errorf("decoder rejected a symbol: %v", err)
			}
		})
	})

	symbols := enc.Transmission(fountain.RepairCount(enc.K(), cfg.LossRate))
	for i, s := range symbols {
		wire := s.Encode()
		net.After(time.Duration(i)*5*time.Second, func() {
			_ = sender.Send(context.Background(), link.Broadcast, wire)
		})
	}
	net.Run(time.Duration(len(symbols)+30) * 5 * time.Second)

	if !dec.Done() {
		t.Fatalf("decoder did not finish after %d arrivals for a %d-symbol block", arrivals, enc.K())
	}
	got, err := dec.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload decoded incorrectly in the presence of duplicates")
	}
	// Duplicates must not have inflated the useful symbol count.
	if dec.Received() > enc.K()+fountain.RepairCount(enc.K(), cfg.LossRate) {
		t.Errorf("decoder counted %d useful symbols from a %d-symbol transmission; duplicates were double-counted",
			dec.Received(), len(symbols))
	}
	t.Logf("%d arrivals (with 60%% duplication) yielded %d useful symbols for K=%d",
		arrivals, dec.Received(), enc.K())
}

// Two senders may pick the same random bundle ID. Decoder state keyed only on
// the bundle ID would then mix symbols from two different payloads and produce
// confident garbage, which is worse than failing.
func TestConcurrentSendersWithTheSameBundleIDDoNotInterfere(t *testing.T) {
	cfg := DefaultConfig(5)
	cfg.LossRate = 0.10
	net := New(cfg)

	const sameID = 0x1234
	a, b := nodeID(1), nodeID(2)
	linkA := net.NewLink(a, 0)
	linkB := net.NewLink(b, 0)

	payloadA := bytes.Repeat([]byte("AAAA"), 400)
	payloadB := bytes.Repeat([]byte("BBBB"), 400)

	encA, err := fountain.NewEncoder(a, sameID, payloadA, symSize)
	if err != nil {
		t.Fatal(err)
	}
	encB, err := fountain.NewEncoder(b, sameID, payloadB, symSize)
	if err != nil {
		t.Fatal(err)
	}

	// The receiver keys its decoders on (sender, bundleID), as §7.2 requires.
	decs := map[identity.NodeID]*fountain.Decoder{}
	decs[a], _ = fountain.NewDecoder(a, sameID, encA.K(), symSize, encA.OrigLen())
	decs[b], _ = fountain.NewDecoder(b, sameID, encB.K(), symSize, encB.OrigLen())

	r := net.NewLink(nodeID(3), 0)
	net.Every(time.Second, func() {
		r.Pump(func(dg link.Datagram) {
			s, err := fountain.DecodeSymbol(dg.Data)
			if err != nil {
				return
			}
			if d := decs[dg.From]; d != nil {
				_, _ = d.Add(s)
			}
		})
	})

	// Interleave the two transmissions, so symbols from both are in flight at
	// once — which is what makes the collision dangerous rather than academic.
	symsA := encA.Transmission(fountain.RepairCount(encA.K(), 0.10))
	symsB := encB.Transmission(fountain.RepairCount(encB.K(), 0.10))
	longest := max(len(symsA), len(symsB))
	for i := 0; i < longest; i++ {
		i := i
		net.After(time.Duration(i)*5*time.Second, func() {
			if i < len(symsA) {
				_ = linkA.Send(context.Background(), link.Broadcast, symsA[i].Encode())
			}
			if i < len(symsB) {
				_ = linkB.Send(context.Background(), link.Broadcast, symsB[i].Encode())
			}
		})
	}
	net.Run(time.Duration(longest+30) * 5 * time.Second)

	for id, want := range map[identity.NodeID][]byte{a: payloadA, b: payloadB} {
		d := decs[id]
		if !d.Done() {
			t.Fatalf("decoder for %s did not finish", id.Short())
		}
		got, err := d.Payload()
		if err != nil {
			t.Fatalf("decoder for %s: %v", id.Short(), err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("decoder for %s produced the wrong payload; two senders sharing a bundle ID interfered",
				id.Short())
		}
	}
}
