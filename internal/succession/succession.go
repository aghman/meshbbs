// Package succession tracks node key rotation (design §6.1.6).
//
// # The problem
//
// The node ID *is* the key: ID = BLAKE3(pubkey)[:8]. Rotating a key therefore
// means becoming a different node, and without a way to say "that was me", every
// rotation silently orphans a node's history and every peer relationship it had.
// A SUCCESSION record, signed by the OLD key, is that statement.
//
// # Why auto-follow needs guardrails
//
// `[N2]` decided peers follow a succession WITHOUT asking the sysop first. That
// is the right call for usability — a blocking prompt on every peer in the
// network turns a routine key rotation into a multi-day outage — but it means
// **whoever holds the old key can redirect every peer in the network**. So the
// decision comes with four guardrails, and this package is where they live:
//
//  1. Always alert, never silent. Following still produces an Event for the
//     audit log and a sysop notification. Auto-follow removes the blocking
//     prompt, not the disclosure.
//  2. The old ID is tombstoned at `effective`. Later records from it are
//     rejected; earlier ones stay valid, so a lossy mesh does not lose the tail
//     of the predecessor's log while the succession propagates.
//  3. One succession per predecessor. A second one signed by the same old key
//     is refused and alerted, so an attacker holding a stolen key gets one
//     visible, logged redirect rather than an untraceable game of
//     pass-the-parcel.
//  4. Sanity window on `effective`. Far-future or far-past values are refused,
//     which stops a pre-dated succession being minted now and held for later.
//
// # A lost key has no recovery path
//
// With no old key there is nothing to sign a SUCCESSION with, and this package
// offers no way around that. The honest answer is "you are a new node;
// re-establish with your peers out of band" — which, because peer setup is just
// an ID exchange (§8.4), is a handful of messages. That is the price of having
// no registry, and it is also the reason there is no registry to petition.
package succession

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

var (
	// ErrAlreadySucceeded is guardrail 3: a predecessor gets one succession.
	ErrAlreadySucceeded = errors.New("this node has already been succeeded")
	// ErrOutsideWindow is guardrail 4.
	ErrOutsideWindow = errors.New("succession effective time is outside the accepted window")
	// ErrWouldCycle is returned when following would create a loop.
	ErrWouldCycle = errors.New("succession would create a cycle")
	// ErrChainTooLong is returned when a chain exceeds MaxChain.
	ErrChainTooLong = errors.New("succession chain is too long")
	// ErrUnknownPredecessor is returned when we hold no key for the origin.
	ErrUnknownPredecessor = errors.New("no known public key for the succession's origin")
)

// DefaultWindow is how far from now `effective` may sit, in either direction.
//
// §6.1.6 asks for "a short interval" without fixing one. A day in each
// direction is the useful compromise: generous enough that a sysop scheduling a
// rotation for tomorrow morning, on hardware whose clock may be hours off
// (§6.2's "clocks are advisory" — an off-grid Pi with no RTC boots believing it
// is 1970), is not refused; tight enough that a pre-dated succession cannot be
// minted now and held for a year.
//
// Note what this does NOT defend against: an attacker with the old key can
// simply publish immediately. The window is against stockpiling, not theft.
const DefaultWindow = 24 * time.Hour

// MaxChain bounds how many hops a succession chain may have.
//
// Chains are legitimate — A→B→C is just a node that rotated twice, each hop
// signed by the then-current key — but resolution walks them, so an unbounded
// chain is an unbounded walk driven by remote input.
const MaxChain = 8

// Kind classifies what happened when a record was offered.
type Kind uint8

const (
	// Followed means the succession was accepted and peers should migrate.
	Followed Kind = iota
	// Refused means a guardrail rejected it.
	Refused
	// Duplicate means we had already recorded exactly this succession.
	Duplicate
)

func (k Kind) String() string {
	switch k {
	case Followed:
		return "followed"
	case Refused:
		return "refused"
	case Duplicate:
		return "duplicate"
	}
	return fmt.Sprintf("Kind(%d)", uint8(k))
}

// Event is what the sysop must be told. Guardrail 1 makes this mandatory
// rather than optional: auto-follow removes the prompt, not the disclosure.
type Event struct {
	Kind Kind
	// Predecessor and Successor are both named, and callers should render both
	// in BOTH forms (§6.1.4.2) — the base32 compact form and the six-word form.
	// A sysop reading an alert about an identity handover needs the rendering
	// they can actually check against something written down.
	Predecessor identity.NodeID
	Successor   identity.NodeID
	Effective   time.Time
	At          time.Time
	// Reason explains a refusal, and is empty when Kind is Followed.
	Reason string
}

