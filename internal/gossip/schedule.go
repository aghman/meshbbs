package gossip

import (
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/rng"
)

// ScheduleConfig governs how often a node broadcasts a standalone digest.
type ScheduleConfig struct {
	// Base is the digest interval for a small mesh, before scaling.
	Base time.Duration

	// PeerDivisor is the N in "interval = Base × max(1, peers/N)" from §7.3.
	PeerDivisor int

	// ControlShare is the fraction of total channel airtime that standalone
	// digests from the WHOLE mesh may consume. §7.3 defaults it to 1%, which
	// is a fifth of our 5% allocation (§1.1).
	ControlShare float64

	// AirtimePerByte and FloodMultiplier price a digest. The multiplier is
	// essential: what the commons pays is our transmission times R, and
	// budgeting local TX alone understates the cost by that factor (§1.1).
	AirtimePerByte  time.Duration
	FloodMultiplier float64

	// JitterFraction spreads transmissions so peers do not synchronise. Two
	// nodes that boot together would otherwise collide every cycle forever.
	JitterFraction float64

	// MaxInterval caps the computed interval. Without it a large mesh pushes
	// anti-entropy out far enough that a returning node waits days for a
	// heartbeat that would have healed it.
	MaxInterval time.Duration
}

// DefaultSchedule returns §7.3's defaults for a LongFast mesh.
func DefaultSchedule() ScheduleConfig {
	return ScheduleConfig{
		Base:            30 * time.Minute,
		PeerDivisor:     5,
		ControlShare:    0.01,
		AirtimePerByte:  8600 * time.Microsecond,
		FloodMultiplier: 4,
		JitterFraction:  0.20,
		MaxInterval:     12 * time.Hour,
	}
}

// DigestAirtime is what one standalone digest of the given size costs the
// channel, flood multiplier included.
func (c ScheduleConfig) DigestAirtime(sizeBytes int) time.Duration {
	return time.Duration(float64(sizeBytes) * float64(c.AirtimePerByte) * c.FloodMultiplier)
}

// Interval computes the standalone digest interval for a mesh of `peers` nodes
// whose digests are `sizeBytes` long.
//
// §7.3 gives two rules and requires both:
//
//	interval = Base × max(1, peers/PeerDivisor)          (the heuristic)
//	interval ≥ peers × DigestAirtime / ControlShare       (the airtime clamp)
//
// We take the larger, so whichever binds, the result is safe.
//
// # The heuristic is decorative, and that is worth knowing
//
// Both rules are linear in peer count. The heuristic gives Base/PeerDivisor per
// peer — 360 s at the defaults — and the clamp gives DigestAirtime/ControlShare
// per peer, which for a 103-byte digest at R=4 and a 1% share is about 354 s.
// Those constants are so close that the two curves nearly coincide, and small
// changes in digest size decide which one wins.
//
// So the heuristic contributes almost nothing the clamp does not already
// enforce, and only the clamp has physical meaning: it is derived from bytes,
// airtime and the flood multiplier rather than chosen. It is kept because a
// sysop who raises ControlShare should still get some peer scaling, but the
// clamp is the rule that matters and the one to reason about.
//
// # What this produces
//
// At the defaults, with ten areas (103 bytes):
//
//	 5 peers → 30 min      20 peers → 2.0 h
//	50 peers → 4.9 h      100 peers → 9.8 h
//
// §7.3's prose said "around 2–3 hours" at 50 nodes. That was wrong, and the
// same section's own table shows why: 50 nodes at a 120-minute interval
// consume 2.8% of the channel, nearly three times the 1% control budget the
// text sets two paragraphs earlier. 4.9 hours is what 1% actually buys.
//
// A multi-hour heartbeat sounds alarming and is not: anti-entropy is the safety
// net. Content propagates by opportunistic push (§7.3 cycle step 1), digests
// piggyback on any bundle already in flight, and the standalone digest is only
// the idle-node heartbeat. What the interval bounds is how long a node that has
// been silent AND has heard nothing waits before announcing itself.
func (c ScheduleConfig) Interval(peers int, sizeBytes int) time.Duration {
	if peers < 1 {
		peers = 1
	}
	div := c.PeerDivisor
	if div < 1 {
		div = 1
	}

	scale := float64(peers) / float64(div)
	if scale < 1 {
		scale = 1
	}
	heuristic := time.Duration(float64(c.Base) * scale)

	clamp := time.Duration(0)
	if c.ControlShare > 0 {
		perNode := c.DigestAirtime(sizeBytes)
		clamp = time.Duration(float64(peers) * float64(perNode) / c.ControlShare)
	}

	interval := heuristic
	if clamp > interval {
		interval = clamp
	}
	if c.MaxInterval > 0 && interval > c.MaxInterval {
		interval = c.MaxInterval
	}
	return interval
}

