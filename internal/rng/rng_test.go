package rng

import (
	"bytes"
	"testing"
)

// §12.1: the same seed must produce the same sequence. This is the property
// that makes a failing simulation run replayable from its seed alone.
func TestSeededIsReproducible(t *testing.T) {
	collect := func(seed uint64) []uint64 {
		s := NewSeeded(seed)
		out := make([]uint64, 64)
		for i := range out {
			out[i] = s.Uint64()
		}
		return out
	}

	a, b := collect(12345), collect(12345)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed diverged at index %d", i)
		}
	}

	c := collect(12346)
	if equalU64(a, c) {
		t.Fatal("different seeds produced identical sequences")
	}
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSeededReadIsReproducible(t *testing.T) {
	read := func(seed uint64) []byte {
		s := NewSeeded(seed)
		b := make([]byte, 128)
		s.Read(b)
		return b
	}
	if !bytes.Equal(read(9), read(9)) {
		t.Fatal("Read is not reproducible for a fixed seed")
	}
	if bytes.Equal(read(9), read(10)) {
		t.Fatal("Read produced identical bytes for different seeds")
	}
}

// Derive gives a subsystem its own stream. The important property is that
// deriving does NOT consume the parent's stream — otherwise adding a call in
// one subsystem would shift every other subsystem's sequence, making
// simulation runs fragile to unrelated changes.
func TestDeriveDoesNotPerturbParent(t *testing.T) {
	withDerive := NewSeeded(42)
	_ = withDerive.Derive("network")
	_ = withDerive.Derive("timing")
	a := withDerive.Uint64()

	without := NewSeeded(42)
	b := without.Uint64()

	if a != b {
		t.Fatal("Derive consumed the parent's stream")
	}
}

func TestDeriveStreamsAreDistinctAndStable(t *testing.T) {
	parent := NewSeeded(7)
	x1 := parent.Derive("alpha").Uint64()
	y1 := parent.Derive("beta").Uint64()
	if x1 == y1 {
		t.Fatal("different labels produced the same stream")
	}

	// Deriving the same label again must reproduce the same stream.
	x2 := NewSeeded(7).Derive("alpha").Uint64()
	if x1 != x2 {
		t.Fatal("the same label produced a different stream on a fresh parent")
	}
}

func TestIntNRange(t *testing.T) {
	s := NewSeeded(3)
	for i := 0; i < 1000; i++ {
		if v := s.IntN(10); v < 0 || v >= 10 {
			t.Fatalf("IntN(10) returned %d", v)
		}
	}
}

// Secret must NOT be reproducible: it backs key generation.
func TestSecretIsNotReproducible(t *testing.T) {
	read := func() []byte {
		b := make([]byte, 32)
		if _, err := NewSecret().Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if bytes.Equal(read(), read()) {
		t.Fatal("crypto secret source produced identical output twice")
	}
}

func TestTestSecretIsReproducible(t *testing.T) {
	read := func(seed uint64) []byte {
		b := make([]byte, 32)
		if _, err := TestSecret(seed).Read(b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if !bytes.Equal(read(1), read(1)) {
		t.Fatal("TestSecret is not reproducible")
	}
}
