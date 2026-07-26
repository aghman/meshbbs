package fountain

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func testSender(b byte) identity.NodeID {
	var id identity.NodeID
	id[0] = b
	return id
}

// roundTrip encodes payload, delivers the given symbol indices, and returns the
// decoded payload.
func roundTrip(t *testing.T, sender identity.NodeID, bundleID uint32, payload []byte, symSize int, deliver []uint16) ([]byte, bool) {
	t.Helper()
	enc, err := NewEncoder(sender, bundleID, payload, symSize)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(sender, bundleID, enc.K(), symSize, enc.OrigLen())
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range deliver {
		// Symbols always travel over the wire, so exercise the codec too.
		wire := enc.Symbol(idx).Encode()
		s, err := DecodeSymbol(wire)
		if err != nil {
			t.Fatalf("symbol %d failed to decode from the wire: %v", idx, err)
		}
		done, err := dec.Add(s)
		if err != nil {
			t.Fatalf("Add(%d): %v", idx, err)
		}
		if done {
			break
		}
	}
	if !dec.Done() {
		return nil, false
	}
	got, err := dec.Payload()
	if err != nil {
		t.Fatal(err)
	}
	return got, true
}

// §7.2's headline property: the systematic prefix means a receiver with no loss
// decodes at ZERO coding overhead. This is what the design gives up pure
// fountain schemes to keep, so it is worth asserting directly.
func TestCleanLinkCostsNothing(t *testing.T) {
	sender := testSender(1)
	src := rng.NewSeeded(1)

	for _, size := range []int{1, 50, 225, 900, 5000} {
		payload := make([]byte, size)
		src.Read(payload)

		enc, err := NewEncoder(sender, 7, payload, 225)
		if err != nil {
			t.Fatal(err)
		}
		// Exactly K symbols, the systematic ones, in order.
		var deliver []uint16
		for i := 0; i < enc.K(); i++ {
			deliver = append(deliver, uint16(i))
		}

		got, ok := roundTrip(t, sender, 7, payload, 225, deliver)
		if !ok {
			t.Fatalf("size %d: K symbols did not decode — the systematic prefix is broken", size)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("size %d: payload mismatch", size)
		}
	}
}

// THE property [D1] rests on: any K+epsilon distinct symbols reconstruct the
// block, whatever subset was lost. If this does not hold, the whole argument
// for choosing fountain coding over ARQ collapses.
func TestAnySufficientSubsetDecodes(t *testing.T) {
	sender := testSender(2)
	src := rng.NewSeeded(42)

	type result struct{ trials, failures, totalOverhead int }
	byK := map[int]*result{}

	for trial := 0; trial < 1500; trial++ {
		k := 1 + src.IntN(40)
		symSize := 32
		payload := make([]byte, (k-1)*symSize+1+src.IntN(symSize-1))
		src.Read(payload)

		enc, err := NewEncoder(sender, uint32(trial), payload, symSize)
		if err != nil {
			t.Fatal(err)
		}
		k = enc.K()

		// Offer a generous stream and drop a random subset, which is what a
		// broadcast medium with independent per-receiver loss actually does.
		lossPct := src.IntN(60)
		var delivered []uint16
		for i := 0; i < k*4+8; i++ {
			if src.IntN(100) >= lossPct {
				delivered = append(delivered, uint16(i))
			}
		}

		dec, err := NewDecoder(sender, uint32(trial), k, symSize, enc.OrigLen())
		if err != nil {
			t.Fatal(err)
		}

		used := 0
		for _, idx := range delivered {
			used++
			done, err := dec.Add(enc.Symbol(idx))
			if err != nil {
				t.Fatal(err)
			}
			if done {
				break
			}
		}

		r := byK[k]
		if r == nil {
			r = &result{}
			byK[k] = r
		}
		r.trials++

		if !dec.Done() {
			r.failures++
			continue
		}
		got, err := dec.Payload()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("trial %d: decoded payload differs from the original", trial)
		}
		r.totalOverhead += used - k
	}

	// The bar is ABSOLUTE overhead, not a percentage.
	//
	// A percentage is misleading at small K: at K=4 you cannot need fractionally
	// fewer than one extra symbol, so an essentially optimal 0.9 reads as 22%.
	// The meaningful figure is how many extra symbols a decoder needs, and for
	// random binary codes the floor is about 1.6 regardless of K — which is
	// exactly the property that makes this scale to larger bundles.
	const maxAbsoluteOverhead = 2.0

	for k := 1; k <= 40; k++ {
		r := byK[k]
		if r == nil || r.trials < 5 {
			continue
		}
		decoded := r.trials - r.failures
		if decoded == 0 {
			t.Errorf("K=%2d: nothing decoded in %d trials", k, r.trials)
			continue
		}
		overhead := float64(r.totalOverhead) / float64(decoded)
		pct := overhead / float64(k) * 100
		t.Logf("K=%2d: %3d trials, %d undecodable, mean overhead %.2f symbols (%.1f%% of K)",
			k, r.trials, r.failures, overhead, pct)

		if overhead > maxAbsoluteOverhead {
			t.Errorf("K=%d: mean overhead %.2f symbols exceeds the %.1f-symbol bar "+
				"(the random-GF(2) floor is about 1.6)", k, overhead, maxAbsoluteOverhead)
		}
	}
}

