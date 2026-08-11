// Package doorevent decides when a league's queued events become a record
// (design §9.5).
//
// # Why batching is not the door's job
//
// §9.5 says door events must be "small and batched". A door could be asked to
// batch its own, and asking third-party binaries to be careful with a commons
// is asking the wrong party — §11.4 is the general form of that argument, and
// the door API is where it gets enforced rather than requested.
//
// A door also cannot do it. A door process exists only while somebody is
// playing; it reports a kill and exits. There is nobody to hold events for a
// window but the BBS.
//
// # Why this package holds no state and touches nothing
//
// Everything here is a pure function of (what is queued, what time it is). The
// store owns the rows, the federation loop owns the clock tick, and bbs.Service
// owns the record. That split is what lets a test drive a full day of league
// play in microseconds on a virtual clock (§12.1), which matters more here than
// almost anywhere else in the codebase: the windows this computes are measured
// in HOURS, and a test that had to wait one out would never be written.
package doorevent

import (
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/record"
)

// Window bounds, applied to whatever the budget arithmetic produces.
//
// The derived window can be absurd in both directions — an area with a large
// share on a quiet mesh computes minutes, and one with a sliver of a 50-instance
// budget computes days. Neither is useful: below the floor the batching stops
// paying for itself, and above the ceiling events expire before they are ever
// sent, which is worse than sending a short batch.
const (
	MinWindow = 5 * time.Minute
	MaxWindow = 12 * time.Hour
)

// DefaultMaxAge is how long an event may wait before it is dropped unsent.
//
// Generous on purpose. This is policy, not correctness: clocks are advisory
// (§6.2.1) and a league night that straddles a restart should still report. What
// it prevents is a node that has been offline for a week waking up and spending
// its whole budget on a game that finished.
const DefaultMaxAge = 24 * time.Hour

// Event is one queued item, as much of it as the policy needs to see.
type Event struct {
	ID       int64
	QueuedAt time.Time
}

// Group is everything queued for one (area, game), oldest first.
//
// One record is about one game (record.DoorEventBody), so a group is exactly
// the set of events that could share a record.
type Group struct {
	Area, Game string
	Events     []Event
}

// Decision is what to do with a group now.
type Decision struct {
	// Expire are too old to send. They are dropped, counted and logged — never
	// silently discarded, because the door was told they were queued.
	Expire []Event
	// Flush go into one record now, oldest first. Empty means not yet.
	Flush []Event
	// Wait is how long until this group would flush on its window, when
	// nothing is being flushed now. Zero when Flush is non-empty.
	Wait time.Duration
}

// Config configures a Policy.
type Config struct {
	// Window is how long a partial batch waits. Zero means the caller could not
	// derive one, in which case MaxWindow applies — the conservative end,
	// because a window that is too long costs latency and one that is too short
	// costs the airtime batching exists to save.
	Window time.Duration
	// MaxAge is how long an event may wait before expiring. Zero means
	// DefaultMaxAge.
	MaxAge time.Duration
	// BatchMax caps one record. Zero means record.MaxDoorEventsPerRecord.
	BatchMax int
	// Clock is injected per §12.1.
	Clock clock.Clock
}

// Policy decides when to flush. It holds no queue and no connection.
type Policy struct {
	cfg Config
}

// New builds a Policy, filling in defaults.
func New(cfg Config) *Policy {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.BatchMax <= 0 {
		cfg.BatchMax = record.MaxDoorEventsPerRecord
	}
	if cfg.Window <= 0 {
		cfg.Window = MaxWindow
	}
	cfg.Window = ClampWindow(cfg.Window)
	return &Policy{cfg: cfg}
}

// Window reports the window in force, after clamping.
func (p *Policy) Window() time.Duration { return p.cfg.Window }

// MaxAge reports the expiry age in force.
func (p *Policy) MaxAge() time.Duration { return p.cfg.MaxAge }

// ClampWindow holds a derived window inside the useful range.
func ClampWindow(d time.Duration) time.Duration {
	if d < MinWindow {
		return MinWindow
	}
	if d > MaxWindow {
		return MaxWindow
	}
	return d
}

// DeriveWindow computes a batch window from what an area's share actually buys.
//
// # Why this is derived rather than configured
//
// The right window depends on R, which is a guess until somebody measures it
// (`N10`, §7.8). A configured default would have to be chosen at one R and
// rewritten at another — and every sysop who had already set one would keep the
// wrong value. Deriving it from the governor's own arithmetic means the
// measurement propagates with no code change and no second sizing exercise.
//
// The arithmetic: if an area's share buys packetsPerDay full packets, and a
// batch is one packet, then waiting a day divided by that is exactly long
// enough to have the budget to send when the window expires. Waiting less means
// queueing behind the governor; waiting more means latency nobody asked for.
//
// A share buying nothing at all returns MaxWindow rather than infinity: the
// events will expire, and they should expire on the age rule where it is
// visible, not by never being considered.
func DeriveWindow(packetsPerDay float64) time.Duration {
	if packetsPerDay <= 0 {
		return MaxWindow
	}
	return ClampWindow(time.Duration(float64(24*time.Hour) / packetsPerDay))
}

// Consider decides what to do with one group now.
//
// Expiry is applied FIRST, so an expired event can never be counted toward the
// batch that triggers a flush. The other order would let a week-old backlog
// look like a full batch and put stale news on the air at full price.
func (p *Policy) Consider(g Group) Decision {
	now := p.cfg.Clock.Now()
	var d Decision

	fresh := make([]Event, 0, len(g.Events))
	for _, e := range g.Events {
		if now.Sub(e.QueuedAt) >= p.cfg.MaxAge {
			d.Expire = append(d.Expire, e)
			continue
		}
		fresh = append(fresh, e)
	}
	if len(fresh) == 0 {
		return d
	}

	// A full batch goes now. This is the case the whole design is for: at eight
	// events the record is carrying its signature efficiently, and waiting
	// longer buys nothing.
	if len(fresh) >= p.cfg.BatchMax {
		d.Flush = fresh[:p.cfg.BatchMax]
		return d
	}

	// A partial batch waits out the window, measured from the OLDEST event
	// rather than the newest. Measuring from the newest would let a door that
	// reports something every few minutes hold the first event forever.
	age := now.Sub(fresh[0].QueuedAt)
	if age >= p.cfg.Window {
		d.Flush = fresh
		return d
	}
	d.Wait = p.cfg.Window - age
	return d
}
