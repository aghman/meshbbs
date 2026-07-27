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
	"fmt"
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
	}
	return "unknown"
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

	stats Stats
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
	InboundAlerts  int
	ChargedChannel time.Duration
}

// New builds a governor.
func New(cfg Config) (*Governor, error) {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
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
func (g *Governor) Allow(n int, class Class) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.check(n, class); err != nil {
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

	g.tokens -= cost
	g.localTX = append(g.localTX, txRecord{at: now, air: air})
	g.sentPackets++
	g.stats.Allowed++
	g.stats.ChargedChannel += cost
	return true
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

	return fmt.Sprintf(
		"share: %.0f channel-seconds/day — about %.0f full packets, or %.0f short posts "+
			"(%.0f%% ceiling ÷ %d instances, R=%.1f%s)",
		perDay, perDay/full, perDay/short,
		g.cfg.CeilingPercent, g.cfg.Instances, g.r, g.rNote())
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
