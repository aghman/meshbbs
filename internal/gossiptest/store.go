// Package gossiptest provides an in-memory Store for exercising the gossip
// engine (design §7.3, §12.1).
//
// It exists so that a fifty-node federation can be simulated for thirty virtual
// days in milliseconds. Driving the real SQLite store would make that
// impossible — not because SQLite is slow, but because fifty databases with
// their own connection pools reintroduce exactly the scheduling nondeterminism
// §12.1 forbids, and a simulation that cannot replay from its seed is not worth
// running.
//
// This is a test double, so it is deliberately strict where the real store is
// forgiving: it rejects unsigned records, refuses sequence regressions, and
// verifies every signature. A double that accepts what production rejects
// proves nothing.
package gossiptest

import (
	"fmt"
	"sort"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

// Store is an in-memory record log for one node.
type Store struct {
	areas map[record.AreaTag]*areaLog
	order []record.AreaTag // sorted, so iteration is deterministic

	// keys maps origins to public keys, so signatures can be verified. In
	// production this comes from NODE records; here it is seeded directly.
	keys map[identity.NodeID][]byte

	// Rejected counts records refused, so a test can assert that a corrupted
	// or forged record was actually caught rather than silently dropped.
	Rejected int
}

type areaLog struct {
	// records is origin → seq → record. Sparse: gaps are normal, because a
	// record can arrive before its predecessor.
	records map[identity.NodeID]map[uint64]*record.Record
	// vector is the CONTIGUOUS high-water mark, which is what a peer needs to
	// decide what to send. Holding 1, 2 and 4 puts the entry at 2, not 4.
	vector *vv.Vector
}

// NewStore builds an empty store for the given areas.
func NewStore(areas ...record.AreaTag) *Store {
	s := &Store{
		areas: map[record.AreaTag]*areaLog{},
		keys:  map[identity.NodeID][]byte{},
	}
	for _, a := range areas {
		s.areas[a] = &areaLog{
			records: map[identity.NodeID]map[uint64]*record.Record{},
			vector:  vv.New(),
		}
		s.order = append(s.order, a)
	}
	sort.Slice(s.order, func(i, j int) bool {
		for b := 0; b < 4; b++ {
			if s.order[i][b] != s.order[j][b] {
				return s.order[i][b] < s.order[j][b]
			}
		}
		return false
	})
	return s
}

// TrustKey registers an origin's public key so its records can be verified.
func (s *Store) TrustKey(id identity.NodeID, pub []byte) {
	s.keys[id] = append([]byte(nil), pub...)
}

// Areas implements gossip.Store.
func (s *Store) Areas() []record.AreaTag {
	return append([]record.AreaTag(nil), s.order...)
}

// Vector implements gossip.Store.
func (s *Store) Vector(area record.AreaTag) *vv.Vector {
	if a := s.areas[area]; a != nil {
		return a.vector
	}
	return vv.New()
}

// Records implements gossip.Store.
func (s *Store) Records(area record.AreaTag, r vv.Range) []*record.Record {
	a := s.areas[area]
	if a == nil {
		return nil
	}
	byOrigin := a.records[r.Origin]
	if byOrigin == nil {
		return nil
	}
	var out []*record.Record
	for seq := r.From; seq <= r.To; seq++ {
		if rec := byOrigin[seq]; rec != nil {
			out = append(out, rec)
		}
	}
	return out
}

// Apply implements gossip.Store. It is idempotent.
func (s *Store) Apply(area record.AreaTag, recs []*record.Record) (int, error) {
	a := s.areas[area]
	if a == nil {
		return 0, fmt.Errorf("not a federated area: %x", area)
	}

	applied := 0
	for _, rec := range recs {
		if rec == nil || rec.Seq == 0 {
			s.Rejected++
			continue
		}
		// Verify against the origin's key. Anti-entropy relays records between
		// nodes that never met, so "it came from a peer we trust" is not a
		// statement about who wrote it (§6.1.1).
		pub, known := s.keys[rec.Origin]
		if !known {
			s.Rejected++
			continue
		}
		if err := rec.Verify(pub); err != nil {
			s.Rejected++
			continue
		}
		if rec.Area != area {
			s.Rejected++
			continue
		}

		byOrigin := a.records[rec.Origin]
		if byOrigin == nil {
			byOrigin = map[uint64]*record.Record{}
			a.records[rec.Origin] = byOrigin
		}
		if existing, dup := byOrigin[rec.Seq]; dup {
			// Same slot twice. Identical content is ordinary duplicate
			// delivery; different content means an origin signed two records
			// at one sequence, which is equivocation and must not be papered
			// over by last-writer-wins.
			if existing.ID() != rec.ID() {
				s.Rejected++
			}
			continue
		}
		byOrigin[rec.Seq] = rec
		applied++
	}

	if applied > 0 {
		s.advance(a)
	}
	return applied, nil
}

// advance recomputes the contiguous high-water mark for each origin.
func (s *Store) advance(a *areaLog) {
	for origin, byOrigin := range a.records {
		seq := a.vector.Get(origin)
		for byOrigin[seq+1] != nil {
			seq++
		}
		a.vector.Set(origin, seq)
	}
}

// Insert seeds locally authored records, failing loudly if any is refused.
//
// It goes through Apply rather than around it, so a test cannot seed state that
// the engine would have rejected on the wire — which would make the test
// describe a network that cannot exist.
func (s *Store) Insert(area record.AreaTag, recs ...*record.Record) error {
	before := s.Rejected
	n, err := s.Apply(area, recs)
	if err != nil {
		return err
	}
	if n != len(recs) {
		return fmt.Errorf("inserted %d of %d records (%d rejected); "+
			"is the origin's key registered with TrustKey?",
			n, len(recs), s.Rejected-before)
	}
	return nil
}

// Total counts records held across all areas, for convergence assertions.
func (s *Store) Total() int {
	n := 0
	for _, a := range s.areas {
		for _, byOrigin := range a.records {
			n += len(byOrigin)
		}
	}
	return n
}

// IDs returns every record ID held in an area, sorted. Two converged nodes must
// return identical slices — that is the convergence assertion, and it is
// stronger than comparing version vectors, which can agree while the underlying
// records differ.
func (s *Store) IDs(area record.AreaTag) []string {
	a := s.areas[area]
	if a == nil {
		return nil
	}
	var out []string
	for _, byOrigin := range a.records {
		for _, rec := range byOrigin {
			out = append(out, rec.ID().String())
		}
	}
	sort.Strings(out)
	return out
}
