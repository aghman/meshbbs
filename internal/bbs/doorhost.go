package bbs

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

// DoorHost is the BBS as a door sees it (§9.1.1).
//
// It exists so that the door package can be written and tested without a
// database, and so that the authority a door has is a list somebody can read in
// one place rather than a set of calls scattered through a socket handler. The
// interface it satisfies IS the door API's reach: adding a method here widens
// what every installed door can do, which is the sort of change that should
// look like a change.
//
// # Why level 4 is barely any code
//
// PostAs and SendDMAs call Post and SendDM — the same methods the menu calls,
// with the user's own nick. That is not laziness, it is §9.1.1's intersection
// rule expressed structurally: the capability gates live inside those methods,
// so a door acting as a user is subject to exactly what the user is subject to,
// and there is no second path here that could drift from the first. A version
// of this that reimplemented the checks would be a privilege-escalation bug
// waiting for the two copies to disagree.
type DoorHost struct{ svc *Service }

// Doors returns the BBS as a door host.
func (s *Service) Doors() *DoorHost { return &DoorHost{svc: s} }

var _ door.Host = (*DoorHost)(nil)

// ---------------------------------------------------------------------------
// Level 2 — state
// ---------------------------------------------------------------------------

func (h *DoorHost) StateGet(ctx context.Context, d, scope, owner, key string) (string, bool, error) {
	return h.svc.store.DoorStateGet(ctx, d, scope, owner, key)
}

func (h *DoorHost) StateSet(ctx context.Context, d, scope, owner, key, value string, quota int64) error {
	return h.svc.store.DoorStateSet(ctx, d, scope, owner, key, value, quota)
}

func (h *DoorHost) StateDelete(ctx context.Context, d, scope, owner, key string) error {
	return h.svc.store.DoorStateDelete(ctx, d, scope, owner, key)
}

func (h *DoorHost) StateKeys(ctx context.Context, d, scope, owner string) ([]string, error) {
	return h.svc.store.DoorStateKeys(ctx, d, scope, owner)
}

// ---------------------------------------------------------------------------
// Level 3 — announce
// ---------------------------------------------------------------------------

// AreaIsFederated reports whether posting to an area spends mesh airtime.
func (h *DoorHost) AreaIsFederated(ctx context.Context, area string) (bool, error) {
	a, err := h.svc.store.GetArea(ctx, area)
	if err != nil {
		return false, err
	}
	return a.Federated, nil
}

