// Package vv implements version vectors for anti-entropy (design §7.3).
//
// A version vector maps each origin to the highest CONTIGUOUS sequence number
// held from it. Contiguous is the operative word: with records at 1, 2 and 4
// the entry is 2, not 4, because the vector asserts "I have everything up to
// here" and a peer uses it to decide what to send.
//
// At 8 bytes of node ID plus a varint sequence, an entry costs about 10 bytes.
// Fifty instances is roughly 500 bytes per area — three mesh packets — which is
// why full vectors are exchanged unicast on demand rather than broadcast every
// cycle (§7.3).
//
// # Why the merge laws matter
//
// Convergence rests entirely on Merge being commutative, associative and
// idempotent. If those hold, nodes reach the same state regardless of the order
// or number of times they exchange information — which on a flooding mesh with
// duplicate delivery is not a nicety but the whole correctness argument. If
// they do not hold, no amount of integration testing will save it, so they are
// property-tested rather than assumed.
package vv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/aghman/meshbbs/internal/identity"
	"lukechampine.com/blake3"
)

// Vector maps origins to their highest contiguous sequence.
//
// The zero value is a valid empty vector, but use New for clarity.
type Vector struct {
	entries map[identity.NodeID]uint64
}

// New returns an empty vector.
func New() *Vector { return &Vector{entries: map[identity.NodeID]uint64{}} }

// FromMap builds a vector from a map, copying it.
func FromMap(m map[identity.NodeID]uint64) *Vector {
	v := New()
	for k, s := range m {
		if s > 0 {
			v.entries[k] = s
		}
	}
	return v
}

// Get returns the highest contiguous sequence held for an origin, or zero.
func (v *Vector) Get(origin identity.NodeID) uint64 {
	if v == nil || v.entries == nil {
		return 0
	}
	return v.entries[origin]
}

// Set records a high-water mark, keeping the larger of old and new.
//
// It never decreases: a vector that could go backwards would make a peer
// re-send records we already hold, and in the worst case would mask the
// sequence regression that §6.2.1 rule 3 exists to catch.
func (v *Vector) Set(origin identity.NodeID, seq uint64) {
	if v.entries == nil {
		v.entries = map[identity.NodeID]uint64{}
	}
	if seq > v.entries[origin] {
		v.entries[origin] = seq
	}
}

// Len is the number of origins known.
func (v *Vector) Len() int {
	if v == nil {
		return 0
	}
	return len(v.entries)
}

// Origins returns the known origins, SORTED.
//
// Sorted because callers encode and hash this, and Go randomises map iteration
// — an unsorted walk would produce a different digest on every call and make
// two identical vectors look divergent (§6.2.1 rule 2).
func (v *Vector) Origins() []identity.NodeID {
	if v == nil || len(v.entries) == 0 {
		return nil
	}
	out := make([]identity.NodeID, 0, len(v.entries))
	for k := range v.entries {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		for b := 0; b < identity.NodeIDLen; b++ {
			if out[i][b] != out[j][b] {
				return out[i][b] < out[j][b]
			}
		}
		return false
	})
	return out
}

// Clone returns an independent copy.
func (v *Vector) Clone() *Vector {
	out := New()
	if v == nil {
		return out
	}
	for k, s := range v.entries {
		out.entries[k] = s
	}
	return out
}

// Merge folds other into v, taking the pointwise maximum.
//
// This is the operation the CRDT laws apply to. Pointwise max over a map is
// commutative, associative and idempotent by construction — the tests exist to
// keep it that way through future changes, not because it is in doubt today.
func (v *Vector) Merge(other *Vector) {
	if other == nil {
		return
	}
	if v.entries == nil {
		v.entries = map[identity.NodeID]uint64{}
	}
	for k, s := range other.entries {
		if s > v.entries[k] {
			v.entries[k] = s
		}
	}
}

// Merged returns a new vector without modifying either input.
func Merged(a, b *Vector) *Vector {
	out := a.Clone()
	out.Merge(b)
	return out
}

// Equal reports whether two vectors hold the same entries.
func (v *Vector) Equal(other *Vector) bool {
	if v.Len() != other.Len() {
		return false
	}
	for k, s := range v.entries {
		if other.entries[k] != s {
			return false
		}
	}
	return true
}

// Dominates reports whether v holds at least everything other holds.
//
// This is what lets a node decide it has nothing to learn from a peer, and so
// what makes digest suppression possible (§7.3).
func (v *Vector) Dominates(other *Vector) bool {
	if other == nil {
		return true
	}
	for k, s := range other.entries {
		if v.Get(k) < s {
			return false
		}
	}
	return true
}

