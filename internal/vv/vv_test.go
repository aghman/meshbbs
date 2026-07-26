package vv

import (
	"bytes"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func randomVectors(t *testing.T, n, maxOrigins int, seed uint64) []*Vector {
	t.Helper()
	src := rng.NewSeeded(seed)
	out := make([]*Vector, n)
	for i := range out {
		v := New()
		for j := 0; j < 1+src.IntN(maxOrigins); j++ {
			var id identity.NodeID
			// A small pool of origins, so vectors actually overlap and the
			// merge laws are exercised rather than trivially satisfied.
			id[0] = byte(src.IntN(6))
			v.Set(id, uint64(1+src.IntN(50)))
		}
		out[i] = v
	}
	return out
}

// §7.3: convergence rests on Merge being commutative. If a merge depends on
// which side you start from, two nodes exchanging the same information in
// different orders end up in different states.
func TestMergeIsCommutative(t *testing.T) {
	vs := randomVectors(t, 400, 8, 1)
	for i := 0; i+1 < len(vs); i += 2 {
		a, b := vs[i], vs[i+1]
		if !Merged(a, b).Equal(Merged(b, a)) {
			t.Fatalf("merge is not commutative at %d", i)
		}
	}
}

// Associativity is what lets a node fold in updates as they arrive, in any
// grouping, rather than having to batch them.
func TestMergeIsAssociative(t *testing.T) {
	vs := randomVectors(t, 300, 6, 2)
	for i := 0; i+2 < len(vs); i += 3 {
		a, b, c := vs[i], vs[i+1], vs[i+2]
		left := Merged(Merged(a, b), c)
		right := Merged(a, Merged(b, c))
		if !left.Equal(right) {
			t.Fatalf("merge is not associative at %d", i)
		}
	}
}

// Idempotence is the one the mesh actually depends on: flooding delivers the
// same digest by several paths, so merging it repeatedly must change nothing.
func TestMergeIsIdempotent(t *testing.T) {
	vs := randomVectors(t, 300, 8, 3)
	for i, v := range vs {
		once := Merged(v, v)
		twice := Merged(once, v)
		if !once.Equal(v) {
			t.Fatalf("merging a vector with itself changed it at %d", i)
		}
		if !twice.Equal(once) {
			t.Fatalf("repeated merges are not idempotent at %d", i)
		}
	}
}

// A version vector must never go backwards. If it could, a peer would resend
// records we hold, and a genuine sequence regression (§6.2.1 rule 3) could be
// masked instead of caught.
func TestSetNeverDecreases(t *testing.T) {
	v := New()
	var id identity.NodeID
	id[0] = 1

	v.Set(id, 10)
	v.Set(id, 5)
	if got := v.Get(id); got != 10 {
		t.Fatalf("Set decreased the high-water mark to %d", got)
	}
	v.Set(id, 11)
	if got := v.Get(id); got != 11 {
		t.Fatalf("Set failed to advance: %d", got)
	}
}

func TestMergeTakesPointwiseMaximum(t *testing.T) {
	var a1, a2 identity.NodeID
	a1[0], a2[0] = 1, 2

	x := New()
	x.Set(a1, 5)
	x.Set(a2, 9)

	y := New()
	y.Set(a1, 7)
	y.Set(a2, 3)

	m := Merged(x, y)
	if m.Get(a1) != 7 || m.Get(a2) != 9 {
		t.Fatalf("merge is not pointwise max: a1=%d a2=%d", m.Get(a1), m.Get(a2))
	}
}

func TestDominates(t *testing.T) {
	var id identity.NodeID
	id[0] = 1

	ahead, behind := New(), New()
	ahead.Set(id, 10)
	behind.Set(id, 4)

	if !ahead.Dominates(behind) {
		t.Error("a strictly ahead vector should dominate")
	}
	if behind.Dominates(ahead) {
		t.Error("a behind vector must not dominate")
	}
	if !ahead.Dominates(ahead) {
		t.Error("a vector should dominate itself")
	}

	// Disjoint origins: neither dominates.
	var other identity.NodeID
	other[0] = 2
	side := New()
	side.Set(other, 1)
	if ahead.Dominates(side) || side.Dominates(ahead) {
		t.Error("vectors with disjoint origins must not dominate each other")
	}
}

// §7.3: the delta request. A peer asks for exactly the ranges it lacks.
func TestMissingComputesTheGaps(t *testing.T) {
	var a, b identity.NodeID
	a[0], b[0] = 1, 2

	mine := New()
	mine.Set(a, 5)

	theirs := New()
	theirs.Set(a, 9)
	theirs.Set(b, 3)

	missing := mine.Missing(theirs)
	if len(missing) != 2 {
		t.Fatalf("got %d ranges, want 2: %+v", len(missing), missing)
	}
	// Ordered by origin, so a request is deterministic.
	if missing[0].Origin != a || missing[0].From != 6 || missing[0].To != 9 {
		t.Errorf("first range wrong: %+v", missing[0])
	}
	if missing[1].Origin != b || missing[1].From != 1 || missing[1].To != 3 {
		t.Errorf("second range wrong: %+v", missing[1])
	}

	// Nothing missing when we already dominate.
	if got := theirs.Missing(mine); len(got) != 0 {
		t.Errorf("expected no ranges, got %+v", got)
	}
}

// §6.2.1 rule 2 in spirit: the same vector must always produce the same bytes
// and the same hash, whatever order entries were inserted in. Go randomises map
// iteration, so this is a real hazard rather than a theoretical one.
func TestEncodingAndHashAreDeterministic(t *testing.T) {
	var a, b, c identity.NodeID
	a[0], b[0], c[0] = 3, 1, 2

	build := func(order []identity.NodeID) *Vector {
		v := New()
		for i, id := range order {
			v.Set(id, uint64(10+i))
		}
		return v
	}

	// Same entries, different insertion orders. Values are tied to position, so
	// construct them explicitly instead.
	v1 := New()
	v1.Set(a, 1)
	v1.Set(b, 2)
	v1.Set(c, 3)

	v2 := New()
	v2.Set(c, 3)
	v2.Set(a, 1)
	v2.Set(b, 2)

	if !bytes.Equal(v1.Encode(), v2.Encode()) {
		t.Fatal("insertion order changed the encoding")
	}
	if v1.Hash() != v2.Hash() {
		t.Fatal("insertion order changed the hash")
	}

	// And repeated calls agree.
	for i := 0; i < 50; i++ {
		if !bytes.Equal(v1.Encode(), v1.Encode()) || v1.Hash() != v1.Hash() {
			t.Fatal("encoding or hash is not stable across calls")
		}
	}
	_ = build
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for i, v := range randomVectors(t, 300, 10, 4) {
		got, err := Decode(v.Encode())
		if err != nil {
			t.Fatalf("vector %d: %v", i, err)
		}
		if !got.Equal(v) {
			t.Fatalf("vector %d did not survive a round trip", i)
		}
		if got.Hash() != v.Hash() {
			t.Fatalf("vector %d hash changed across a round trip", i)
		}
	}
}

func TestEmptyVectorRoundTrips(t *testing.T) {
	got, err := Decode(New().Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != 0 {
		t.Fatalf("empty vector decoded to %d entries", got.Len())
	}
}

// A non-canonical encoding would let one logical vector have two byte forms,
// so two nodes with identical state could compute different digests and chase
// a divergence that does not exist.
func TestDecodeRejectsNonCanonicalOrdering(t *testing.T) {
	var a, b identity.NodeID
	a[0], b[0] = 1, 2

	// Hand-build an encoding with descending origins.
	buf := []byte{2}
	buf = append(buf, b[:]...)
	buf = append(buf, 5)
	buf = append(buf, a[:]...)
	buf = append(buf, 7)

	if _, err := Decode(buf); err == nil {
		t.Fatal("accepted a vector whose origins were out of order")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{
		nil,
		{0xff},                     // truncated count varint
		{1},                        // count with no entry
		append([]byte{1}, 1, 2, 3), // truncated origin
	} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("accepted malformed input % x", bad)
		}
	}

	// Trailing bytes are rejected too: a decoder that ignores them lets one
	// vector have many encodings.
	v := New()
	var id identity.NodeID
	id[0] = 1
	v.Set(id, 3)
	if _, err := Decode(append(v.Encode(), 0x00)); err == nil {
		t.Error("accepted trailing bytes after a vector")
	}
}

// §7.3 sizing: about 10 bytes per origin, so 50 instances is roughly 500 bytes
// per area — three mesh packets, which is why digests carry a hash instead.
func TestEncodedSizeMatchesTheDesignBudget(t *testing.T) {
	v := New()
	src := rng.NewSeeded(9)
	for i := 0; i < 50; i++ {
		var id identity.NodeID
		src.Read(id[:])
		v.Set(id, uint64(1+src.IntN(60000)))
	}

	size := len(v.Encode())
	perOrigin := float64(size) / 50
	if perOrigin > 12 {
		t.Errorf("version vector costs %.1f bytes per origin, design §7.3 budgets ~10", perOrigin)
	}
	t.Logf("50 origins encode to %d bytes (%.1f per origin)", size, perOrigin)

	// And the digest form is tiny by comparison — that contrast is the whole
	// argument for not broadcasting full vectors.
	if got := len(v.Hash()); got != 4 {
		t.Errorf("hash is %d bytes, want 4", got)
	}
}

func TestHashDetectsDivergence(t *testing.T) {
	var id identity.NodeID
	id[0] = 1

	a := New()
	a.Set(id, 5)
	b := New()
	b.Set(id, 5)

	if a.Hash() != b.Hash() {
		t.Fatal("identical vectors produced different hashes")
	}
	b.Set(id, 6)
	if a.Hash() == b.Hash() {
		t.Fatal("a changed vector produced the same hash")
	}
}

func TestCloneIsIndependent(t *testing.T) {
	var id identity.NodeID
	id[0] = 1
	original := New()
	original.Set(id, 5)

	c := original.Clone()
	c.Set(id, 99)

	if original.Get(id) != 5 {
		t.Fatal("mutating a clone changed the original")
	}
}

func FuzzDecode(f *testing.F) {
	v := New()
	var id identity.NodeID
	id[0] = 7
	v.Set(id, 42)
	f.Add(v.Encode())
	f.Add([]byte{})
	f.Add([]byte{0})

	// §12.5: this parses bytes from any peer on a public channel.
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Decode(data)
		if err != nil {
			return
		}
		if got.Len() > MaxOrigins {
			t.Fatalf("decoded %d origins, over the %d limit", got.Len(), MaxOrigins)
		}
		// Anything that parses must re-encode to exactly the input, or the
		// encoding is not canonical and two byte strings share one vector.
		if !bytes.Equal(got.Encode(), data) {
			t.Fatalf("decode/encode is not canonical")
		}
	})
}