// Message renders the event for an audit log or sysop alert.
func (e Event) Message() string {
	switch e.Kind {
	case Followed:
		return fmt.Sprintf(
			"FOLLOWED node succession: %s (%s) is now %s (%s), effective %s. "+
				"Allowlist, alias, peer config and sync state have been migrated. "+
				"If you did not expect this, the predecessor's key may be compromised.",
			e.Predecessor.Compact(), e.Predecessor.Words(),
			e.Successor.Compact(), e.Successor.Words(),
			e.Effective.UTC().Format(time.RFC3339))
	case Duplicate:
		return fmt.Sprintf("Repeat of a succession already followed: %s -> %s.",
			e.Predecessor.Compact(), e.Successor.Compact())
	default:
		return fmt.Sprintf(
			"REFUSED node succession: %s (%s) claimed to become %s (%s) — %s. "+
				"A refused succession from a peer you trust is worth investigating.",
			e.Predecessor.Compact(), e.Predecessor.Words(),
			e.Successor.Compact(), e.Successor.Words(), e.Reason)
	}
}

// entry is one accepted hop.
type entry struct {
	successor identity.NodeID
	effective time.Time
	pubkey    ed25519.PublicKey
	recordID  record.ID
}

// Registry holds accepted successions and answers questions about them.
//
// Single-threaded by contract, like everything else in the sync path (§12.1).
type Registry struct {
	clk    clock.Clock
	window time.Duration

	// byPredecessor is the accepted hop for each old ID. At most one, which is
	// guardrail 3.
	byPredecessor map[identity.NodeID]entry
	// keys are the public keys we can verify successions against, learned from
	// NODE records.
	keys map[identity.NodeID]ed25519.PublicKey

	events []Event
}

// New builds an empty registry.
func New(clk clock.Clock) *Registry {
	return &Registry{
		clk:           clk,
		window:        DefaultWindow,
		byPredecessor: map[identity.NodeID]entry{},
		keys:          map[identity.NodeID]ed25519.PublicKey{},
	}
}

// SetWindow overrides the sanity window (guardrail 4).
func (r *Registry) SetWindow(d time.Duration) { r.window = d }

// LearnKey registers a node's public key, normally from its NODE record.
//
// A succession cannot be verified without the predecessor's key, so a node we
// have never heard of cannot hand its identity to anyone as far as we are
// concerned. That is deliberate: it means an attacker cannot introduce a
// fictional node and immediately redirect it somewhere useful.
func (r *Registry) LearnKey(id identity.NodeID, pub ed25519.PublicKey) {
	if id.Matches(pub) {
		r.keys[id] = append(ed25519.PublicKey(nil), pub...)
	}
}

// KnownKey returns the public key held for a node.
func (r *Registry) KnownKey(id identity.NodeID) (ed25519.PublicKey, bool) {
	k, ok := r.keys[id]
	return k, ok
}

// Offer presents a SUCCESSION record. It always returns an Event, because
// guardrail 1 requires every outcome to be disclosed — including refusals,
// which are the interesting ones.
func (r *Registry) Offer(rec *record.Record) Event {
	now := r.clk.Now()

	refuse := func(pred, succ identity.NodeID, eff time.Time, why string) Event {
		e := Event{Kind: Refused, Predecessor: pred, Successor: succ,
			Effective: eff, At: now, Reason: why}
		r.events = append(r.events, e)
		return e
	}

	if rec == nil || rec.Type != record.TypeSuccession {
		return refuse(identity.NodeID{}, identity.NodeID{}, time.Time{},
			"record is not a SUCCESSION")
	}

	pub, known := r.keys[rec.Origin]
	if !known {
		// Parse enough to name the successor in the alert, but do not trust it.
		var succ identity.NodeID
		if body, err := record.UnmarshalSuccessionBody(rec.Body); err == nil {
			succ = body.Successor
		}
		return refuse(rec.Origin, succ, time.Time{}, ErrUnknownPredecessor.Error())
	}

	body, err := record.VerifySuccessionRecord(rec, pub)
	if err != nil {
		return refuse(rec.Origin, identity.NodeID{}, time.Time{}, err.Error())
	}
	effective := time.Unix(int64(body.Effective), 0).UTC()

	// GUARDRAIL 3: one succession per predecessor.
	//
	// Checked before the window, so that a stolen key cannot be used to hunt
	// for a moment when a second redirect would pass the window check.
	if existing, seen := r.byPredecessor[rec.Origin]; seen {
		if existing.recordID == rec.ID() {
			e := Event{Kind: Duplicate, Predecessor: rec.Origin,
				Successor: existing.successor, Effective: existing.effective, At: now}
			r.events = append(r.events, e)
			return e
		}
		return refuse(rec.Origin, body.Successor, effective,
			fmt.Sprintf("%v — it already named %s, and a second redirect from one key is "+
				"how a stolen key would be laundered", ErrAlreadySucceeded, existing.successor.Compact()))
	}

	// GUARDRAIL 4: sanity window on `effective`.
	if r.window > 0 {
		if effective.After(now.Add(r.window)) {
			return refuse(rec.Origin, body.Successor, effective,
				fmt.Sprintf("%v: effective %s is more than %s in the future",
					ErrOutsideWindow, effective.Format(time.RFC3339), r.window))
		}
		if effective.Before(now.Add(-r.window)) {
			return refuse(rec.Origin, body.Successor, effective,
				fmt.Sprintf("%v: effective %s is more than %s in the past",
					ErrOutsideWindow, effective.Format(time.RFC3339), r.window))
		}
	}

	// Cycles. A→B→A would make Resolve walk forever, and is never a legitimate
	// rotation: it says an identity was handed away and handed back, which the
	// one-succession-per-predecessor rule already forbids in the other direction.
	if r.reaches(body.Successor, rec.Origin) {
		return refuse(rec.Origin, body.Successor, effective, ErrWouldCycle.Error())
	}
	if r.chainLen(rec.Origin)+1 > MaxChain {
		return refuse(rec.Origin, body.Successor, effective,
			fmt.Sprintf("%v: longer than %d hops", ErrChainTooLong, MaxChain))
	}

	r.byPredecessor[rec.Origin] = entry{
		successor: body.Successor,
		effective: effective,
		pubkey:    append(ed25519.PublicKey(nil), body.NewPublicKey...),
		recordID:  rec.ID(),
	}
	// The successor's key is now known, which is what lets a chain's next hop
	// be verified without a separate NODE record arriving first.
	r.keys[body.Successor] = append(ed25519.PublicKey(nil), body.NewPublicKey...)

	e := Event{Kind: Followed, Predecessor: rec.Origin, Successor: body.Successor,
		Effective: effective, At: now}
	r.events = append(r.events, e)
	return e
}

