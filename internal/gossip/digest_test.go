package gossip

import (
	"bytes"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

func tag(s string) record.AreaTag { return record.AreaTagFor(s) }

func node(b byte) identity.NodeID {
	var id identity.NodeID
	id[0] = b
	return id
}

func vec(pairs ...any) *vv.Vector {
	v := vv.New()
	for i := 0; i < len(pairs); i += 2 {
		v.Set(node(byte(pairs[i].(int))), uint64(pairs[i+1].(int)))
	}
	return v
}

// The airtime claim §7.3 rests on: ten areas fit in one mesh packet. If a
// digest ever outgrows that, the whole storm analysis has to be redone.
func TestTenAreasFitInOnePacket(t *testing.T) {
	areas := map[record.AreaTag]*vv.Vector{}
	for i := 0; i < 10; i++ {
		areas[tag(string(rune('a'+i)))] = vec(1, 100, 2, 200, 3, 300)
	}
	d := NewDigest(areas)
	size := len(d.Encode())

	if size != d.Size() {
		t.Errorf("Size() reports %d but Encode produced %d", d.Size(), size)
	}
	// 233 is the Meshtastic MTU. The design budgets 100 bytes for ten areas;
	// the 3-byte envelope takes it to 103.
	if size > 233 {
		t.Fatalf("a ten-area digest is %d bytes and no longer fits one packet", size)
	}
	if size != 103 {
		t.Errorf("ten-area digest is %d bytes, expected 103 (3 envelope + 10x10)", size)
	}
	t.Logf("ten areas: %d bytes, %d of the 233-byte MTU", size, size)
}

// Every control message must fit one mesh packet, at its own declared limit.
//
// A control message that spans packets has to be fragmented, so it can arrive
// partially and need its own repair — a request that itself requires reliable
// delivery. That recursion is exactly what the no-session design avoids, so the
// limits are derived from the MTU and this test holds them to it.
//
// Full version vectors are the deliberate exception (§7.3): one is ~500 bytes
// at fifty instances, which is why they are exchanged on demand rather than
// broadcast.
func TestControlMessagesFitOnePacket(t *testing.T) {
	areas := map[record.AreaTag]*vv.Vector{}
	tags := make([]record.AreaTag, 0, MaxAreas)
	for i := 0; i < MaxAreas; i++ {
		tg := tag(string(rune('a' + i)))
		tags = append(tags, tg)
		areas[tg] = vec(1, 65535, 2, 65535)
	}

	for _, tc := range []struct {
		name string
		size int
	}{
		{"digest at MaxAreas", len(NewDigest(areas).Encode())},
		{"vector request at MaxAreas", len((&VectorReq{Areas: tags}).Encode())},
	} {
		t.Logf("%-28s %3d bytes (limit %d, MTU %d)", tc.name, tc.size, MaxControlMessage, MeshMTU)
		if tc.size > MaxControlMessage {
			t.Errorf("%s is %d bytes, over the %d-byte single-packet budget",
				tc.name, tc.size, MaxControlMessage)
		}
	}
}

// A range's encoded width grows with the magnitude of its sequence numbers, so
// a count-based limit fits one packet in a young area and quietly overflows in
// a mature one. That is a fragmentation bug that would only surface after a
// deployment had been running for months.
//
// FitRanges measures instead of estimating, so assert it holds across the whole
// range of plausible sequence magnitudes.
func TestRangeRequestsFitOnePacketAtAnySequenceMagnitude(t *testing.T) {
	for _, mag := range []uint64{1, 1 << 7, 1 << 14, 1 << 20, 1 << 28, 1 << 35, 1 << 48} {
		var ranges []vv.Range
		for i := 0; i < MaxRanges; i++ {
			ranges = append(ranges, vv.Range{Origin: node(byte(i + 1)), From: mag, To: mag + 100})
		}
		fitted := FitRanges(tag("general"), ranges, MaxControlMessage)
		size := len((&RangeReq{Area: tag("general"), Ranges: fitted}).Encode())

		perRange := 0.0
		if len(fitted) > 0 {
			perRange = float64(size-7) / float64(len(fitted))
		}
		t.Logf("sequences near 2^%-2d: %2d of %d ranges fit, %3d bytes, %.1f per range",
			bits(mag), len(fitted), MaxRanges, size, perRange)

		if size > MaxControlMessage {
			t.Errorf("at sequence magnitude %d the request is %d bytes, over the %d-byte budget",
				mag, size, MaxControlMessage)
		}
		if len(fitted) == 0 {
			t.Errorf("at sequence magnitude %d no ranges fit at all", mag)
		}
	}
}

func bits(v uint64) int {
	n := 0
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

func TestDigestRoundTrip(t *testing.T) {
	areas := map[record.AreaTag]*vv.Vector{
		tag("general"): vec(1, 5, 2, 9),
		tag("meta"):    vec(3, 1),
		tag("files"):   vv.New(),
	}
	d := NewDigest(areas)
	got, err := DecodeDigest(d.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Areas) != 3 {
		t.Fatalf("decoded %d areas, want 3", len(got.Areas))
	}
	for _, want := range d.Areas {
		gotState, ok := got.Get(want.Tag)
		if !ok {
			t.Fatalf("area %x missing after round trip", want.Tag)
		}
		if gotState != want {
			t.Errorf("area %x: got %+v, want %+v", want.Tag, gotState, want)
		}
	}
}

// A digest must have exactly one wire form. Two encodings of one logical digest
// would let a peer present identical state under a different fingerprint, and
// anti-entropy would read that as permanent divergence — two converged nodes
// exchanging deltas forever, burning airtime, looking exactly like a flaky
// radio. This bug class has now been found three times by fuzzing elsewhere in
// the codebase, so it is asserted here rather than waited for.
func TestDigestEncodingIsCanonical(t *testing.T) {
	areas := map[record.AreaTag]*vv.Vector{
		tag("alpha"): vec(1, 1),
		tag("beta"):  vec(2, 2),
		tag("gamma"): vec(3, 3),
	}

	// Building the same digest repeatedly must give identical bytes, despite Go
	// randomising the map iteration that feeds it.
	first := NewDigest(areas).Encode()
	for i := 0; i < 50; i++ {
		if again := NewDigest(areas).Encode(); !bytes.Equal(first, again) {
			t.Fatal("the same digest encoded to different bytes; map iteration is leaking into the wire format")
		}
	}

	// Areas out of order must be rejected, not silently sorted on decode.
	d, err := DecodeDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Areas) < 2 {
		t.Skip("need at least two areas")
	}
	scrambled := append([]byte(nil), first...)
	// Swap the first two entries.
	a := make([]byte, 10)
	copy(a, scrambled[3:13])
	copy(scrambled[3:13], scrambled[13:23])
	copy(scrambled[13:23], a)
	if _, err := DecodeDigest(scrambled); err == nil {
		t.Error("a digest with areas out of order was accepted; it is a second wire form for one digest")
	}
}

func TestDigestRejectsMalformed(t *testing.T) {
	good := NewDigest(map[record.AreaTag]*vv.Vector{tag("x"): vec(1, 1)}).Encode()

	cases := []struct {
		name string
		in   []byte
	}{
		{"empty", nil},
		{"header only", good[:2]},
		{"truncated entry", good[:len(good)-1]},
		{"trailing bytes", append(append([]byte(nil), good...), 0xFF)},
		{"wrong message type", func() []byte {
			b := append([]byte(nil), good...)
			b[0] = byte(MsgVectorReq)
			return b
		}()},
		{"unknown version", func() []byte {
			b := append([]byte(nil), good...)
			b[1] = 99
			return b
		}()},
		{"too many areas declared", func() []byte {
			b := append([]byte(nil), good...)
			b[2] = MaxAreas + 1
			return b
		}()},
	}
	for _, tc := range cases {
		if _, err := DecodeDigest(tc.in); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// Duplicate area tags are the other way to get two wire forms for one digest:
// which entry wins would decide the fingerprint.
func TestDigestRejectsDuplicateAreas(t *testing.T) {
	d := NewDigest(map[record.AreaTag]*vv.Vector{tag("a"): vec(1, 1)})
	b := d.Encode()
	// Duplicate the single entry.
	dup := append([]byte(nil), b...)
	dup[2] = 2
	dup = append(dup, b[3:13]...)
	if _, err := DecodeDigest(dup); err == nil {
		t.Error("a digest with a repeated area was accepted")
	}
}

func TestCountSaturates(t *testing.T) {
	if got := SaturatingCount(0xFFFE); got != 0xFFFE {
		t.Errorf("count 65534 became %d", got)
	}
	if got := SaturatingCount(1 << 40); got != 0xFFFF {
		t.Errorf("a huge count became %d, want saturation at 65535", got)
	}
	// The property that matters: saturation must not wrap. A wrapped count
	// would make a node with a million records look emptier than one with ten,
	// and the "who is behind?" decision would invert.
	if SaturatingCount(1<<16) < SaturatingCount(1<<15) {
		t.Error("saturating count wrapped; the who-is-behind heuristic would invert")
	}
}

func TestVectorReqRoundTrip(t *testing.T) {
	req := &VectorReq{Areas: []record.AreaTag{tag("z"), tag("a"), tag("m")}}
	got, err := DecodeVectorReq(req.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Areas) != 3 {
		t.Fatalf("decoded %d areas, want 3", len(got.Areas))
	}
	// Encode sorts, so decode must come back sorted.
	for i := 1; i < len(got.Areas); i++ {
		if !lessTag(got.Areas[i-1], got.Areas[i]) {
			t.Error("decoded areas are not ascending")
		}
	}
}

func TestVectorMsgRoundTrip(t *testing.T) {
	msg := &VectorMsg{Area: tag("general"), Vector: vec(1, 10, 2, 20, 3, 30)}
	got, err := DecodeVectorMsg(msg.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Area != msg.Area {
		t.Errorf("area %x != %x", got.Area, msg.Area)
	}
	if !got.Vector.Equal(msg.Vector) {
		t.Error("vector did not survive the round trip")
	}
}

func TestRangeReqRoundTrip(t *testing.T) {
	req := &RangeReq{
		Area: tag("general"),
		Ranges: []vv.Range{
			{Origin: node(3), From: 10, To: 14},
			{Origin: node(1), From: 1, To: 1},
			{Origin: node(1), From: 5, To: 9},
		},
	}
	got, err := DecodeRangeReq(req.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ranges) != 3 {
		t.Fatalf("decoded %d ranges, want 3", len(got.Ranges))
	}
	// Sorted by origin then start.
	want := []vv.Range{
		{Origin: node(1), From: 1, To: 1},
		{Origin: node(1), From: 5, To: 9},
		{Origin: node(3), From: 10, To: 14},
	}
	for i := range want {
		if got.Ranges[i] != want[i] {
			t.Errorf("range %d: got %+v, want %+v", i, got.Ranges[i], want[i])
		}
	}
}

// A delta request is the message a peer sends when it is behind, so its size
// determines whether catching up is affordable. §7.3 budgets ~10 bytes per
// range; encoding the span rather than the absolute end keeps it there.
func TestRangeRequestStaysSmall(t *testing.T) {
	var ranges []vv.Range
	for i := 0; i < 20; i++ {
		ranges = append(ranges, vv.Range{Origin: node(byte(i + 1)), From: 1, To: 40})
	}
	req := &RangeReq{Area: tag("general"), Ranges: ranges}
	size := len(req.Encode())
	perRange := float64(size-7) / 20
	t.Logf("20 ranges: %d bytes, %.1f bytes per range", size, perRange)
	if perRange > 11 {
		t.Errorf("a range costs %.1f bytes; §7.3 budgets about 10", perRange)
	}
}

func TestRangeReqRejectsMalformed(t *testing.T) {
	good := (&RangeReq{
		Area:   tag("general"),
		Ranges: []vv.Range{{Origin: node(1), From: 1, To: 3}, {Origin: node(2), From: 1, To: 3}},
	}).Encode()

	t.Run("descending origins", func(t *testing.T) {
		b := append([]byte(nil), good...)
		// Swap the two origin IDs, making them descend.
		o1 := make([]byte, 8)
		copy(o1, b[7:15])
		// Second entry starts after origin(8) + two uvarints (1 byte each).
		copy(b[7:15], b[17:25])
		copy(b[17:25], o1)
		if _, err := DecodeRangeReq(b); err == nil {
			t.Error("accepted a request whose origins descend")
		}
	})

	t.Run("sequence zero", func(t *testing.T) {
		b := append([]byte(nil), good...)
		b[15] = 0 // From = 0
		if _, err := DecodeRangeReq(b); err == nil {
			t.Error("accepted a range starting at sequence 0")
		}
	})

	t.Run("overlong varint", func(t *testing.T) {
		// 0x81 0x00 decodes to 1 but is not the canonical encoding of 1.
		b := append([]byte(nil), good[:15]...)
		b = append(b, 0x81, 0x00, 0x02)
		b = append(b, good[17:]...)
		if _, err := DecodeRangeReq(b); err == nil {
			t.Error("accepted an overlong varint; one range now has two wire forms")
		}
	})

	t.Run("too many ranges declared", func(t *testing.T) {
		b := append([]byte(nil), good...)
		b[6] = MaxRanges + 1
		if _, err := DecodeRangeReq(b); err == nil {
			t.Error("accepted a request declaring more ranges than the limit")
		}
	})
}

func TestPeekTypeRejectsUnknown(t *testing.T) {
	if _, err := PeekType([]byte{99, FormatVersion}); err == nil {
		t.Error("accepted an unknown message type")
	}
	if _, err := PeekType([]byte{byte(MsgDigest), 99}); err == nil {
		t.Error("accepted an unknown format version")
	}
	if _, err := PeekType([]byte{byte(MsgDigest)}); err == nil {
		t.Error("accepted a one-byte message")
	}
	got, err := PeekType([]byte{byte(MsgRangeReq), FormatVersion, 0})
	if err != nil {
		t.Fatal(err)
	}
	if got != MsgRangeReq {
		t.Errorf("got %s, want %s", got, MsgRangeReq)
	}
}

// Every gossip parser is reachable by anyone holding the channel PSK (§12.5).
// Beyond not crashing, a parse must be canonical: re-encoding what was parsed
// has to reproduce the input exactly, or one logical message has two wire forms
// and anti-entropy can livelock on state that is actually identical.
func FuzzDecodeDigest(f *testing.F) {
	f.Add(NewDigest(map[record.AreaTag]*vv.Vector{tag("a"): vec(1, 1)}).Encode())
	f.Add(NewDigest(map[record.AreaTag]*vv.Vector{
		tag("a"): vec(1, 1), tag("b"): vec(2, 2),
	}).Encode())
	f.Add([]byte{byte(MsgDigest), FormatVersion, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := DecodeDigest(data)
		if err != nil {
			return
		}
		// Re-encode from the PARSED fields, not from retained input bytes —
		// otherwise this compares the input to itself and proves nothing. That
		// mistake made an earlier fuzz target in this codebase vacuous.
		if again := d.Encode(); !bytes.Equal(again, data) {
			t.Fatalf("non-canonical digest accepted:\n input %x\nre-enc %x", data, again)
		}
	})
}

func FuzzDecodeRangeReq(f *testing.F) {
	f.Add((&RangeReq{Area: tag("a"), Ranges: []vv.Range{{Origin: node(1), From: 1, To: 3}}}).Encode())
	f.Add([]byte{byte(MsgRangeReq), FormatVersion, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := DecodeRangeReq(data)
		if err != nil {
			return
		}
		if again := r.Encode(); !bytes.Equal(again, data) {
			t.Fatalf("non-canonical range request accepted:\n input %x\nre-enc %x", data, again)
		}
	})
}

func FuzzDecodeVectorReq(f *testing.F) {
	f.Add((&VectorReq{Areas: []record.AreaTag{tag("a")}}).Encode())
	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := DecodeVectorReq(data)
		if err != nil {
			return
		}
		if again := r.Encode(); !bytes.Equal(again, data) {
			t.Fatalf("non-canonical vector request accepted:\n input %x\nre-enc %x", data, again)
		}
	})
}
