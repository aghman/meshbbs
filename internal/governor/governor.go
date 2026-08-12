// Package governor decides what a node may transmit (design §7.6).
//
// §7.6 calls this "the most important piece of civic infrastructure in the
// system", and the reason is §1.1: a mesh is a shared medium with no admission
// control, so the only thing stopping fifty BBS instances from making a
// community's mesh unusable is fifty instances each choosing not to. There is
// no authority to appeal to and no back-pressure signal except other people's
// traffic failing.
//
// # What is being budgeted
//
// Not bytes, and not our own transmit time. The unit is CHANNEL time: what one
// packet costs everybody, which is its airtime multiplied by the flood
// multiplier R (§1.1). A node that budgets its own transmissions is measuring
// what it costs itself and ignoring the R−1 rebroadcasts its neighbours make on
// its behalf.
//
// Bytes fail for a second reason the design states backwards. §7.6 says airtime
// is "superlinear in payload"; measurement says the opposite (Appendix A's own
// numbers), and the direction matters here. Airtime is affine — a large fixed
// cost plus a linear term — so a byte budget wildly under-prices SMALL packets:
// 233 bytes in one packet costs 2.16 s, and one byte at a time costs 101 s for
// the same data. Everything below is therefore charged PER PACKET.
package governor

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
)

// Class is a traffic priority (§7.6). Under backpressure the governor drops
// from the bottom.
type Class uint8

const (
	// ClassControl is digests, delta requests, NODE and SUCCESSION records.
	// Starving it does not save airtime, it wastes it: peers that cannot
	// reconcile ask again.
	ClassControl Class = iota
	// ClassDM is private mail, which a user is waiting for.
	ClassDM
	// ClassForum is public posts.
	ClassForum
	// ClassFileCatalog is file metadata. There is no class for file CONTENT
	// because `[D8]` puts no files on the mesh at all.
	ClassFileCatalog
	// ClassDoorEvent is inter-BBS door league traffic (§9.5).
	//
	// # Why below the file catalog, when §1.1 says "alongside forum posts"
	//
	// §1.1 wrote that before §7.6's ladder had a bottom rung, and §7.6's own
	// list stops at the file catalog. Putting door events below it is a
	// refinement of both rather than a contradiction, and the reason is worth
	// stating because it decides what a congested mesh drops first.
	//
	// A catalog entry that arrives a week late is still TRUE: the file is still
	// there, the hash still matches, and a user searching next month finds it.
	// A battle report that arrives a week late is noise — the game has moved
	// on, nobody is waiting for it, and it competes with a person's post for
	// the same second of channel time. Between a person's words and a game's
	// telemetry, the person wins.
	//
	// This also makes the feature safe to ship before `N10` is measured. A
	// league that turns out to be too chatty for the mesh degrades by losing
	// its own traffic first, rather than by pushing somebody's mail out.
	ClassDoorEvent
)

func (c Class) String() string {
	switch c {
	case ClassControl:
		return "control"
	case ClassDM:
		return "dm"
	case ClassForum:
		return "forum"
	case ClassFileCatalog:
		return "file-catalog"
	case ClassDoorEvent:
		return "door-event"
	}
	return "unknown"
}

// Charge is what one transmission is billed to.
//
// Class alone was enough while the only question was priority. A per-area
// sub-budget needs to know WHICH area as well, and the two travel together
// because the class is derived from the area — passing them separately invites
// a caller to compute one from a tag and pass the other from somewhere else.
//
// Area is the raw four-byte tag rather than record.AreaTag so this package need
// not import record. The dependency would be acyclic, but link.go already
// names a constant rather than importing the layer above it, and this matches.
type Charge struct {
	Class Class
	// Area is the tag the traffic belongs to. The zero tag is the roster,
	// which is a real area and simply has no sub-budget configured.
	Area [4]byte
}

// reserve is the fraction of the bucket a class must leave untouched.
//
// This is how "drop from the bottom" is implemented: as the bucket drains,
// classes fall out of contention in reverse priority order, so the last of the
// budget is always available to the traffic that keeps the federation
// converging rather than to a file catalog.
var reserve = map[Class]float64{
	ClassControl:     0.0,
	ClassDM:          0.10,
	ClassForum:       0.35,
	ClassFileCatalog: 0.60,
	ClassDoorEvent:   0.70,
}