// Missing returns the ranges v needs from other, as (origin, from, to) with
// `from` and `to` inclusive.
//
// This is the delta request of §7.3: about 10 bytes per range, and the reason a
// peer that notices divergence can ask for exactly what it lacks rather than a
// full resend.
type Range struct {
	Origin identity.NodeID
	From   uint64 // inclusive
	To     uint64 // inclusive
}

// Missing computes what v lacks relative to other, ordered by origin so the
// request is deterministic.
func (v *Vector) Missing(other *Vector) []Range {
	if other == nil {
		return nil
	}
	var out []Range
	for _, origin := range other.Origins() {
		theirs := other.Get(origin)
		mine := v.Get(origin)
		if theirs > mine {
			out = append(out, Range{Origin: origin, From: mine + 1, To: theirs})
		}
	}
	return out
}

// Count returns the total number of records the vector accounts for.
//
// Useful as a cheap divergence hint in a digest, alongside the rolling hash.
func (v *Vector) Count() uint64 {
	var n uint64
	if v == nil {
		return 0
	}
	for _, s := range v.entries {
		n += s
	}
	return n
}

// Hash is a compact fingerprint of the vector.
//
// Digests carry this rather than the whole vector (§7.3): at fifty instances a
// full vector is three mesh packets per area, and broadcasting that every cycle
// is exactly the digest storm the design rejects. A hash mismatch proves
// divergence and triggers a unicast exchange of the real thing.
func (v *Vector) Hash() [4]byte {
	h := blake3.New(32, nil)
	for _, origin := range v.Origins() {
		h.Write(origin[:])
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v.Get(origin))
		h.Write(buf[:])
	}
	var out [4]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ---------------------------------------------------------------------------
// Wire encoding
// ---------------------------------------------------------------------------

// MaxOrigins bounds a decoded vector, so a hostile peer cannot force a large
// allocation with a small packet.
const MaxOrigins = 512

// ErrTruncated is returned when input ends mid-entry.
var ErrTruncated = errors.New("truncated version vector")

// Encode serialises the vector.
//
// Layout: count(uvarint) | (origin[8] | seq(uvarint))* , origins ascending.
// Sorted order makes the encoding canonical, so the same vector always
// produces the same bytes and a digest over it is stable.
func (v *Vector) Encode() []byte {
	origins := v.Origins()
	buf := make([]byte, 0, 2+len(origins)*10)
	buf = binary.AppendUvarint(buf, uint64(len(origins)))
	for _, o := range origins {
		buf = append(buf, o[:]...)
		buf = binary.AppendUvarint(buf, v.Get(o))
	}
	return buf
}

// canonicalUvarint reads a uvarint and rejects overlong encodings.
//
// binary.Uvarint happily accepts a padded encoding — 0x80 0x00 decodes to zero
// just as 0x00 does — which would give one logical vector several wire forms.
// That is malleability: a peer could re-encode the same state into different
// bytes, and anything that compares or hashes the encoded form would read
// identical nodes as divergent. Found by the fuzzer, not by inspection.
func canonicalUvarint(b []byte) (uint64, int, error) {
	val, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, ErrTruncated
	}
	// The shortest encoding of val is the only one accepted.
	if want := binary.AppendUvarint(nil, val); len(want) != n {
		return 0, 0, fmt.Errorf("non-canonical varint: %d bytes encode a value that needs %d", n, len(want))
	}
	return val, n, nil
}

// Decode parses the Encode form.
func Decode(b []byte) (*Vector, error) {
	n, read, err := canonicalUvarint(b)
	if err != nil {
		return nil, err
	}
	if n > MaxOrigins {
		return nil, fmt.Errorf("version vector declares %d origins, limit is %d", n, MaxOrigins)
	}
	p := read

	v := New()
	var prev identity.NodeID
	for i := uint64(0); i < n; i++ {
		if p+identity.NodeIDLen > len(b) {
			return nil, ErrTruncated
		}
		var origin identity.NodeID
		copy(origin[:], b[p:p+identity.NodeIDLen])
		p += identity.NodeIDLen

		seq, read, err := canonicalUvarint(b[p:])
		if err != nil {
			return nil, err
		}
		p += read

		// Reject non-canonical encodings: entries must ascend and not repeat.
		// Two byte strings decoding to one vector would let a peer produce a
		// different digest for identical state, which anti-entropy would read
		// as permanent divergence.
		if i > 0 && !less(prev, origin) {
			return nil, errors.New("version vector origins are not in ascending order")
		}
		prev = origin

		v.entries[origin] = seq
	}
	if p != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after version vector", len(b)-p)
	}
	return v, nil
}

func less(a, b identity.NodeID) bool {
	for i := 0; i < identity.NodeIDLen; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
