package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

// GossipStore adapts the SQLite record log to what the sync engine needs
// (design §7.3).
//
// # Why this is a separate type
//
// The engine's Store interface has no context and returns no errors on the read
// path, because it is called from a scheduler that has nothing useful to do with
// either: a version vector that cannot be read is not a condition anti-entropy
// can recover from by trying a different peer. That shape is right for the
// engine and wrong for a database, so the adapter is where the mismatch is
// absorbed — errors become logged-and-empty, and a caller that needs the real
// error uses the Store directly.
//
// The in-memory double in internal/gossiptest implements the same interface for
// the simulator, and is deliberately stricter than this: it rejects unsigned
// records and refuses sequence regressions. Everything it enforces, Apply below
// enforces too, because a double that accepts what production rejects proves
// nothing — and so does the reverse.
type GossipStore struct {
	st  *Store
	ctx context.Context

	// onError reports what the interface cannot return.
	onError func(error)

	mu sync.RWMutex
	// keys caches origin public keys, since every inbound record needs one and
	// they never change for a given ID — the ID is the key's hash (§6.1.1).
	keys map[identity.NodeID][]byte
	// areas caches the federated area list, refreshed by Refresh.
	areas []record.AreaTag
}

// NewGossipStore builds the adapter. The context bounds every query it makes,
// so cancelling it stops the engine's database work along with everything else.
func NewGossipStore(ctx context.Context, st *Store, onError func(error)) (*GossipStore, error) {
	if onError == nil {
		onError = func(error) {}
	}
	g := &GossipStore{st: st, ctx: ctx, onError: onError, keys: map[identity.NodeID][]byte{}}
	if err := g.Refresh(); err != nil {
		return nil, err
	}
	return g, nil
}

// Refresh reloads the federated area list.
//
// Areas are created by a sysop while the BBS is running, so the engine's view
// has to be able to change without a restart — but not on every call, since
// Areas() is on the digest path and runs every beat.
func (g *GossipStore) Refresh() error {
	rows, err := g.st.db.QueryContext(g.ctx,
		`SELECT tag FROM areas WHERE federated = 1 ORDER BY tag`)
	if err != nil {
		return fmt.Errorf("list federated areas: %w", err)
	}
	defer rows.Close()

	var out []record.AreaTag
	for rows.Next() {
		var tag []byte
		if err := rows.Scan(&tag); err != nil {
			return fmt.Errorf("scan area tag: %w", err)
		}
		if len(tag) != 4 {
			continue
		}
		var a record.AreaTag
		copy(a[:], tag)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	g.areas = out
	g.mu.Unlock()
	return nil
}

// RosterArea is the area NODE and SUCCESSION records live in.
//
// It is the zero tag because those records carry no area of their own: they are
// about the network rather than about a conversation in it. It is ALWAYS
// federated, whatever the sysop has configured, and that is not an oversight —
// it is the bootstrap. §6.1.2 makes the roster idempotent, replayable and safe
// to serve to anyone, because it is nothing but public keys; without it in
// sync, a peer's first post arrives from an origin nobody can verify and is
// quarantined forever.
var RosterArea = record.AreaTag{}

// Areas implements gossip.Store.
//
// The roster is always included, ahead of the sysop's own areas.
func (g *GossipStore) Areas() []record.AreaTag {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]record.AreaTag, 0, len(g.areas)+1)
	out = append(out, RosterArea)
	return append(out, g.areas...)
}

// Vector implements gossip.Store, returning the CONTIGUOUS high-water mark per
// origin.
//
// Contiguous, not maximum: holding sequences 1, 2 and 4 puts the entry at 2, not
// 4. A vector that claimed 4 would tell peers we have record 3 when we do not,
// and anti-entropy would never ask for it again — the gap would be permanent
// and silent, which is the worst failure mode this protocol has.
func (g *GossipStore) Vector(area record.AreaTag) *vv.Vector {
	v := vv.New()
	rows, err := g.st.db.QueryContext(g.ctx,
		`SELECT origin, seq FROM records WHERE area = ? ORDER BY origin, seq`, area[:])
	if err != nil {
		g.onError(fmt.Errorf("read version vector for area %s: %w", area, err))
		return v
	}
	defer rows.Close()

	var cur identity.NodeID
	var have bool
	var contiguous uint64
	flush := func() {
		if have && contiguous > 0 {
			v.Set(cur, contiguous)
		}
	}

	for rows.Next() {
		var originBytes []byte
		var seq uint64
		if err := rows.Scan(&originBytes, &seq); err != nil {
			g.onError(fmt.Errorf("scan version vector row: %w", err))
			return v
		}
		if len(originBytes) != identity.NodeIDLen {
			continue
		}
		var origin identity.NodeID
		copy(origin[:], originBytes)

		if !have || origin != cur {
			flush()
			cur, have, contiguous = origin, true, 0
		}
		// Sequences start at 1 and arrive in order from the query, so the run
		// continues only while each is exactly one past the last.
		if seq == contiguous+1 {
			contiguous = seq
		}
	}
	flush()
	if err := rows.Err(); err != nil {
		g.onError(fmt.Errorf("iterate version vector: %w", err))
	}
	return v
}

// maxRecordsPerRange bounds one Records call.
//
// The engine asks for what a peer is missing, which after a long partition can
// be thousands of records — far more than the airtime to send them exists for.
// Returning a slice is explicitly allowed to be short (the interface says so),
// and the next digest cycle asks for the rest.
const maxRecordsPerRange = 256

