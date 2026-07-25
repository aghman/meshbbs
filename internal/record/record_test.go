package record

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func testKey(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustNew(t *testing.T, k identity.NodeKey, r Record) *Record {
	t.Helper()
	out, err := New(k, r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func samplePost(t *testing.T, k identity.NodeKey) *Record {
	return mustNew(t, k, Record{
		Seq:  42,
		TS:   1_700_000_000,
		Type: TypePost,
		Area: AreaTagFor("general"),
		Body: []byte("Anyone else running this off solar?"),
	})
}

// §6.2.1 rule 2 / §12.2: encoding the same record must produce byte-identical
// output every time. This is the property that a stray map iteration breaks,
// and the failure mode is a signature that verifies only on the machine that
// created it.
func TestEncodingDeterminism(t *testing.T) {
	k := testKey(t, 1)
	src := rng.NewSeeded(99)

	for i := 0; i < 200; i++ {
		body := make([]byte, src.IntN(300))
		src.Read(body)
		var parent ID
		if i%2 == 0 {
			src.Read(parent[:])
		}
		base := Record{
			Seq:    uint64(src.Uint64() % 1e6),
			TS:     uint32(src.Uint64()),
			Type:   TypePost,
			Area:   AreaTagFor("area-" + string(rune('a'+i%26))),
			Parent: parent,
			Body:   body,
		}

		first := mustNew(t, k, base)
		for j := 0; j < 20; j++ {
			again := mustNew(t, k, base)
			if !bytes.Equal(first.SignedBytes(), again.SignedBytes()) {
				t.Fatalf("encoding is not deterministic at i=%d j=%d", i, j)
			}
			if first.ID() != again.ID() {
				t.Fatalf("record ID is not deterministic at i=%d j=%d", i, j)
			}
			if !bytes.Equal(first.Signature(), again.Signature()) {
				t.Fatalf("signature is not deterministic at i=%d j=%d", i, j)
			}
		}
	}
}

// §12.2: marshal/unmarshal round-trips, preserving every field and the ID.
func TestMarshalRoundTrip(t *testing.T) {
	k := testKey(t, 2)
	src := rng.NewSeeded(7)

	for i := 0; i < 500; i++ {
		body := make([]byte, src.IntN(1000))
		src.Read(body)
		var parent ID
		if i%3 == 0 {
			src.Read(parent[:])
		}
		orig := mustNew(t, k, Record{
			Seq:    src.Uint64() % 100000,
			TS:     uint32(src.Uint64()),
			Type:   TypePost,
			Area:   AreaTagFor("general"),
			Parent: parent,
			Body:   body,
		})

		got, err := Unmarshal(orig.Marshal())
		if err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.ID() != orig.ID() {
			t.Fatalf("ID mismatch after round-trip")
		}
		if got.Origin != orig.Origin || got.Seq != orig.Seq || got.TS != orig.TS ||
			got.Type != orig.Type || got.Area != orig.Area || got.Parent != orig.Parent {
			t.Fatalf("header mismatch after round-trip")
		}
		if !bytes.Equal(got.Body, orig.Body) {
			t.Fatalf("body mismatch after round-trip")
		}
		if err := got.Verify(k.Public); err != nil {
			t.Fatalf("Verify after round-trip: %v", err)
		}
	}
}

// A record must not verify against a key that is not its origin's — the
// self-certifying check from §6.1.1.
func TestVerifyRejectsWrongKey(t *testing.T) {
	a, b := testKey(t, 3), testKey(t, 4)
	r := samplePost(t, a)
	if err := r.Verify(b.Public); err == nil {
		t.Fatal("record verified against the wrong node's key")
	}
	if err := r.Verify(a.Public); err != nil {
		t.Fatalf("record failed to verify against its own key: %v", err)
	}
}

// Any mutation of the signed bytes must be detected.
func TestVerifyDetectsTampering(t *testing.T) {
	k := testKey(t, 5)
	r := samplePost(t, k)
	wire := r.Marshal()

	for i := range wire {
		tampered := append([]byte(nil), wire...)
		tampered[i] ^= 0x01

		got, err := Unmarshal(tampered)
		if err != nil {
			continue // rejected at parse time, which is also a detection
		}
		if err := got.Verify(k.Public); err == nil {
			t.Fatalf("tampering at byte %d went undetected", i)
		}
	}
}

// New must refuse to sign a record attributed to a different origin: catching
// it locally beats discovering it on a remote peer.
func TestNewRejectsForeignOrigin(t *testing.T) {
	a, b := testKey(t, 6), testKey(t, 7)
	_, err := New(a, Record{Origin: b.ID(), Seq: 1, Type: TypePost})
	if err == nil {
		t.Fatal("New signed a record whose origin was a different node")
	}
}

// §6.2.1: the body cap must be enforced on the declared length, before
// allocation, so a hostile peer cannot force a large allocation with a short
// packet.
func TestBodyCapEnforced(t *testing.T) {
	k := testKey(t, 8)
	_, err := New(k, Record{Seq: 1, Type: TypePost, Body: make([]byte, MaxBodyLen+1)})
	if err == nil {
		t.Fatal("New accepted an over-long body")
	}

	// Hand-craft a header declaring a huge body but carrying none.
	r := mustNew(t, k, Record{Seq: 1, Type: TypePost, Body: []byte("x")})
	wire := r.Marshal()
	// The body length varint sits just before the 1-byte body and the 64-byte
	// signature. Rewrite it to a large value.
	idx := len(wire) - 64 - 1 - 1
	wire[idx] = 0xFF // large single-byte uvarint continuation start
	if _, err := Unmarshal(wire); err == nil {
		t.Fatal("Unmarshal accepted a record declaring an oversized body")
	}
}

func TestUnknownTypeAndFlagsRejected(t *testing.T) {
	k := testKey(t, 9)
	r := samplePost(t, k)
	wire := r.Marshal()

	bad := append([]byte(nil), wire...)
	bad[2] = 200 // unknown type
	if _, err := Unmarshal(bad); err == nil {
		t.Fatal("Unmarshal accepted an unknown record type")
	}

	bad = append([]byte(nil), wire...)
	bad[1] = 0x80 // unknown flag bit
	if _, err := Unmarshal(bad); err == nil {
		t.Fatal("Unmarshal accepted unknown flag bits")
	}

	bad = append([]byte(nil), wire...)
	bad[0] = FormatVersion + 1
	if _, err := Unmarshal(bad); err == nil {
		t.Fatal("Unmarshal accepted a future format version")
	}
}

func TestTruncatedInputRejected(t *testing.T) {
	k := testKey(t, 10)
	wire := samplePost(t, k).Marshal()
	for n := 0; n < len(wire); n++ {
		if _, err := Unmarshal(wire[:n]); err == nil {
			t.Fatalf("Unmarshal accepted input truncated to %d bytes", n)
		}
	}
}

// §6.1.2: a NODE record validates with no external input at all.
func TestNodeRecordSelfVerifies(t *testing.T) {
	k := testKey(t, 11)
	r, err := NewNodeRecord(k, 1, 1_700_000_000, "pnw-bbs", "sysop@example", 0)
	if err != nil {
		t.Fatal(err)
	}
	body, err := VerifyNodeRecord(r)
	if err != nil {
		t.Fatalf("VerifyNodeRecord: %v", err)
	}
	if body.DisplayName != "pnw-bbs" {
		t.Fatalf("display name round-trip failed: %q", body.DisplayName)
	}
	if !r.Origin.Matches(body.PublicKey) {
		t.Fatal("NODE body key does not hash to the record origin")
	}
}

// A NODE record carrying someone else's public key must be rejected: this is
// the check that makes identity unspoofable without a registry.
func TestNodeRecordRejectsMismatchedKey(t *testing.T) {
	a, b := testKey(t, 12), testKey(t, 13)
	r, err := NewNodeRecord(a, 1, 0, "a", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Swap in b's public key, keeping a's origin and signature.
	forged := *r
	body, _ := UnmarshalNodeBody(r.Body)
	body.PublicKey = b.Public
	newBody, _ := MarshalNodeBody(body)
	forged.Body = newBody

	if _, err := VerifyNodeRecord(&forged); err == nil {
		t.Fatal("a NODE record advertising a foreign key was accepted")
	}
}

// Display names are rendered into a terminal and arrive from strangers, so
// control characters must be rejected at the parser (ANSI injection).
func TestNodeBodyRejectsControlCharacters(t *testing.T) {
	k := testKey(t, 14)
	if _, err := NewNodeRecord(k, 1, 0, "evil\x1b[2Jname", "", 0); err == nil {
		t.Fatal("a display name containing an escape sequence was accepted")
	}
	if _, err := NewNodeRecord(k, 1, 0, strings.Repeat("x", MaxDisplayNameLen+1), "", 0); err == nil {
		t.Fatal("an over-long display name was accepted")
	}
}

// §12.7: the byte budget is an assertion, not a document. Appendix B claims a
// top-level post leaves ~140 bytes for the body within one 233-byte packet;
// hold the per-record overhead to what the budget assumes.
func TestRecordOverheadBudget(t *testing.T) {
	k := testKey(t, 15)

	// A top-level post: no parent.
	top := mustNew(t, k, Record{Seq: 1, TS: 1_700_000_000, Type: TypePost,
		Area: AreaTagFor("general"), Body: nil})
	// Overhead = everything except the body: format+flags+type+origin+seq+ts+
	// area+bodylen, plus the signature.
	const maxTopOverhead = 88
	if got := len(top.Marshal()); got > maxTopOverhead {
		t.Errorf("top-level record overhead is %d bytes, budget is %d (§12.7, Appendix B)",
			got, maxTopOverhead)
	}

	// A threaded reply pays 16 more for the parent reference.
	var parent ID
	for i := range parent {
		parent[i] = byte(i)
	}
	reply := mustNew(t, k, Record{Seq: 1, TS: 1_700_000_000, Type: TypePost,
		Area: AreaTagFor("general"), Parent: parent, Body: nil})
	const maxReplyOverhead = maxTopOverhead + IDLen
	if got := len(reply.Marshal()); got > maxReplyOverhead {
		t.Errorf("threaded record overhead is %d bytes, budget is %d (§12.7, Appendix B)",
			got, maxReplyOverhead)
	}

	t.Logf("record overhead: top-level %d bytes, threaded %d bytes",
		len(top.Marshal()), len(reply.Marshal()))
}

func FuzzUnmarshal(f *testing.F) {
	k, err := identity.GenerateNodeKey(rng.TestSecret(16))
	if err != nil {
		f.Fatal(err)
	}
	r, err := New(k, Record{Seq: 1, TS: 1, Type: TypePost, Body: []byte("seed")})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(r.Marshal())
	nr, err := NewNodeRecord(k, 2, 1, "n", "c", 0)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(nr.Marshal())
	f.Add([]byte{})
	f.Add(make([]byte, 128))

	// §12.5: the parser is reachable by anyone holding the channel PSK, so it
	// must never panic and never allocate unboundedly on hostile input.
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Unmarshal(data)
		if err != nil {
			return
		}
		if len(got.Body) > MaxBodyLen {
			t.Fatalf("parsed a body of %d bytes, over the %d limit", len(got.Body), MaxBodyLen)
		}
		// Anything that parses must re-marshal to exactly the input, or the
		// codec has a non-canonical representation — which would let two
		// different byte strings share a record ID.
		if !bytes.Equal(got.Marshal(), data) {
			t.Fatalf("parse/serialize is not canonical")
		}
	})
}
