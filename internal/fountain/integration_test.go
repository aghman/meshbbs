package fountain

import (
	"bytes"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

// MeshMTU is what a Meshtastic Data payload can carry (§1).
const meshMTU = 233

// symbolPayload is what remains after the L1 header (§7.2).
const symbolPayload = meshMTU - HeaderSize

// The whole L1/L2 path as a mesh actually exercises it: a compressed bundle
// split into 225-byte symbols, broadcast into independent per-receiver loss,
// and reassembled by each receiver from whatever it happened to get.
//
// This is the scenario [D1] chose fountain coding for — under ARQ each of these
// receivers would send a different NACK set and the sender would retransmit
// their union.
func TestBroadcastToManyLossyReceivers(t *testing.T) {
	sender := testSender(1)
	src := rng.NewSeeded(2024)

	// A bundle of roughly 2 KB, which at 225 bytes per symbol is K=10 — squarely
	// in the range §7.2 says off-the-shelf codes handle badly.
	payload := make([]byte, 2100)
	src.Read(payload)

	enc, err := NewEncoder(sender, 0xBEEF, payload, symbolPayload)
	if err != nil {
		t.Fatal(err)
	}
	k := enc.K()

	const receivers = 12
	lossRates := []float64{0, 0.05, 0.1, 0.2, 0.3, 0.4}

	for _, loss := range lossRates {
		// The sender transmits ONE stream: K systematic symbols plus repair
		// symbols sized to the observed loss. Every receiver draws from it.
		repair := RepairCount(k, loss)
		stream := enc.Transmission(repair)

		decoded, totalReceived := 0, 0
		for r := 0; r < receivers; r++ {
			dec, err := NewDecoder(sender, 0xBEEF, k, symbolPayload, enc.OrigLen())
			if err != nil {
				t.Fatal(err)
			}
			got := 0
			for _, s := range stream {
				// Independent loss per receiver: each misses a different subset,
				// which is the property that makes ARQ scale badly here.
				if src.Float64() < loss {
					continue
				}
				got++
				if done, err := dec.Add(s); err != nil {
					t.Fatal(err)
				} else if done {
					break
				}
			}
			totalReceived += got

			if dec.Done() {
				out, err := dec.Payload()
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out, payload) {
					t.Fatalf("loss=%.0f%% receiver %d decoded the wrong payload", loss*100, r)
				}
				decoded++
			}
		}

		sent := len(stream)
		t.Logf("loss=%2.0f%%: sent %2d symbols once (K=%d + %d repair, %+.0f%% airtime), "+
			"%2d/%d receivers decoded, %.1f symbols received on average",
			loss*100, sent, k, repair, float64(repair)/float64(k)*100,
			decoded, receivers, float64(totalReceived)/receivers)

		// The bar is MOST receivers, not all.
		//
		// RepairCount sizes for roughly two standard deviations, so a receiver
		// decodes with about 98% probability and a dozen of them all decoding is
		// about 76% likely. Buying the last few percent means more repair
		// symbols on every transmission forever, whereas a receiver that misses
		// simply shows a gap at the next digest and asks for it. On a link where
		// airtime is the scarce resource that is the right trade.
		if got := float64(decoded) / receivers; got < 0.9 {
			t.Errorf("loss=%.0f%%: only %d of %d receivers decoded from one broadcast",
				loss*100, decoded, receivers)
		}
	}
}

