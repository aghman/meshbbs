// Package profile decides WHEN a user becomes visible to the network (§6.7).
//
// # The arithmetic that forces this
//
// A PROFILE record is what makes a user DM-addressable network-wide, and at
// roughly 100 bytes it looks cheap. It is not, because the budget it spends is
// airtime and airtime is the binding constraint (§1.1):
//
//	 10 local users →  1.0 KB → 0.4 days of that node's ENTIRE mesh allocation
//	 50 local users →  5.0 KB → 2.0 days
//	200 local users → 20.0 KB → 7.9 days
//
// Fifty users would spend two full days of a node's total allocation just
// announcing that they exist, before anybody posts anything. Across fifty
// instances that is ~244 KB of pure directory data. Eager publication is not
// slightly wasteful here; it is the difference between a network that carries
// conversation and one that carries only introductions.
//
// # The rule
//
// A profile is published when, and only when, the user first does something
// that REQUIRES the network to know them: posts to a federated area, or sends a
// DM off-node. Registering publishes nothing. Reading publishes nothing. Posting
// to a local-only area publishes nothing.
//
// The consequence worth stating plainly: a user who only ever reads is invisible
// to the rest of the network, permanently and by design. That is not a
// limitation to apologise for — it is what makes a fifty-user node affordable.
package profile

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aghman/meshbbs/internal/record"
)

// Trigger is a reason a profile might need publishing.
type Trigger uint8

const (
	// TriggerNone is the zero value: no reason to publish.
	TriggerNone Trigger = iota
	// TriggerFederatedPost is a post to an area that replicates off-node.
	TriggerFederatedPost
	// TriggerOffNodeDM is a DM addressed to a user on another instance.
	TriggerOffNodeDM
	// TriggerDirectoryRequest is a peer explicitly asking for this user.
	//
	// §6.7 makes directory backfill PULL, not push: nobody broadcasts their
	// whole roster. A specific request is a legitimate trigger because the
	// airtime is being spent on something a peer actually asked for.
	TriggerDirectoryRequest
	// TriggerFlagsChanged is a user becoming unlisted after having been listed.
	TriggerFlagsChanged
)

func (t Trigger) String() string {
	switch t {
	case TriggerNone:
		return "none"
	case TriggerFederatedPost:
		return "posted to a federated area"
	case TriggerOffNodeDM:
		return "sent an off-node DM"
	case TriggerDirectoryRequest:
		return "a peer requested the directory entry"
	case TriggerFlagsChanged:
		return "listing preference changed"
	}
	return fmt.Sprintf("Trigger(%d)", uint8(t))
}

// ErrNoDMKey is returned when a user has no X25519 key to publish.
var ErrNoDMKey = errors.New("user has no DM key, so no profile can be published")

// User is the local account state the policy needs.
type User struct {
	Nick  string
	DMKey [record.X25519KeyLen]byte
	// Unlisted is the per-user directory opt-out `[N9]`. Users are listed by
	// default: a BBS user list has always been public, and discoverability is
	// most of the point of a network directory.
	Unlisted bool
}

// Decision is what the policy concluded, and why.
//
// It carries the reasoning because "why did this user's profile go out" is a
// question a sysop will eventually ask about their airtime bill, and because
// the negative cases are the ones that save the budget.
type Decision struct {
	Publish bool
	Trigger Trigger
	Reason  string
	// Bytes is the on-wire cost if published, for the audit log.
	Bytes int
}

// Policy tracks which local users have been published.
//
// Single-threaded by contract, like the rest of the sync path (§12.1).
type Policy struct {
	// published maps nick to the flags last published for that user. Presence
	// means the network has been told; the value catches a later change.
	published map[string]record.ProfileFlags
	// suppressed counts publications avoided, which is the number that
	// justifies this package existing.
	suppressed int
	// emitted counts publications made.
	emitted int
}

// New returns an empty policy: nobody has been published yet.
func New() *Policy { return &Policy{published: map[string]record.ProfileFlags{}} }