// reaches reports whether following successions from `from` arrives at `to`.
func (r *Registry) reaches(from, to identity.NodeID) bool {
	cur := from
	for i := 0; i <= MaxChain; i++ {
		if cur == to {
			return true
		}
		e, ok := r.byPredecessor[cur]
		if !ok {
			return false
		}
		cur = e.successor
	}
	return false
}

// chainLen counts hops already recorded ending at id.
func (r *Registry) chainLen(id identity.NodeID) int {
	// Walk backwards is expensive without a reverse index; walk forwards from
	// every root instead. At the scale of [D2] — fifty instances, of which a
	// handful ever rotate — this is trivially cheap and avoids a second index
	// that could disagree with the first.
	best := 0
	for pred := range r.byPredecessor {
		n := 0
		cur := pred
		for n <= MaxChain {
			if cur == id {
				if n > best {
					best = n
				}
				break
			}
			e, ok := r.byPredecessor[cur]
			if !ok {
				break
			}
			cur = e.successor
			n++
		}
	}
	return best
}

// Resolve follows the chain from id to the current identity.
//
// Callers route to, address, and store against the resolved ID. Chains are
// walked rather than flattened on insert so that a later hop arriving out of
// order — normal on a mesh — does not need a rewrite pass over stored state.
func (r *Registry) Resolve(id identity.NodeID) identity.NodeID {
	cur := id
	for i := 0; i < MaxChain; i++ {
		e, ok := r.byPredecessor[cur]
		if !ok {
			return cur
		}
		cur = e.successor
	}
	return cur
}

// Chain returns the full path from id to its current identity, id included.
func (r *Registry) Chain(id identity.NodeID) []identity.NodeID {
	out := []identity.NodeID{id}
	cur := id
	for i := 0; i < MaxChain; i++ {
		e, ok := r.byPredecessor[cur]
		if !ok {
			break
		}
		cur = e.successor
		out = append(out, cur)
	}
	return out
}

// Superseded reports whether id has been succeeded, and by whom.
func (r *Registry) Superseded(id identity.NodeID) (identity.NodeID, bool) {
	e, ok := r.byPredecessor[id]
	if !ok {
		return identity.NodeID{}, false
	}
	return e.successor, true
}

// AcceptsRecord implements GUARDRAIL 2: the old ID is tombstoned at `effective`.
//
// A record from a superseded origin is accepted only if it predates the
// handover. The asymmetry is deliberate and is the whole point: rejecting
// everything from the old ID the moment a succession lands would discard the
// tail of the predecessor's log, which on a mesh is still in flight for minutes
// or hours after the succession itself arrives. Accepting everything forever
// would mean a rotated-away key still speaks for the node.
func (r *Registry) AcceptsRecord(origin identity.NodeID, ts uint32) (bool, string) {
	e, superseded := r.byPredecessor[origin]
	if !superseded {
		return true, ""
	}
	recTS := time.Unix(int64(ts), 0).UTC()
	if recTS.After(e.effective) {
		return false, fmt.Sprintf(
			"origin %s was succeeded by %s effective %s; this record is stamped %s, after the handover",
			origin.Compact(), e.successor.Compact(),
			e.effective.Format(time.RFC3339), recTS.Format(time.RFC3339))
	}
	return true, ""
}

// Events returns every recorded outcome, oldest first. Guardrail 1's disclosure
// is only real if someone can read it: this feeds the audit log (§11.6) and the
// sysop status screen.
func (r *Registry) Events() []Event { return append([]Event(nil), r.events...) }

// Predecessors lists every superseded ID, sorted, for the status screen.
func (r *Registry) Predecessors() []identity.NodeID {
	out := make([]identity.NodeID, 0, len(r.byPredecessor))
	for id := range r.byPredecessor {
		out = append(out, id)
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
