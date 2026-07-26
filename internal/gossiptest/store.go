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
	"bytes"
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

// Entry is one record's identity within an area, for invariant checking.
type Entry struct {
	Origin identity.NodeID
	Seq    uint64
	ID     record.ID
}

// Entries returns every record held in an area, sorted by (origin, seq).
//
// This is the raw material for the invariant checker, and it deliberately
// exposes the (origin, seq) → ID mapping rather than just a set of IDs: the
// interesting violations are about that mapping changing, not about the set
// shrinking. A record replaced by a different record at the same slot leaves
// the count identical.
func (s *Store) Entries(area record.AreaTag) []Entry {
	a := s.areas[area]
	if a == nil {
		return nil
	}
	var out []Entry
	for origin, byOrigin := range a.records {
		for seq, rec := range byOrigin {
			out = append(out, Entry{Origin: origin, Seq: seq, ID: rec.ID()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin != out[j].Origin {
			for b := 0; b < identity.NodeIDLen; b++ {
				if out[i].Origin[b] != out[j].Origin[b] {
					return out[i].Origin[b] < out[j].Origin[b]
				}
			}
		}
		return out[i].Seq < out[j].Seq
	})
	return out
}

// VerifyAll re-verifies every record held in an area.
//
// Apply already verifies on the way in, so this is deliberately redundant. It
// checks a different thing: that nothing has been mutated SINCE admission.
// Records are immutable by contract (§6.2), and an invariant that only tests
// the admission path cannot tell the difference between "we never accepted a
// forgery" and "we accepted one and then quietly rewrote it".
func (s *Store) VerifyAll(area record.AreaTag) error {
	a := s.areas[area]
	if a == nil {
		return nil
	}
	for _, origin := range sortedOrigins(a) {
		byOrigin := a.records[origin]
		pub, known := s.keys[origin]
		if !known {
			return fmt.Errorf("holding records from %s with no key to verify them", origin.Short())
		}
		for _, seq := range sortedSeqs(byOrigin) {
			rec := byOrigin[seq]
			if err := rec.Verify(pub); err != nil {
				return fmt.Errorf("record %s (%s seq %d) no longer verifies: %w",
					rec.ID(), origin.Short(), seq, err)
			}
			// Verify alone is not enough, and the reason is subtle.
			//
			// §6.2.1 rule 1 requires verification to use the exact bytes that
			// were signed, never a re-serialization of parsed fields — so
			// Verify checks the signature against the RETAINED bytes and never
			// looks at Body, Type or Parent. That is correct, and it means a
			// mutated parsed field passes Verify untouched while the record
			// displays something its author never signed.
			//
			// So compare the parsed view against what the signed bytes actually
			// decode to. Re-parsing is safe here precisely because it is not
			// being used to verify anything.
			reparsed, err := record.Unmarshal(rec.Marshal())
			if err != nil {
				return fmt.Errorf("record %s no longer re-parses: %w", rec.ID(), err)
			}
			if !bytes.Equal(reparsed.Body, rec.Body) ||
				reparsed.Type != rec.Type || reparsed.Area != rec.Area ||
				reparsed.Parent != rec.Parent || reparsed.TS != rec.TS {
				return fmt.Errorf("record %s (%s seq %d) has been mutated since admission: "+
					"its fields no longer match its signed bytes",
					rec.ID(), origin.Short(), seq)
			}
			if rec.Origin != origin || rec.Seq != seq {
				return fmt.Errorf("record filed under (%s, %d) claims to be (%s, %d)",
					origin.Short(), seq, rec.Origin.Short(), rec.Seq)
			}
		}
	}
	return nil
}

// ContiguousHighWater recomputes the contiguous high-water mark for an origin
// from the records actually held.
//
// The invariant checker compares this against the stored vector. They are
// maintained separately — the vector is advanced incrementally by Apply — so a
// disagreement means the vector is describing a log that does not exist, which
// makes every peer's delta calculation wrong.
func (s *Store) ContiguousHighWater(area record.AreaTag, origin identity.NodeID) uint64 {
	a := s.areas[area]
	if a == nil {
		return 0
	}
	byOrigin := a.records[origin]
	var seq uint64
	for byOrigin[seq+1] != nil {
		seq++
	}
	return seq
}

func sortedOrigins(a *areaLog) []identity.NodeID {
	out := make([]identity.NodeID, 0, len(a.records))
	for o := range a.records {
		out = append(out, o)
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

func sortedSeqs(m map[uint64]*record.Record) []uint64 {
	out := make([]uint64, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ---------------------------------------------------------------------------
// Deliberate corruption, for testing that invariant checkers work
// ---------------------------------------------------------------------------
//
// These break the guarantees the store otherwise enforces. They exist for one
// purpose: proving that an invariant check actually rejects a violation. A
// checker nobody has watched fail is indistinguishable from one that cannot
// fail, and this repo has already shipped a fuzz target that was structurally
// unable to detect the bug class it was written for.
//
// Never call these from a scenario test. They describe networks that cannot
// exist, so any behaviour observed afterwards is meaningless.

// CorruptDropOne deletes one record, violating monotonicity.
func (s *Store) CorruptDropOne(area record.AreaTag) {
	a := s.areas[area]
	if a == nil {
		return
	}
	for _, origin := range sortedOrigins(a) {
		byOrigin := a.records[origin]
		for _, seq := range sortedSeqs(byOrigin) {
			delete(byOrigin, seq)
			return
		}
	}
}

// CorruptReplaceOne swaps the occupant of the first slot for a different
// record, violating immutability while leaving the record COUNT unchanged —
// which is precisely why a count-based check cannot see it.
func (s *Store) CorruptReplaceOne(area record.AreaTag, with *record.Record) {
	a := s.areas[area]
	if a == nil || with == nil {
		return
	}
	for _, origin := range sortedOrigins(a) {
		byOrigin := a.records[origin]
		for _, seq := range sortedSeqs(byOrigin) {
			byOrigin[seq] = with
			return
		}
	}
}

// CorruptTamperOne mutates a stored record's body in place, so its signature no
// longer covers its contents. Apply's admission check cannot see this; only
// re-verification of what is already held can.
func (s *Store) CorruptTamperOne(area record.AreaTag) {
	a := s.areas[area]
	if a == nil {
		return
	}
	for _, origin := range sortedOrigins(a) {
		byOrigin := a.records[origin]
		for _, seq := range sortedSeqs(byOrigin) {
			rec := byOrigin[seq]
			if len(rec.Body) == 0 {
				continue
			}
			rec.Body[0] ^= 0xFF
			return
		}
	}
}