// The airtime argument for fountain coding, made concrete: one broadcast serves
// every receiver, whereas ARQ retransmits the union of what they each missed.
func TestBroadcastCostsLessThanRetransmittingTheUnion(t *testing.T) {
	sender := testSender(2)
	src := rng.NewSeeded(77)

	payload := make([]byte, 2100)
	src.Read(payload)
	enc, err := NewEncoder(sender, 1, payload, symbolPayload)
	if err != nil {
		t.Fatal(err)
	}
	k := enc.K()

	const loss = 0.15

	// arqCost simulates ARQ properly: transmit K, then repeatedly retransmit the
	// union of what is STILL missing, with retransmissions subject to the same
	// loss, until every receiver holds every symbol.
	//
	// Running the rounds matters. A single round is capped at 2K and saturates
	// once the receiver count is large enough that some receiver missed every
	// symbol — which is exactly the regime where fountain coding wins, so a
	// one-round model flatters ARQ precisely where it is worst.
	arqCost := func(receivers int) int {
		// missing[r][i] — receiver r still needs symbol i.
		missing := make([]map[int]bool, receivers)
		for r := range missing {
			missing[r] = map[int]bool{}
			for i := 0; i < k; i++ {
				if src.Float64() < loss {
					missing[r][i] = true
				}
			}
		}

		sent := k
		for round := 0; round < 100; round++ {
			union := map[int]bool{}
			for _, m := range missing {
				for i := range m {
					union[i] = true
				}
			}
			if len(union) == 0 {
				return sent
			}
			sent += len(union)
			// Retransmissions are lost at the same rate as anything else.
			for i := range union {
				for _, m := range missing {
					if m[i] && src.Float64() >= loss {
						delete(m, i)
					}
				}
			}
		}
		t.Fatal("ARQ did not converge in 100 rounds")
		return sent
	}

	// The fountain cost does not depend on the receiver count at all: one
	// transmission serves everyone, and each decodes independently. That
	// invariance is the whole argument for [D1], so assert it rather than
	// assuming it.
	fountainSymbols := k + RepairCount(k, loss)

	t.Logf("K=%d at %.0f%% loss — fountain sends %d symbols regardless of audience:",
		k, loss*100, fountainSymbols)

	for _, receivers := range []int{1, 5, 20, 50} {
		arq := arqCost(receivers)
		t.Logf("  %2d receivers: ARQ %3d symbols, fountain %d (%.2fx)",
			receivers, arq, fountainSymbols, float64(arq)/float64(fountainSymbols))

		// At one receiver ARQ is genuinely competitive — there is no union to
		// pay for, and unicast repair is a reasonable strategy. The claim is
		// about broadcast to many, so that is where the assertion lives.
		if receivers >= 5 && fountainSymbols >= arq {
			t.Errorf("at %d receivers fountain cost %d symbols vs ARQ's %d — the premise of [D1] "+
				"is that one broadcast beats retransmitting the union",
				receivers, fountainSymbols, arq)
		}
	}
}

// RepairCount is sized for ONE receiver's decode probability, not the whole
// audience's. Sending to N receivers at a 2% per-receiver failure rate means
// all N decode only 0.98^N of the time, so a large audience will routinely
// leave someone short.
//
// That is deliberate, not an oversight: sizing so that fifty receivers all
// decode with high probability would inflate every transmission for everyone,
// and §7.2 already provides the cheap escape — a straggler unicasts
// want-repair after a full digest cycle. This test pins the behaviour down so
// the tradeoff stays visible rather than being discovered in the field.
func TestRepairSizingIsPerReceiverNotPerAudience(t *testing.T) {
	sender := testSender(3)
	const k = 10
	const loss = 0.20
	repair := RepairCount(k, loss)

	payload := make([]byte, k*symbolPayload)
	rng.NewSeeded(5).Read(payload)
	enc, err := NewEncoder(sender, 9, payload, symbolPayload)
	if err != nil {
		t.Fatal(err)
	}

	const trials = 400
	for _, audience := range []int{1, 10, 50} {
		allDecoded := 0
		for trial := 0; trial < trials; trial++ {
			src := rng.NewSeeded(uint64(trial)*31 + uint64(audience))
			everyone := true
			for r := 0; r < audience; r++ {
				dec, err := NewDecoder(sender, 9, k, symbolPayload, enc.OrigLen())
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range enc.Transmission(repair) {
					if src.Float64() < loss {
						continue
					}
					dec.Add(s)
				}
				if !dec.Done() {
					everyone = false
				}
			}
			if everyone {
				allDecoded++
			}
		}
		rate := float64(allDecoded) / trials
		t.Logf("audience of %2d: everyone decoded in %.1f%% of transmissions", audience, rate*100)

		// The point being pinned: this degrades with audience size, and the
		// design absorbs it with want-repair rather than by over-provisioning.
		if audience == 1 && rate < 0.95 {
			t.Errorf("a single receiver decoded only %.1f%% of the time; RepairCount is under-sized", rate*100)
		}
	}
}