// Ceiling limits, as a percentage of total channel time.
const (
	// DefaultCeilingPercent is what the whole BBS network should consume.
	DefaultCeilingPercent = 5.0
	// MaxCeilingPercent is enforced in code, not merely documented. A sysop
	// who edits the config to 60% is not making a decision about their own
	// instance; they are making one about everybody else's mesh.
	MaxCeilingPercent = 15.0
)

// DefaultFloodMultiplier is the guess the design runs on until someone measures
// it (`N10`, §7.8). Every figure here scales linearly with it.
const DefaultFloodMultiplier = 4.0

// DefaultInstanceCount is the network size `[D2]` plans for.
const DefaultInstanceCount = 50

// Telemetry thresholds from §7.6.
const (
	// backoffThreshold is where we start yielding to other traffic.
	backoffThreshold = 25.0
	// silenceThreshold is where we stop originating anything but DMs.
	silenceThreshold = 40.0
	// maxPenalty caps the exponential backoff, so recovery is not
	// indefinitely delayed once the channel clears.
	maxPenalty = 32.0
)

// Window is a span of local wall-clock time, for quiet hours.
type Window struct {
	// Start and End are durations since local midnight. A window that ends
	// before it starts wraps midnight, which is what "22:00 to 06:00" means.
	Start, End time.Duration
}

// Contains reports whether t falls inside the window, in t's own location.
func (w Window) Contains(t time.Time) bool {
	since := time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second
	if w.Start <= w.End {
		return since >= w.Start && since < w.End
	}
	return since >= w.Start || since < w.End
}

// Config configures a governor.
type Config struct {
	// Preset is the radio's modem configuration, for airtime arithmetic.
	Preset airtime.Preset
	// Region is the Meshtastic region name, for duty-cycle rules.
	Region string
	// CeilingPercent is the mesh-wide share for the whole BBS network.
	CeilingPercent float64
	// Instances is how many BBS instances divide that ceiling. §7.6 has the
	// node learn this from the NODE roster; SetInstances updates it as the
	// roster grows.
	Instances int
	// FloodMultiplier is R. Live estimation refines it; a survey sets the
	// starting value (§7.8).
	FloodMultiplier float64
	// FloodMultiplierOverride pins R for testing, disabling live estimation.
	FloodMultiplierOverride bool
	// QuietHours are windows of zero transmission.
	QuietHours []Window
	// Burst is how much budget may accumulate. Defaults to an hour's worth,
	// floored at three full packets — a bucket that cannot hold one packet can
	// never send one.
	Burst time.Duration
	// InboundQuotaPerHour bounds what one peer may send us before we complain.
	InboundQuotaPerHour int
	// Clock is injected per §12.1.
	Clock clock.Clock
	// OnAlert reports a peer exceeding its inbound quota, which §7.6 requires
	// be surfaced rather than silently dropped.
	OnAlert func(string)
	// AreaShares caps what a single area may spend, as a fraction of this
	// instance's whole share (§6.3's airtime_share). An area with no entry
	// draws from the general pool and is bounded only by its class.
	AreaShares map[[4]byte]float64
}

// dutyCycleLimit is the fraction of a rolling hour a region allows one
// transmitter. Zero means no regulatory limit we need to track.
//
// The firmware enforces these itself; §7.6's point is not to fight it but to
// track it locally so traffic queues instead of being rejected at the radio.
var dutyCycleLimit = map[string]float64{
	"EU_433": 0.10,
	"EU_868": 0.10,
	"UA_433": 0.10,
	"UA_868": 0.10,
}

// Governor is a token bucket denominated in channel-seconds.
type Governor struct {
	cfg Config
	clk clock.Clock

	mu sync.Mutex
	// tokens is available channel time.
	tokens   time.Duration
	capacity time.Duration
	last     time.Time

	// penalty divides the refill rate while the channel is busy.
	penalty float64
	// chanUtil and airUtilTx are the radio's latest self-report.
	chanUtil, airUtilTx float64
	haveTelemetry       bool

	// localTX is our own transmissions in the last hour, for duty cycle.
	localTX []txRecord
	// inbound tracks per-peer receive volume.
	inbound map[identity.NodeID]*inboundRec

	// R estimation.
	sentPackets, echoes int
	r                   float64

	// areaBuckets are the per-area sub-budgets (§6.3). Lazily created, so an
	// area with no configured share costs nothing.
	areaShares  map[[4]byte]float64
	areaBuckets map[[4]byte]*areaBucket

	stats Stats
}