// Consider decides whether a trigger requires publishing a profile.
func (p *Policy) Consider(u User, t Trigger) Decision {
	cost := record.ProfileSize(u.Nick)

	if u.DMKey == ([record.X25519KeyLen]byte{}) {
		p.suppressed++
		return Decision{Reason: ErrNoDMKey.Error(), Trigger: t, Bytes: cost}
	}
	if t == TriggerNone {
		p.suppressed++
		return Decision{Reason: "no triggering activity; registering and reading publish nothing",
			Trigger: t, Bytes: cost}
	}

	wantFlags := record.ProfileFlags(0)
	if u.Unlisted {
		wantFlags |= record.FlagUnlisted
	}

	prev, already := p.published[u.Nick]

	// An unlisted user who has never been published stays invisible. This is the
	// opt-out working as designed: they can still send off-node DMs, and a reply
	// still reaches them because the sender's DM carries what is needed (§6.7).
	if u.Unlisted && !already {
		p.suppressed++
		return Decision{
			Reason:  "user is unlisted and was never published, so there is nothing to say",
			Trigger: t, Bytes: cost,
		}
	}

	// A user who WAS published and has now opted out must be published once
	// more, carrying the unlisted flag. A tombstone would be wrong here: it
	// would destroy the key material peers need to deliver replies, turning an
	// opt-out of the directory into an opt-out of receiving mail.
	if already && prev != wantFlags {
		p.published[u.Nick] = wantFlags
		p.emitted++
		return Decision{
			Publish: true, Trigger: TriggerFlagsChanged, Bytes: cost,
			Reason: "listing preference changed; peers holding the old profile must be told",
		}
	}

	if already {
		p.suppressed++
		return Decision{
			Reason:  "already published and unchanged",
			Trigger: t, Bytes: cost,
		}
	}

	p.published[u.Nick] = wantFlags
	p.emitted++
	return Decision{
		Publish: true, Trigger: t, Bytes: cost,
		Reason: "first activity requiring the network to know this user: " + t.String(),
	}
}

// Published reports whether a nick has been announced to the network.
func (p *Policy) Published(nick string) bool {
	_, ok := p.published[nick]
	return ok
}

// Forget removes a user, for account deletion.
//
// §6.7: deletion emits a TOMBSTONE for the profile, but only if one was ever
// published. A local-only account that never triggered publication needs no
// tombstone — and emitting one would be worse than pointless, since it would
// announce to the whole network the existence of a user it had never heard of.
func (p *Policy) Forget(nick string) (needsTombstone bool) {
	_, was := p.published[nick]
	delete(p.published, nick)
	return was
}

// Stats reports publications made and avoided.
func (p *Policy) Stats() (emitted, suppressed int) { return p.emitted, p.suppressed }

// PublishedNicks lists announced users, sorted.
func (p *Policy) PublishedNicks() []string {
	out := make([]string, 0, len(p.published))
	for n := range p.published {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Directory is the network-wide user directory assembled from PROFILE records.
type Directory struct {
	entries map[string]Entry
}

// Entry is one user known to the network.
type Entry struct {
	Nick  string
	Node  record.ID // the record that announced them, for provenance
	Body  record.ProfileBody
	Owner string // "nick@node", the only globally meaningful form (§6.1.4)
}

// NewDirectory returns an empty directory.
func NewDirectory() *Directory { return &Directory{entries: map[string]Entry{}} }

// Add records a verified profile. The caller must have verified the record
// against its origin node's key first; this function assumes that was done.
func (d *Directory) Add(rec *record.Record, body record.ProfileBody) {
	qualified := body.Nick + "@" + rec.Origin.Compact()
	if body.Unlisted() {
		// Respect the opt-out by dropping the entry, while the caller keeps the
		// record itself — the DM key is still needed to deliver replies.
		delete(d.entries, qualified)
		return
	}
	d.entries[qualified] = Entry{
		Nick: body.Nick, Node: rec.ID(), Body: body, Owner: qualified,
	}
}

// Lookup finds a user by their qualified "nick@node" address.
func (d *Directory) Lookup(qualified string) (Entry, bool) {
	e, ok := d.entries[qualified]
	return e, ok
}

// Search returns entries whose nick contains the query, sorted.
//
// Case-insensitive because nicks are for humans; qualified by node in the
// result because a bare nick is only unique within one instance (§6.1.4).
func (d *Directory) Search(q string) []Entry {
	q = strings.ToLower(q)
	var out []Entry
	for _, e := range d.entries {
		if strings.Contains(strings.ToLower(e.Nick), q) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Owner < out[j].Owner })
	return out
}

// Len is the number of listed users.
func (d *Directory) Len() int { return len(d.entries) }