// Announce posts as the door's own identity, never as the user.
//
// The author is the door's marked name (store.DoorAuthorPrefix), which is what
// makes the post attributable and rate-limitable without being mistakable for
// an account: no nick can be spelled that way (§6.7).
func (h *DoorHost) Announce(ctx context.Context, d, area, subject, text string) (string, error) {
	author := store.DoorAuthor(d)
	if len(author) > store.MaxAnnounceDoorNameLen+len(store.DoorAuthorPrefix) {
		return "", fmt.Errorf("door %s: name is too long to post under", d)
	}
	id, err := h.svc.Post(ctx, author, area, subject, text)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// ---------------------------------------------------------------------------
// Level 4 — act as the user
// ---------------------------------------------------------------------------

func (h *DoorHost) PostAs(ctx context.Context, nick, area, subject, text string) (string, error) {
	id, err := h.svc.Post(ctx, nick, area, subject, text)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (h *DoorHost) SendDMAs(ctx context.Context, nick, to, subject, text string) (string, error) {
	id, err := h.svc.SendDM(ctx, nick, to, subject, text)
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (h *DoorHost) NoticeNeeded(ctx context.Context, d, nick string) (bool, error) {
	return h.svc.store.DoorNoticeNeeded(ctx, d, nick)
}

func (h *DoorHost) Audit(ctx context.Context, actor, action, target, detail string) error {
	return h.svc.store.Audit(ctx, actor, action, target, detail)
}

// ---------------------------------------------------------------------------
// Level 3 + a league grant — inter-BBS door events (§9.5)
// ---------------------------------------------------------------------------

// QueueDoorEvent records one game event for the league's next batch.
//
// Three refusals happen here rather than in the door package, and all three are
// the same shape: the door API knows what a door asked for, and only the BBS
// knows whether it was possible.
//
// The area must be a FEDERATED door league. Not merely federated — a door
// pointed at a forum would put DOOR_EVENT records where readers cannot
// interpret them, and GossipStore.Apply refuses those from peers, so accepting
// them locally would mean originating traffic the network is designed to drop.
//
// The target is resolved through ResolveRecipient, the same call SendDM uses,
// so "bob@pnw" means here exactly what it means there and aliases never travel
// (§6.1.4.1: the wire carries the node ID).
//
// And the event has to encode. Validation runs through the real codec rather
// than a copy of its rules, because a queued event that cannot become a record
// is one the flusher would have to drop later — silently, after this call
// already told the door it was queued.
func (h *DoorHost) QueueDoorEvent(ctx context.Context, ev door.DoorEventRequest) error {
	area, err := h.svc.store.GetAnyArea(ctx, ev.Area)
	if err != nil {
		return fmt.Errorf("%w: %s", door.ErrNotALeague, ev.Area)
	}
	if area.Kind != store.KindDoor {
		return fmt.Errorf("%w: %s is %s", door.ErrNotALeague, area.Name, area.Kind.Describe())
	}
	if !area.Federated {
		// Not an error the door can fix, and worth saying so: a local-only
		// league is a sysop decision, and the door has nowhere else to report.
		return fmt.Errorf("%w: %s is local only, so a league cannot cross boards from it",
			door.ErrNotALeague, area.Name)
	}

	queued := store.QueuedDoorEvent{
		Door: ev.Door, Area: area.Name, Game: ev.Game,
		Kind: ev.Kind, Actor: ev.Actor, Payload: ev.Payload,
	}
	if ev.Target != "" {
		nick, node, err := h.svc.ResolveRecipient(ctx, ev.Target)
		if err != nil {
			return fmt.Errorf("%w: %s", door.ErrUnknownTarget, ev.Target)
		}
		queued.Target, queued.TargetNode = nick, node
	}

	// The codec is the authority on what fits, so ask it rather than restating
	// its bounds. A door that trips this gets a bad_request naming the field.
	body := record.DoorEventBody{
		Game: queued.Game,
		Events: []record.DoorEvent{{
			Kind: queued.Kind, Actor: queued.Actor,
			Target: queued.Target, TargetNode: queued.TargetNode,
			Payload: queued.Payload,
		}},
	}
	if _, err := record.MarshalDoorEventBody(body); err != nil {
		return fmt.Errorf("%w: %s", door.ErrInvalidEvent, err)
	}

	return h.svc.store.QueueDoorEvent(ctx, queued)
}

// PollDoorEvents reads a league back for a door.
//
// The area check is the same one QueueDoorEvent makes, and it is repeated
// rather than shared with it: reading and reporting are separate operations
// that could be granted apart one day, and a check written once and reached
// from two places is the kind of thing that gets moved to the wrong side.
//
// Node IDs are rendered rather than raw. A door gets a string it can print and
// compare, and never has to know how eight bytes become a name (§6.1.4.1 —
// aliases are local, so the ID is what is portable between boards).
func (h *DoorHost) PollDoorEvents(ctx context.Context, p door.DoorEventPoll) (door.DoorEventBatch, error) {
	area, err := h.svc.store.GetAnyArea(ctx, p.Area)
	if err != nil {
		return door.DoorEventBatch{}, fmt.Errorf("%w: %s", door.ErrNotALeague, p.Area)
	}
	if area.Kind != store.KindDoor {
		return door.DoorEventBatch{}, fmt.Errorf("%w: %s is %s",
			door.ErrNotALeague, area.Name, area.Kind.Describe())
	}

	events, cursor, truncated, err := h.svc.store.DoorEventsSince(ctx, area.Tag, p.Game, p.After, 0)
	if err != nil {
		return door.DoorEventBatch{}, err
	}

	out := door.DoorEventBatch{Cursor: cursor, Truncated: truncated}
	for _, ev := range events {
		polled := door.PolledDoorEvent{
			Origin: ev.Origin.Compact(),
			At:     ev.At,
			Kind:   ev.Kind,
			Actor:  ev.Actor,
			Target: ev.Target,
		}
		if ev.Target != "" {
			polled.TargetNode = ev.TargetNode.Compact()
		}
		if len(ev.Payload) > 0 {
			polled.Payload = base64.StdEncoding.EncodeToString(ev.Payload)
		}
		out.Events = append(out.Events, polled)
	}
	return out, nil
}