// §7.2 item 5: K=1 needs no coding at all, and most DMs land there.
func TestSingleSymbolBlock(t *testing.T) {
	sender := testSender(3)
	payload := []byte("a short direct message")

	got, ok := roundTrip(t, sender, 99, payload, 225, []uint16{0})
	if !ok {
		t.Fatal("a one-symbol block did not decode from its single systematic symbol")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q", got)
	}

	// And a repair symbol alone must also work, since a blind repeat is the
	// only redundancy available at K=1.
	got, ok = roundTrip(t, sender, 99, payload, 225, []uint16{1})
	if !ok {
		t.Fatal("a one-symbol block did not decode from a repair symbol")
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("repair-only decode gave %q", got)
	}
}

// The mask is derived, never transmitted. Both ends must compute the same one
// from data they already hold, or repair symbols are useless.
func TestMaskIsDeterministicAndSenderScoped(t *testing.T) {
	a, b := testSender(1), testSender(2)

	for i := 0; i < 200; i++ {
		idx := uint16(10 + i)
		m1 := mask(a, 1234, idx, 8)
		m2 := mask(a, 1234, idx, 8)
		for j := range m1 {
			if m1[j] != m2[j] {
				t.Fatalf("mask is not deterministic at index %d", idx)
			}
		}
	}

	// §7.2 is explicit that the sender is in the tuple: bundle IDs are chosen
	// independently by each node, so without this two nodes colliding on an ID
	// would silently corrupt each other's decodes.
	var differs bool
	for i := 0; i < 50; i++ {
		idx := uint16(10 + i)
		ma := mask(a, 777, idx, 8)
		mb := mask(b, 777, idx, 8)
		for j := range ma {
			if ma[j] != mb[j] {
				differs = true
			}
		}
	}
	if !differs {
		t.Fatal("masks do not depend on the sender; two nodes colliding on a bundle ID " +
			"would corrupt each other's decodes")
	}
}

// A decoder keyed to one block must reject symbols from another, rather than
// mixing them into its equations.
func TestDecoderRejectsForeignSymbols(t *testing.T) {
	sender := testSender(4)
	payload := make([]byte, 300)

	enc, err := NewEncoder(sender, 1, payload, 100)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(sender, 2, enc.K(), 100, enc.OrigLen()) // different bundle ID
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dec.Add(enc.Symbol(0)); err == nil {
		t.Fatal("decoder accepted a symbol from a different bundle")
	}
}