// Records implements gossip.Store.
func (g *GossipStore) Records(area record.AreaTag, r vv.Range) []*record.Record {
	origin := r.Origin
	rows, err := g.st.db.QueryContext(g.ctx,
		`SELECT signed, sig FROM records
		 WHERE area = ? AND origin = ? AND seq >= ? AND seq <= ?
		 ORDER BY seq LIMIT ?`,
		area[:], origin[:], r.From, r.To, maxRecordsPerRange)
	if err != nil {
		g.onError(fmt.Errorf("read records for %s: %w", area, err))
		return nil
	}
	defer rows.Close()

	var out []*record.Record
	for rows.Next() {
		var signed, sig []byte
		if err := rows.Scan(&signed, &sig); err != nil {
			g.onError(fmt.Errorf("scan record: %w", err))
			return out
		}
		// Rebuilt from the EXACT signed bytes, never re-encoded from parsed
		// fields (§6.2.1 rule 1). An encoder change must not be able to
		// invalidate history we already hold and have already vouched for.
		rec, err := record.Unmarshal(append(signed, sig...))
		if err != nil {
			g.onError(fmt.Errorf("decode stored record: %w", err))
			continue
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		g.onError(fmt.Errorf("iterate records: %w", err))
	}
	return out
}

// Apply implements gossip.Store: verify, store, and report how many were new.
//
// Verification happens HERE rather than at a boundary further out, because this
// is the last point before a record enters the log and every path into the log
// goes through it. A record whose origin we hold no key for is quarantined by
// rejection: §6.1.2 says the NODE record may be minutes behind on a lossy mesh,
// and the peer will resend.
func (g *GossipStore) Apply(area record.AreaTag, recs []*record.Record) (int, error) {
	added := 0
	for _, r := range recs {
		if r.Area != area {
			// A bundle claims one area in its header; a record inside it
			// claiming another is either a bug or an attempt to write into an
			// area the sender is not federating.
			g.onError(fmt.Errorf("record %s claims area %s inside a %s bundle", r.ID(), r.Area, area))
			continue
		}
		// A NODE record is verified WITHOUT a prior key, because it carries the
		// key and the origin ID is that key's hash (§6.1.1, §6.1.2). This is
		// the bootstrap: it is how a peer we have never heard of becomes a peer
		// whose posts we can check. Anything else needs the roster first.
		if r.Type == record.TypeNode {
			if _, err := record.VerifyNodeRecord(r); err != nil {
				g.onError(fmt.Errorf("NODE record from %s failed verification: %w",
					r.Origin.Short(), err))
				continue
			}
			if err := g.st.PutNodeFromRecord(g.ctx, r); err != nil {
				g.onError(fmt.Errorf("storing NODE record from %s: %w", r.Origin.Short(), err))
				continue
			}
			g.forget(r.Origin)
		} else {
			pub, err := g.keyFor(r.Origin)
			if err != nil {
				g.onError(fmt.Errorf("no key for origin %s yet: %w", r.Origin.Short(), err))
				continue
			}
			if err := r.Verify(pub); err != nil {
				g.onError(fmt.Errorf("record %s from %s failed verification: %w",
					r.ID(), r.Origin.Short(), err))
				continue
			}
		}

		// Ask before writing, because PutRecord is idempotent and cannot tell
		// us whether it inserted. The engine's "how many were new" answer
		// drives whether a peer is told anything changed, so counting
		// duplicates as new would keep two converged nodes talking forever —
		// on a mesh where §7.3 says a digest costs a second of channel time.
		held, err := g.st.HasRecord(g.ctx, r.ID())
		if err != nil {
			return added, fmt.Errorf("checking for record %s: %w", r.ID(), err)
		}
		if err := g.st.PutRecord(g.ctx, r); err != nil {
			if errors.Is(err, ErrSeqConflict) {
				// Two different records at one (origin, seq) is divergence, not
				// duplication — §6.2.1 rule 3's unrecoverable case. Refuse it
				// and keep going: one bad coordinate must not stop the rest of
				// a bundle from landing.
				g.onError(fmt.Errorf("refusing divergent record from %s: %w", r.Origin.Short(), err))
				continue
			}
			return added, fmt.Errorf("storing record %s: %w", r.ID(), err)
		}
		if !held {
			added++
		}
	}
	return added, nil
}

// keyFor looks up an origin's public key, caching it.
func (g *GossipStore) keyFor(id identity.NodeID) ([]byte, error) {
	g.mu.RLock()
	pub, ok := g.keys[id]
	g.mu.RUnlock()
	if ok {
		return pub, nil
	}

	n, err := g.st.GetNode(g.ctx, id)
	if err != nil {
		return nil, err
	}
	// The key is only usable if it hashes to the ID it claims — the property
	// §6.1.1 calls self-certifying. Checking here means a corrupted row cannot
	// authorise a forged record.
	if !id.Matches(n.PublicKey) {
		return nil, fmt.Errorf("stored key for %s does not hash to its ID", id.Short())
	}

	g.mu.Lock()
	g.keys[id] = n.PublicKey
	g.mu.Unlock()
	return n.PublicKey, nil
}

// forget drops a cached key, so a SUCCESSION or a re-published NODE record
// takes effect rather than being masked by what we looked up first (§6.1.6).
func (g *GossipStore) forget(id identity.NodeID) {
	g.mu.Lock()
	delete(g.keys, id)
	g.mu.Unlock()
}

// SortAreas orders tags deterministically, for callers building digests.
func SortAreas(areas []record.AreaTag) {
	sort.Slice(areas, func(i, j int) bool {
		for b := 0; b < 4; b++ {
			if areas[i][b] != areas[j][b] {
				return areas[i][b] < areas[j][b]
			}
		}
		return false
	})
}