// areaBucket is one area's slice of this instance's budget.
//
// # Why the class reserve is not this
//
// A reserve stops a class spending the LAST x% of the bucket. It says nothing
// about rate, so a chatty area inside a 0.70 reserve can still take 30% of the
// day's budget in an hour and leave the rest of the node nothing until the
// bucket refills. §6.3 asks for a share, which is a rate, and that needs its
// own bucket.
type areaBucket struct {
	share    float64
	tokens   time.Duration
	capacity time.Duration
	last     time.Time
}

type txRecord struct {
	at  time.Time
	air time.Duration
}

type inboundRec struct {
	windowStart time.Time
	bytes       int
	alerted     bool
}

// Stats are counters for the sysop status screen.
type Stats struct {
	Allowed        int
	RefusedBudget  int
	RefusedQuiet   int
	RefusedDuty    int
	RefusedBusy    int
	RefusedArea    int
	InboundAlerts  int
	ChargedChannel time.Duration
}

// New builds a governor.
func New(cfg Config) (*Governor, error) {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
	}
	if cfg.AreaShares == nil {
		cfg.AreaShares = map[[4]byte]float64{}
	}
	if cfg.CeilingPercent <= 0 {
		cfg.CeilingPercent = DefaultCeilingPercent
	}
	if cfg.CeilingPercent > MaxCeilingPercent {
		// Clamped rather than rejected: a misconfigured instance should still
		// run, just politely. Refusing to start would tempt a sysop to remove
		// the governor instead.
		cfg.CeilingPercent = MaxCeilingPercent
	}
	if cfg.Instances < 1 {
		cfg.Instances = DefaultInstanceCount
	}
	if cfg.FloodMultiplier < 1 {
		cfg.FloodMultiplier = DefaultFloodMultiplier
	}
	if cfg.InboundQuotaPerHour <= 0 {
		// Generous by mesh standards: at LongFast a peer cannot physically send
		// this much in an hour without taking most of the channel, so crossing
		// it means something is wrong rather than merely busy.
		cfg.InboundQuotaPerHour = 32 * 1024
	}
	if cfg.Preset.SpreadFactor == 0 {
		return nil, fmt.Errorf("governor: no modem preset configured")
	}

	g := &Governor{
		cfg:     cfg,
		clk:     cfg.Clock,
		penalty: 1,
		r:       cfg.FloodMultiplier,
		inbound: map[identity.NodeID]*inboundRec{},
		last:    cfg.Clock.Now(),

		areaShares:  map[[4]byte]float64{},
		areaBuckets: map[[4]byte]*areaBucket{},
	}
	for a, share := range cfg.AreaShares {
		if share > 0 {
			g.areaShares[a] = share
		}
	}
	g.capacity = g.burst()
	// Start full. A node that has just booted has not spent anything, and
	// starting empty would mean an instance cannot announce itself for hours.
	g.tokens = g.capacity
	return g, nil
}

// burst is how much budget may accumulate.
func (g *Governor) burst() time.Duration {
	if g.cfg.Burst > 0 {
		return g.cfg.Burst
	}
	hourly := time.Duration(g.shareFraction() * float64(time.Hour))
	// A bucket smaller than one full packet can never send one, which at 50
	// instances is the normal case: an hour's accrual is about 3.6 channel
	// seconds against a full packet's 8.6.
	if floor := 3 * g.costLocked(233); hourly < floor {
		return floor
	}
	return hourly
}

// shareFraction is this instance's slice of total channel time.
func (g *Governor) shareFraction() float64 {
	return (g.cfg.CeilingPercent / 100) / float64(g.cfg.Instances)
}

// Would reports whether a packet would be allowed, without charging for it.
//
// The sync engine needs this to decide whether to START work: packing a bundle
// and fountain-coding it only to be refused at the radio wastes the CPU of a
// Raspberry Pi and, worse, makes the decision look like a transmission failure
// rather than a budget one.
func (g *Governor) Would(n int, class Class) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.check(n, class) == nil
}

// Allow reports whether a packet of n application bytes may be sent now, and
// charges the budget when it says yes.
//
// The area-less form: the packet is billed to the node's budget and to no area.
// Used by callers that have no area to name, such as the link's own control
// frames.
func (g *Governor) Allow(n int, class Class) bool {
	return g.AllowCharge(n, Charge{Class: class}, false)
}

