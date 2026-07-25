// Package rng provides the injected randomness source required by design §12.1.
//
// Two distinct kinds of randomness exist in this system and conflating them is
// a security bug:
//
//   - Source is *deterministic* randomness: jitter, backoff, fountain symbol
//     masks, test data. It must be reproducible from a seed so that a failing
//     simulation run replays exactly (§12.1).
//   - Secret is *cryptographic* randomness: key generation, nonces, PSKs. It
//     must never be seeded or reproducible.
//
// Domain code takes a Source. Only identity and crypto code takes a Secret,
// and in production Secret is always crypto/rand.
package rng

import (
	crand "crypto/rand"
	"encoding/binary"
	"io"
	mrand "math/rand/v2"
)

// Source is deterministic, seedable randomness for domain logic.
type Source interface {
	Uint64() uint64
	// Float64 returns a value in [0.0, 1.0).
	Float64() float64
	// IntN returns a value in [0, n). It panics if n <= 0.
	IntN(n int) int
	// Read fills b with pseudo-random bytes. It never returns an error.
	Read(b []byte) (int, error)
	// Derive returns an independent Source deterministically derived from this
	// one and label. Use it to give a subsystem its own stream so that adding a
	// call in one subsystem does not shift every other subsystem's sequence —
	// which would otherwise make simulation runs fragile to unrelated changes.
	Derive(label string) Source
}

type seeded struct {
	r    *mrand.Rand
	seed [32]byte
}

// NewSeeded returns a deterministic Source. The same seed always produces the
// same sequence, on every platform and architecture.
func NewSeeded(seed uint64) Source {
	var s [32]byte
	binary.LittleEndian.PutUint64(s[:8], seed)
	return newFromState(s)
}

func newFromState(s [32]byte) *seeded {
	return &seeded{
		r: mrand.New(mrand.NewChaCha8(s)),
		// Keep the state so Derive can mix it without consuming the stream.
		seed: s,
	}
}

func (s *seeded) Uint64() uint64   { return s.r.Uint64() }
func (s *seeded) Float64() float64 { return s.r.Float64() }
func (s *seeded) IntN(n int) int   { return s.r.IntN(n) }

func (s *seeded) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(s.r.Uint64())
	}
	return len(b), nil
}

func (s *seeded) Derive(label string) Source {
	// Mix the parent seed with the label. Deliberately not using the parent's
	// output stream: deriving must not perturb the parent's sequence.
	var next [32]byte
	copy(next[:], s.seed[:])
	for i, c := range []byte(label) {
		next[i%32] ^= c + byte(i)
	}
	return newFromState(next)
}

// Secret is cryptographic randomness. It is never seeded and never
// reproducible.
type Secret interface {
	Read(b []byte) (int, error)
	Reader() io.Reader
}

type cryptoSecret struct{}

// NewSecret returns the production cryptographic source (crypto/rand).
func NewSecret() Secret { return cryptoSecret{} }

func (cryptoSecret) Read(b []byte) (int, error) { return io.ReadFull(crand.Reader, b) }
func (cryptoSecret) Reader() io.Reader          { return crand.Reader }

// TestSecret returns a *deterministic* Secret for tests that need reproducible
// key material. It must never be used in production; the name is deliberately
// unpleasant so that a review catches it.
func TestSecret(seed uint64) Secret { return testSecret{NewSeeded(seed)} }

type testSecret struct{ s Source }

func (t testSecret) Read(b []byte) (int, error) { return t.s.Read(b) }
func (t testSecret) Reader() io.Reader          { return &sourceReader{t.s} }

type sourceReader struct{ s Source }

func (r *sourceReader) Read(b []byte) (int, error) { return r.s.Read(b) }