// A flooding mesh delivers the same symbol by several paths. Duplicates must
// not corrupt the equation set or inflate the received count.
func TestDuplicateSymbolsAreIgnored(t *testing.T) {
	sender := testSender(5)
	src := rng.NewSeeded(11)
	payload := make([]byte, 500)
	src.Read(payload)

	enc, err := NewEncoder(sender, 3, payload, 100)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := NewDecoder(sender, 3, enc.K(), 100, enc.OrigLen())
	if err != nil {
		t.Fatal(err)
	}

	for rep := 0; rep < 4; rep++ {
		for i := 0; i < enc.K(); i++ {
			if _, err := dec.Add(enc.Symbol(uint16(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if dec.Received() != enc.K() {
		t.Fatalf("counted %d symbols after delivering %d distinct ones four times",
			dec.Received(), enc.K())
	}
	got, err := dec.Payload()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch after duplicate delivery")
	}
}

// Symbols arrive out of order on a mesh; order must not matter.
func TestOutOfOrderDelivery(t *testing.T) {
	sender := testSender(6)
	src := rng.NewSeeded(13)
	payload := make([]byte, 1000)
	src.Read(payload)

	enc, err := NewEncoder(sender, 4, payload, 128)
	if err != nil {
		t.Fatal(err)
	}
	k := enc.K()

	// Reverse order, then a shuffle.
	var reversed []uint16
	for i := k - 1; i >= 0; i-- {
		reversed = append(reversed, uint16(i))
	}
	got, ok := roundTrip(t, sender, 4, payload, 128, reversed)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatal("reverse-order delivery failed")
	}

	order := make([]uint16, k*2)
	for i := range order {
		order[i] = uint16(i)
	}
	for i := len(order) - 1; i > 0; i-- {
		j := src.IntN(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	got, ok = roundTrip(t, sender, 4, payload, 128, order)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatal("shuffled delivery failed")
	}
}

func TestRepairCountSurvivesTheLossItIsSizedFor(t *testing.T) {
	// A clean link decodes from the systematic prefix; one spare is enough.
	if got := RepairCount(4, 0); got != 1 {
		t.Errorf("clean link at K=4 asks for %d repair symbols, want 1", got)
	}

	// The property that matters: after losing p of the transmission, a receiver
	// must still hold more than K symbols with margin. The design's
	// ceil(p*K)+1 fails this, which is why it was replaced.
	for _, k := range []int{2, 5, 10, 20, 40} {
		for _, p := range []float64{0.05, 0.1, 0.2, 0.3, 0.5} {
			n := k + RepairCount(k, p)
			mean := float64(n) * (1 - p)
			if mean < float64(k)+1.6 {
				t.Errorf("K=%d p=%.0f%%: sends %d symbols, expected arrivals %.1f "+
					"does not clear K+1.6=%.1f", k, p*100, n, mean, float64(k)+1.6)
			}
		}
	}

	// Absurd loss rates must not produce unbounded transmissions.
	if got := RepairCount(10, 5.0); got > 130 {
		t.Errorf("clamping failed: %d repair symbols for a nonsense loss rate", got)
	}
}

// TestRepairCountHoldsTheFailureRate measures what RepairCount actually
// achieves, rather than checking the model it is built on.
//
// The distinction matters, and cost us once. The test above asserts that the
// *expected* arrivals clear K+1.6, which the old constants satisfied while
// still failing to decode 6.2% of the time — because the codec's overhead is
// not 1.6 but a long-tailed distribution with p95 = 5, and because expected
// arrivals say nothing about the half of receivers that do worse than expected.
// A model can be self-consistent and wrong. Only a measured decode rate catches
// that.
//
// So: transmit, drop symbols independently, and count how often the decoder
// fails to finish. The seeds are fixed, so this is a deterministic number and
// not a flaky threshold.
//
// If mask()'s degree distribution changes, epsilon changes with it and this
// test fails. That is the intended alarm: refit the constants against a fresh
// failure curve, do not relax the threshold.
func TestRepairCountHoldsTheFailureRate(t *testing.T) {
	if testing.Short() {
		t.Skip("statistical; needs a few thousand decodes")
	}

	var sender identity.NodeID
	sender[0] = 1

	// The design targets under 2% of receivers needing a repair round. Allow
	// 4% here so an ordinary refit does not have to be exact, but stay well
	// under the 6.2% the original constants produced.
	const allowed = 0.04

	cases := []struct {
		k      int
		loss   float64
		trials int
	}{
		// Small K is where the flat-epsilon assumption was wrong and where most
		// real bundles live: §7.2 says most DMs and small post batches are K=1.
		{1, 0.15, 900},
		{1, 0.50, 900},
		{2, 0.30, 900},
		{3, 0.15, 900},
		{4, 0.50, 900},
		{5, 0.10, 900},
		{10, 0.05, 900},
		{10, 0.20, 900},
		{15, 0.30, 700},
		{20, 0.10, 700},
		{40, 0.05, 250}, // K=40 is O(K^3) to solve; fewer trials
	}

	for _, tc := range cases {
		repair := RepairCount(tc.k, tc.loss)
		failed := 0
		for trial := 0; trial < tc.trials; trial++ {
			src := rng.NewSeeded(uint64(trial)*7919 + uint64(tc.k)*131)
			payload := make([]byte, tc.k*200)
			if _, err := src.Read(payload); err != nil {
				t.Fatal(err)
			}
			enc, err := NewEncoder(sender, uint32(trial), payload, 200)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := NewDecoder(sender, uint32(trial), enc.K(), 200, enc.OrigLen())
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range enc.Transmission(repair) {
				if src.Float64() < tc.loss {
					continue // lost in flight
				}
				if _, err := dec.Add(s); err != nil {
					t.Fatalf("decoder rejected a well-formed symbol: %v", err)
				}
			}
			if !dec.Done() {
				failed++
				continue
			}
			got, err := dec.Payload()
			if err != nil {
				t.Fatalf("K=%d: decoder finished but produced no payload: %v", tc.k, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("K=%d: decoder produced a payload that is not the original", tc.k)
			}
		}

		rate := float64(failed) / float64(tc.trials)
		t.Logf("K=%2d loss=%2.0f%%: K+%d symbols, %.2f%% of receivers failed to decode",
			tc.k, tc.loss*100, repair, rate*100)
		if rate > allowed {
			t.Errorf("K=%d at %.0f%% loss: %.2f%% of receivers failed to decode with %d repair symbols "+
				"(allowed %.0f%%). Refit epsilon and zScore against a fresh failure curve.",
				tc.k, tc.loss*100, rate*100, repair, allowed*100)
		}
	}
}

func TestSymbolWireRoundTrip(t *testing.T) {
	src := rng.NewSeeded(17)
	for i := 0; i < 500; i++ {
		s := Symbol{
			BundleID: uint32(src.Uint64()),
			Index:    uint16(src.IntN(70000)),
			K:        uint8(1 + src.IntN(MaxK)),
			Data:     make([]byte, 1+src.IntN(200)),
		}
		src.Read(s.Data)

		got, err := DecodeSymbol(s.Encode())
		if err != nil {
			t.Fatalf("symbol %d: %v", i, err)
		}
		if got.BundleID != s.BundleID || got.Index != s.Index || got.K != s.K {
			t.Fatalf("symbol %d header mismatch: got %+v want %+v", i, got, s)
		}
		if !bytes.Equal(got.Data, s.Data) {
			t.Fatalf("symbol %d payload mismatch", i)
		}
	}
}

// §7.2's header budget: 8 bytes, leaving 225 of a 233-byte Meshtastic payload.
func TestHeaderFitsTheBudget(t *testing.T) {
	if HeaderSize != 8 {
		t.Fatalf("L1 header is %d bytes, design §7.2 budgets 8", HeaderSize)
	}
	s := Symbol{BundleID: 1, Index: 0, K: 1, Data: make([]byte, 225)}
	if got := len(s.Encode()); got != 233 {
		t.Fatalf("a full mesh symbol encodes to %d bytes, want 233", got)
	}
}

func TestDecodeSymbolRejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{
		nil,
		make([]byte, HeaderSize-1),                // truncated header
		make([]byte, HeaderSize),                  // no payload
		{0, 0, 0, 0, 0, 0, 1, 0, 'x'},             // version 0
		{1 << 6, 0, 0, 0, 0, 0, 0, 0, 'x'},        // K=0
		{1 << 6, 0, 0, 0, 0, 0, MaxK + 1, 0, 'x'}, // K over the limit
	} {
		if _, err := DecodeSymbol(bad); err == nil {
			t.Errorf("accepted malformed symbol % x", bad)
		}
	}
}

func TestEncoderRejectsOversizedBlocks(t *testing.T) {
	sender := testSender(7)
	// A payload needing more than MaxK symbols must be refused rather than
	// silently truncated — the caller should split the bundle.
	huge := make([]byte, (MaxK+1)*32)
	if _, err := NewEncoder(sender, 1, huge, 32); err == nil {
		t.Fatal("accepted a payload needing more than MaxK symbols")
	}
	if _, err := NewEncoder(sender, 1, nil, 32); err == nil {
		t.Fatal("accepted an empty payload")
	}
	if _, err := NewEncoder(sender, 1, []byte("x"), 0); err == nil {
		t.Fatal("accepted a zero symbol size")
	}
}

func FuzzDecodeSymbol(f *testing.F) {
	s := Symbol{BundleID: 1, Index: 3, K: 4, Data: []byte("payload")}
	f.Add(s.Encode())
	f.Add([]byte{})
	f.Add(make([]byte, 32))

	// §12.5: symbols arrive from anyone holding the channel PSK.
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := DecodeSymbol(data)
		if err != nil {
			return
		}
		if got.K == 0 || got.K > MaxK {
			t.Fatalf("accepted K=%d", got.K)
		}
		if len(got.Data) == 0 || len(got.Data) > MaxSymbolSize {
			t.Fatalf("accepted a %d-byte payload", len(got.Data))
		}
		if !bytes.Equal(got.Encode(), data) {
			t.Fatal("symbol encoding is not canonical")
		}
	})
}

var _ = fmt.Sprintf

// A one-symbol bundle needs no coding at all — §7.2 item 5 calls for "optional
// blind repeats" — and most DMs and small post batches are exactly that. A
// repair count that treats K=1 like K=40 puts a 3x airtime bill on the most
// common case on the mesh.
func TestSmallBundlesDoNotPayLargeBundleOverhead(t *testing.T) {
	for _, tc := range []struct {
		k       int
		loss    float64
		maxSent int
	}{
		// At K=1 every symbol decodes the block, so the count is pure blind
		// repetition: enough copies that one survives.
		{1, 0.15, 3},
		{1, 0.30, 4},
		{1, 0.50, 6},
		{2, 0.15, 6},
		{3, 0.15, 9},
	} {
		sent := tc.k + RepairCount(tc.k, tc.loss)
		t.Logf("K=%d at %2.0f%% loss: %d symbols (%.1fx the payload)",
			tc.k, tc.loss*100, sent, float64(sent)/float64(tc.k))
		if sent > tc.maxSent {
			t.Errorf("K=%d at %.0f%% loss sends %d symbols, more than the %d that suffice; "+
				"small bundles are paying large-bundle overhead", tc.k, tc.loss*100, sent, tc.maxSent)
		}
	}
}