// AllowCharge is Allow with an area sub-budget applied.
//
// withArea distinguishes "this traffic belongs to the roster", whose tag is the
// zero value, from "this traffic belongs to no area at all". Without it the two
// are the same four bytes and a sysop who capped the roster would find the cap
// silently applied to the link's own frames as well.
func (g *Governor) AllowCharge(n int, ch Charge, withArea bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.check(n, ch.Class); err != nil {
		switch err {
		case errQuiet:
			g.stats.RefusedQuiet++
		case errBusy:
			g.stats.RefusedBusy++
		case errDuty:
			g.stats.RefusedDuty++
		default:
			g.stats.RefusedBudget++
		}
		return false
	}

	now := g.clk.Now()
	air := g.cfg.Preset.Packet(n)
	cost := time.Duration(float64(air) * g.r)

	// The area's own bucket is checked AFTER the node's and before either is
	// charged. Charging the node for a packet the area cannot afford would
	// spend the commons on a transmission that never happens.
	var b *areaBucket
	if withArea {
		if b = g.bucketFor(ch.Area, now); b != nil && b.tokens < cost {
			g.stats.RefusedArea++
			return false
		}
	}

	g.tokens -= cost
	if b != nil {
		b.tokens -= cost
	}
	g.localTX = append(g.localTX, txRecord{at: now, air: air})
	g.sentPackets++
	g.stats.Allowed++
	g.stats.ChargedChannel += cost
	return true
}

// bucketFor returns the area's sub-budget, refilled, or nil if it has none.
// Caller holds the lock.
func (g *Governor) bucketFor(area [4]byte, now time.Time) *areaBucket {
	share, ok := g.areaShares[area]
	if !ok || share <= 0 {
		return nil
	}
	b, ok := g.areaBuckets[area]
	if !ok {
		// A bucket that cannot hold one full packet can never send one, which
		// is the same trap burst() guards against and a worse one here: a
		// sysop who set a small share would get an area that is not slowed but
		// switched off, with nothing to say so. The floor means shares do not
		// strictly sum, which is the honest trade and is why the sum rule is a
		// warning rather than an invariant.
		capacity := time.Duration(share * float64(g.capacity))
		if floor := g.costLocked(233); capacity < floor {
			capacity = floor
		}
		b = &areaBucket{share: share, tokens: capacity, capacity: capacity, last: now}
		g.areaBuckets[area] = b
		return b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.last = now
		// Same penalty divisor as the node's bucket: a busy channel slows an
		// area for the same reason it slows everything else.
		rate := g.shareFraction() * b.share / g.penalty
		b.tokens += time.Duration(float64(elapsed) * rate)
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
	}
	return b
}

// SetAreaShares replaces the per-area caps.
//
// Areas are created and re-shared by a sysop while the BBS is running, so this
// is refreshed from the federation loop alongside the area list rather than
// read once at startup. Buckets for areas whose share is unchanged keep their
// tokens: re-reading the config must not hand an area a fresh full bucket, or a
// sysop could lift the cap by touching the database in a loop.
func (g *Governor) SetAreaShares(shares map[[4]byte]float64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.areaShares = map[[4]byte]float64{}
	for a, s := range shares {
		if s > 0 {
			g.areaShares[a] = s
		}
	}
	for a, b := range g.areaBuckets {
		share, ok := g.areaShares[a]
		if !ok {
			delete(g.areaBuckets, a)
			continue
		}
		if share != b.share {
			// The size changed, so the bucket is rebuilt — but it keeps what it
			// had, clamped, rather than being refilled.
			capacity := time.Duration(share * float64(g.capacity))
			if floor := g.costLocked(233); capacity < floor {
				capacity = floor
			}
			b.share, b.capacity = share, capacity
			if b.tokens > capacity {
				b.tokens = capacity
			}
		}
	}
}

// check applies every rule without charging. Caller holds the lock.
func (g *Governor) check(n int, class Class) error {
	now := g.clk.Now()
	g.refill(now)

	// Quiet hours are absolute: a sysop who asked for silence at night gets
	// silence, including for control traffic.
	for _, w := range g.cfg.QuietHours {
		if w.Contains(now) {
			return errQuiet
		}
	}

	// §7.6: above ~40% channel utilization, nothing but DM traffic.
	if g.haveTelemetry && g.chanUtil >= silenceThreshold && class != ClassDM {
		return errBusy
	}

	air := g.cfg.Preset.Packet(n)
	if !g.dutyAllows(now, air) {
		return errDuty
	}

	cost := time.Duration(float64(air) * g.r)
	if g.tokens < cost+time.Duration(reserve[class]*float64(g.capacity)) {
		return errBudget
	}
	return nil
}

