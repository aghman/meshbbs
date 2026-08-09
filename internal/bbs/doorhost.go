package bbs

import (
	"context"
	"fmt"

	"github.com/aghman/meshbbs/internal/door"
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