// ControlUtilisation reports the fraction of channel airtime that standalone
// digests consume at a given interval, for the sysop status screen and for
// tests that assert the budget is actually respected.
func (c ScheduleConfig) ControlUtilisation(peers, sizeBytes int, interval time.Duration) float64 {
	if interval <= 0 {
		return 1
	}
	total := float64(peers) * float64(c.DigestAirtime(sizeBytes))
	return total / float64(interval)
}

// Scheduler decides when this node should broadcast a standalone digest.
//
// It implements mitigations 2, 3 and 4 together, because they only make sense
// together: the interval sets the rhythm, piggybacking removes most beats, and
// suppression removes the rest once the mesh is converged. On a quiet converged
// mesh the three compose to almost no traffic at all, which is the goal — a
// converged network should be nearly silent.
type Scheduler struct {
	cfg   ScheduleConfig
	clk   clock.Clock
	rnd   rng.Source
	peers func() int

	// last is when we last put a digest on the air by ANY means. Piggybacked
	// digests count: mitigation 3 works precisely because a node with normal
	// traffic has already told everyone what it holds.
	last time.Time
	// due is the next scheduled time, jittered.
	due time.Time
	// heardMatching is set when we heard a peer's digest that agreed with ours
	// everywhere we overlap, since our last transmission.
	heardMatching bool
	// suppressed counts skipped transmissions, for the status screen.
	suppressed int
	// sent counts actual standalone digest transmissions.
	sent int
}

// NewScheduler builds a scheduler. `peers` reports the current known peer
// count, which the interval scales with.
func NewScheduler(cfg ScheduleConfig, clk clock.Clock, rnd rng.Source, peers func() int) *Scheduler {
	s := &Scheduler{cfg: cfg, clk: clk, rnd: rnd, peers: peers, last: clk.Now()}
	s.reschedule(0)
	return s
}

// reschedule sets the next due time with jitter applied.
func (s *Scheduler) reschedule(sizeBytes int) {
	if sizeBytes <= 0 {
		sizeBytes = 103 // ten areas, the design's reference digest
	}
	interval := s.cfg.Interval(s.peers(), sizeBytes)

	// Jitter is symmetric around the interval. Asymmetric jitter would bias the
	// mean interval and quietly break the airtime budget the clamp computed.
	if s.cfg.JitterFraction > 0 {
		span := float64(interval) * s.cfg.JitterFraction
		offset := (s.rnd.Float64()*2 - 1) * span
		interval = time.Duration(float64(interval) + offset)
	}
	if interval < time.Second {
		interval = time.Second
	}
	s.due = s.clk.Now().Add(interval)
}

// NoteTransmitted records that a digest went out, standalone or piggybacked.
//
// Call this whenever a digest reaches the air. Mitigation 3 depends on it: a
// node sending bundles has already announced its state, and a standalone digest
// on top would be pure overhead.
func (s *Scheduler) NoteTransmitted(sizeBytes int) {
	s.last = s.clk.Now()
	s.heardMatching = false
	s.reschedule(sizeBytes)
}

// NoteHeard records a digest received from a peer.
//
// `matches` should be true when the peer's rolling hashes agreed with ours
// across every area we share. That means the peer has just told the mesh
// exactly what we would have said, so our own digest would carry no
// information (§7.3 mitigation 4).
//
// Note it takes a peer that AGREES, not merely any peer. A digest from a peer
// that differs from us is not a substitute for ours — it is the trigger for
// reconciliation, and suppressing on it would let two diverged nodes fall
// silent about their disagreement.
func (s *Scheduler) NoteHeard(matches bool) {
	if matches {
		s.heardMatching = true
	}
}

// Due reports whether a standalone digest should be broadcast now, and why not
// when it should not.
func (s *Scheduler) Due() (bool, string) {
	if s.clk.Now().Before(s.due) {
		return false, "not yet due"
	}
	if s.heardMatching {
		// Someone already said what we would have said. Push the next beat out
		// and stay quiet.
		s.suppressed++
		s.heardMatching = false
		s.reschedule(0)
		return false, "suppressed: a peer already announced matching state"
	}
	return true, ""
}

// MarkSent records a standalone digest transmission.
func (s *Scheduler) MarkSent(sizeBytes int) {
	s.sent++
	s.NoteTransmitted(sizeBytes)
}

// Stats reports scheduler activity for the sysop status screen (§11.6).
func (s *Scheduler) Stats() (sent, suppressed int, next time.Time) {
	return s.sent, s.suppressed, s.due
}