// Refusal reasons, distinguished so the status screen can say WHY a node is
// quiet. "Over budget" and "the channel is busy" call for different responses
// from a sysop, and both look like silence.
var (
	errQuiet  = fmt.Errorf("quiet hours")
	errBusy   = fmt.Errorf("channel too busy")
	errDuty   = fmt.Errorf("regional duty cycle")
	errBudget = fmt.Errorf("over budget")
)

// refill adds accrued budget and decays the busy-channel penalty.
func (g *Governor) refill(now time.Time) {
	elapsed := now.Sub(g.last)
	if elapsed <= 0 {
		return
	}
	g.last = now

	rate := g.shareFraction() / g.penalty
	g.tokens += time.Duration(float64(elapsed) * rate)
	if g.tokens > g.capacity {
		g.tokens = g.capacity
	}
	g.pruneTX(now)
}

// dutyAllows enforces the region's rolling-hour transmit limit.
func (g *Governor) dutyAllows(now time.Time, air time.Duration) bool {
	limit, ok := dutyCycleLimit[g.cfg.Region]
	if !ok {
		return true
	}
	g.pruneTX(now)
	var used time.Duration
	for _, r := range g.localTX {
		used += r.air
	}
	return used+air <= time.Duration(limit*float64(time.Hour))
}

func (g *Governor) pruneTX(now time.Time) {
	cutoff := now.Add(-time.Hour)
	keep := g.localTX[:0]
	for _, r := range g.localTX {
		if r.at.After(cutoff) {
			keep = append(keep, r)
		}
	}
	g.localTX = keep
}

// Observe records the radio's own telemetry (§7.6).
//
// Above the backoff threshold the refill rate is divided by a penalty that
// doubles with each consecutive busy reading — exponential backoff, as the
// design asks — and halves back toward 1 once the channel clears.
func (g *Governor) Observe(channelUtilization, airUtilTx float64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.refill(g.clk.Now())
	g.chanUtil, g.airUtilTx, g.haveTelemetry = channelUtilization, airUtilTx, true

	switch {
	case channelUtilization >= backoffThreshold:
		g.penalty *= 2
		if g.penalty > maxPenalty {
			g.penalty = maxPenalty
		}
	default:
		g.penalty /= 2
		if g.penalty < 1 {
			g.penalty = 1
		}
	}
}

// NoteEcho records one of our own packets heard coming back — a rebroadcast by
// a neighbour, which is exactly what R counts (§7.6).
//
// This is a weak estimator and is documented as such: Meshtastic firmware
// deduplicates before the client API, so most repeats of a given packet are
// invisible from here and the count is a floor, not a census. It is enough to
// notice R climbing, which §7.6 says is the point — a survey (§7.8) is what
// produces a number worth trusting.
func (g *Governor) NoteEcho() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.echoes++
	g.reestimate()
}

// reestimate updates R from observed echoes. Caller holds the lock.
func (g *Governor) reestimate() {
	if g.cfg.FloodMultiplierOverride || g.sentPackets < 20 {
		return
	}
	// Each of our packets costs the channel one transmission by us plus every
	// rebroadcast. Echoes we can see are rebroadcasts, so R ≥ 1 + echoes/sent.
	observed := 1 + float64(g.echoes)/float64(g.sentPackets)
	// Never revise DOWNWARD below the configured starting value. What we can
	// see is a floor, and treating an under-count as evidence of a cheap mesh
	// is how a governor talks itself into spending more than it should.
	if observed > g.r {
		g.r = observed
	}
}

// NoteInbound records traffic received from a peer and reports whether it is
// still within quota (§7.6).
func (g *Governor) NoteInbound(peer identity.NodeID, bytes int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.clk.Now()
	rec := g.inbound[peer]
	if rec == nil || now.Sub(rec.windowStart) >= time.Hour {
		rec = &inboundRec{windowStart: now}
		g.inbound[peer] = rec
	}
	rec.bytes += bytes
	if rec.bytes <= g.cfg.InboundQuotaPerHour {
		return true
	}
	if !rec.alerted {
		rec.alerted = true
		g.stats.InboundAlerts++
		if g.cfg.OnAlert != nil {
			g.cfg.OnAlert(fmt.Sprintf(
				"peer %s has sent %d bytes in the last hour, over its %d byte quota — "+
					"a rogue or malfunctioning instance can flood a mesh, so its traffic is being dropped",
				peer.Short(), rec.bytes, g.cfg.InboundQuotaPerHour))
		}
	}
	return false
}