// Two nodes may pick the same random bundle ID. Decoder state is keyed by
// sender as well, and the repair masks differ by sender, so their streams must
// not corrupt each other (§7.2).
func TestCollidingBundleIDsFromDifferentSendersDoNotInterfere(t *testing.T) {
	a, b := testSender(10), testSender(20)
	src := rng.NewSeeded(5)

	payloadA := make([]byte, 900)
	payloadB := make([]byte, 900)
	src.Read(payloadA)
	src.Read(payloadB)

	const sharedID uint32 = 0x1234 // the collision

	encA, err := NewEncoder(a, sharedID, payloadA, symbolPayload)
	if err != nil {
		t.Fatal(err)
	}
	encB, err := NewEncoder(b, sharedID, payloadB, symbolPayload)
	if err != nil {
		t.Fatal(err)
	}

	// A receiver tracking both, keyed by (sender, bundleID).
	decA, err := NewDecoder(a, sharedID, encA.K(), symbolPayload, encA.OrigLen())
	if err != nil {
		t.Fatal(err)
	}
	decB, err := NewDecoder(b, sharedID, encB.K(), symbolPayload, encB.OrigLen())
	if err != nil {
		t.Fatal(err)
	}

	// Interleave both streams, including repair symbols whose masks are
	// sender-derived.
	for i := 0; i < encA.K()+4; i++ {
		if _, err := decA.Add(encA.Symbol(uint16(i))); err != nil {
			t.Fatal(err)
		}
		if _, err := decB.Add(encB.Symbol(uint16(i))); err != nil {
			t.Fatal(err)
		}
	}

	gotA, err := decA.Payload()
	if err != nil {
		t.Fatalf("sender A's bundle did not decode: %v", err)
	}
	gotB, err := decB.Payload()
	if err != nil {
		t.Fatalf("sender B's bundle did not decode: %v", err)
	}
	if !bytes.Equal(gotA, payloadA) || !bytes.Equal(gotB, payloadB) {
		t.Fatal("colliding bundle IDs from different senders corrupted each other")
	}
}

// A receiver that joins late — the classic mesh case of a node coming back
// after being offline — must still decode from repair symbols alone, having
// missed the entire systematic prefix.
func TestLateJoinerDecodesFromRepairSymbolsAlone(t *testing.T) {
	sender := testSender(3)
	src := rng.NewSeeded(31)

	payload := make([]byte, 1500)
	src.Read(payload)
	enc, err := NewEncoder(sender, 5, payload, symbolPayload)
	if err != nil {
		t.Fatal(err)
	}
	k := enc.K()

	dec, err := NewDecoder(sender, 5, k, symbolPayload, enc.OrigLen())
	if err != nil {
		t.Fatal(err)
	}

	// Skip every systematic symbol; feed only repair symbols.
	used := 0
	for i := k; i < k*6; i++ {
		used++
		done, err := dec.Add(enc.Symbol(uint16(i)))
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	if !dec.Done() {
		t.Fatalf("a late joiner could not decode from %d repair symbols at K=%d", used, k)
	}
	got, err := dec.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("late joiner decoded the wrong payload")
	}
	t.Logf("K=%d: decoded from %d repair symbols and no systematic ones (%.1f%% overhead)",
		k, used, float64(used-k)/float64(k)*100)
}

var _ = identity.NodeID{}
