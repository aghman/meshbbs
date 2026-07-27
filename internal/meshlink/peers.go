package meshlink

import (
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
)

// maxClockSkew bounds how far in the future an announcement may be dated.
//
// The timestamp exists to order announcements, not to date them, so this only
// has to stop one absurd value from pinning a peer's binding forever: a node
// whose clock is set to 2038 would otherwise make every later, honest
// announcement look stale. Meshtastic nodes without GPS or an app connection
// genuinely do have bad clocks, so the tolerance is generous.
const maxClockSkew = 24 * time.Hour

// peerTable maps between node IDs and radio addresses.
//
// # Why rebinding is allowed at all
//
// A node's radio is not part of its identity (§6.1.1) — that is the point of
// deriving the ID from a key. Sysops replace failed hardware, move a node from
// a Heltec to a RAK, or reflash and get a new node number, and none of that
// should look like a new instance to the federation. So a later signed
// announcement replaces an earlier binding.
//
// The timestamp is what stops that being a free redirection primitive: an
// attacker who replays a peer's OLD announcement cannot move it back to a radio
// it has left, because a binding only ever moves forward in the peer's own
// time.
type peerTable struct {
	mu sync.RWMutex
	// byID and byRadio are two views of one binding, kept consistent under mu.
	byID    map[identity.NodeID]binding
	byRadio map[uint32]identity.NodeID
}

type binding struct {
	radio uint32
	at    time.Time
}

func newPeerTable() *peerTable {
	return &peerTable{
		byID:    map[identity.NodeID]binding{},
		byRadio: map[uint32]identity.NodeID{},
	}
}

// learn records a verified announcement.
//
// It returns ErrStaleAnnounce for one that does not advance the peer's own
// clock, so the caller can tell "nothing to do" from "someone is replaying
// frames at us" — the two look identical from the outside and only one is
// worth a log line.
func (t *peerTable) learn(a Announcement, now time.Time) error {
	if a.At.After(now.Add(maxClockSkew)) {
		return ErrStaleAnnounce
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if old, ok := t.byID[a.ID]; ok {
		if !a.At.After(old.at) {
			// Equal timestamps included: a rebroadcast of the announcement we
			// already hold is not new information, and treating it as new would
			// let a replayed frame flap a binding between two radios.
			return ErrStaleAnnounce
		}
		delete(t.byRadio, old.radio)
	}
	// A radio number can be reused by a different node — firmware derives it
	// from hardware, and hardware gets reflashed and handed on. Whoever
	// announced last owns it, and the previous holder becomes unattributed
	// until it announces again.
	if prev, ok := t.byRadio[a.Radio]; ok && prev != a.ID {
		delete(t.byID, prev)
	}

	t.byID[a.ID] = binding{radio: a.Radio, at: a.At}
	t.byRadio[a.Radio] = a.ID
	return nil
}

// radioFor returns the radio address bound to a node ID.
func (t *peerTable) radioFor(id identity.NodeID) (uint32, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	b, ok := t.byID[id]
	return b.radio, ok
}

// idFor returns the node ID bound to a radio address.
func (t *peerTable) idFor(radio uint32) (identity.NodeID, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	id, ok := t.byRadio[radio]
	return id, ok
}

// bindings returns a copy for the status screen, sorted by node ID.
func (t *peerTable) bindings() []Binding {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Binding, 0, len(t.byID))
	for id, b := range t.byID {
		out = append(out, Binding{ID: id, Radio: b.radio, Announced: b.at})
	}
	sortBindings(out)
	return out
}

// Binding is one learned node-ID-to-radio mapping.
type Binding struct {
	ID        identity.NodeID
	Radio     uint32
	Announced time.Time
}

// sortBindings orders by node ID. Insertion sort keeps the output deterministic
// without dragging map iteration order into anything observable (§6.2.1).
func sortBindings(b []Binding) {
	for i := 1; i < len(b); i++ {
		for j := i; j > 0 && less(b[j].ID, b[j-1].ID); j-- {
			b[j], b[j-1] = b[j-1], b[j]
		}
	}
}

func less(a, b identity.NodeID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// askRate limits how often we ask a given radio to identify itself.
//
// An unattributed sender is usually a peer whose announcement we missed, and it
// will keep transmitting — so without a limiter every packet from it would
// provoke a question, turning one missed frame into a conversation that costs
// more airtime than the traffic it is about.
type askRate struct {
	mu    sync.Mutex
	last  map[uint32]time.Time
	every time.Duration
}

func newAskRate(every time.Duration) *askRate {
	return &askRate{last: map[uint32]time.Time{}, every: every}
}

// allow reports whether we may ask this radio now, and records that we did.
func (a *askRate) allow(radio uint32, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if prev, ok := a.last[radio]; ok && now.Sub(prev) < a.every {
		return false
	}
	a.last[radio] = now
	return true
}