// Budget reports the current allowance in the units link.Budget uses.
func (g *Governor) Budget() link.Budget {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refill(g.clk.Now())

	per := time.Duration(float64(g.cfg.Preset.Packet(233)) * g.r)
	busy := g.haveTelemetry && g.chanUtil >= silenceThreshold
	return link.Budget{
		Available:    g.tokens,
		PerDatagram:  per,
		Backpressure: busy || g.tokens < per,
	}
}

// R reports the flood multiplier currently in use.
func (g *Governor) R() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.r
}

// SetInstances updates the divisor as the NODE roster grows (§6.1).
func (g *Governor) SetInstances(n int) {
	if n < 1 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refill(g.clk.Now())
	g.cfg.Instances = n
	g.capacity = g.burst()
	if g.tokens > g.capacity {
		g.tokens = g.capacity
	}
}

// Stats reports counters for the status screen.
func (g *Governor) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Explain renders the budget the way §7.6 insists a sysop sees it.
//
// "Sysops must not have to compute this." A share expressed as 0.1% of channel
// time is not something anyone can act on; "about eleven full packets a day" is.
func (g *Governor) Explain() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	perDay := 86400 * g.shareFraction()
	full := float64(g.cfg.Preset.Packet(233)) * g.r / float64(time.Second)
	short := float64(g.cfg.Preset.Packet(100)) * g.r / float64(time.Second)

	out := fmt.Sprintf(
		"share: %.0f channel-seconds/day — about %.0f full packets, or %.0f short posts "+
			"(%.0f%% ceiling ÷ %d instances, R=%.1f%s)",
		perDay, perDay/full, perDay/short,
		g.cfg.CeilingPercent, g.cfg.Instances, g.r, g.rNote())

	// §7.6 insists a sysop sees the derived figure rather than the fraction.
	// "0.1 of your share" is not something anyone can act on; "about one full
	// packet a day" is, and it is the number that decides whether a league is
	// playable at all.
	if len(g.areaShares) > 0 {
		areas := make([][4]byte, 0, len(g.areaShares))
		for a := range g.areaShares {
			areas = append(areas, a)
		}
		// Sorted: this string is read from a status screen and a log, and map
		// order would reshuffle it on every call (§6.2.1 rule 2).
		sort.Slice(areas, func(i, j int) bool {
			return bytes.Compare(areas[i][:], areas[j][:]) < 0
		})
		for _, a := range areas {
			share := g.areaShares[a]
			out += fmt.Sprintf("\n  area %x: %.0f%% of that — about %.1f full packets/day",
				a[:], share*100, perDay*share/full)
		}
	}
	return out
}

// AreaPacketsPerDay reports how many full packets an area's share buys per day.
//
// Exported for the door-event flusher, which derives its batch window from it
// (§9.5). Everything that makes this number what it is — the ceiling, the
// instance count, R, and the area's own share — lives here, so a caller that
// computed it itself would be a second copy of the arithmetic that drifts the
// first time any of them changes.
//
// An area with no configured share gets the node's whole rate, which is the
// truth: nothing is capping it.
func (g *Governor) AreaPacketsPerDay(area [4]byte) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	perDay := 86400 * g.shareFraction()
	if share, ok := g.areaShares[area]; ok && share > 0 {
		perDay *= share
	}
	full := float64(g.cfg.Preset.Packet(233)) * g.r / float64(time.Second)
	if full <= 0 {
		return 0
	}
	return perDay / full
}

func (g *Governor) rNote() string {
	if g.cfg.FloodMultiplierOverride {
		return " pinned"
	}
	if g.echoes == 0 {
		return " unmeasured"
	}
	return " observed"
}

// costLocked is the channel cost of one packet. Caller holds the lock, or is
// constructing.
func (g *Governor) costLocked(n int) time.Duration {
	return time.Duration(float64(g.cfg.Preset.Packet(n)) * g.r)
}
