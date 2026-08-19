// Package survey measures R, the flood multiplier, on a real mesh (design
// §7.8).
//
// # Why this exists and why it runs before the governor
//
// R is the one number in the design that cannot be derived, simulated or
// reasoned about: it is a property of a specific mesh's RF topology at a
// specific moment. Every airtime figure in the document scales linearly with
// it, and it is currently a guessed 4 (`N10`). If the real value is 8, every
// budget is twice as generous as it should be — so the governor's defaults
// should be set from a measurement rather than the measurement being built to
// check the governor afterwards.
//
// # The method, and the one thing hardware changed about it
//
// §7.8.1's trick: if our node is the only originator during a window, then
// everything on the channel that is not our own transmission is a rebroadcast
// of it.
//
//	R ≈ (channel_utilization_load − channel_utilization_baseline) / air_util_tx_load
//
// Both figures come from the node's own telemetry. What §7.8 assumed, and
// hardware disproved, is that they can be sampled on demand: a self-addressed
// telemetry request is answered with a routing error rather than metrics, so
// the only reading available is whatever the node last wrote into its own
// nodedb entry. A survey can therefore go no faster than that entry refreshes.
//
// The trap is which setting controls it, and it cost an afternoon to find.
// `device_update_interval` governs how often the node BROADCASTS telemetry to
// the mesh. It does NOT govern how often the firmware refreshes its own entry,
// which is the thing read here. Measured on a Heltec V3 on firmware 2.7.15
// with that interval set to 3600s, uptime advanced in exact 60-second steps and
// both utilization figures moved with it — a factor of sixty between the number
// the node declares and the rate it actually refreshes at.
//
// An earlier reading of this same radio, four minutes with no movement at all,
// was taken with the telemetry module switched OFF. That is the thing that
// actually stops the refresh, and no interval setting substitutes for it.
//
// So preflight MEASURES the refresh rate instead of reading the configured
// interval, because the configured interval is a statement about the mesh
// rather than about us. See probeCadence.
package survey

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/clock"
)

// DefaultTelemetryInterval is what the firmware uses when the config says
// nothing.
const DefaultTelemetryInterval = 30 * time.Minute

// maxPlausibleInterval separates a real configured interval from the "unset"
// sentinel. A stock node reports 2147483647 seconds — INT32_MAX — and nobody
// configures a telemetry interval of a day.
const maxPlausibleInterval = 24 * time.Hour

// TelemetryCadence interprets the interval a node reports.
//
// This is the node's telemetry BROADCAST interval, not the rate at which it
// refreshes the metrics a survey reads — the two are independent, and assuming
// otherwise is what probeCadence exists to stop. It survives because `mesh
// info` displays it and because it is useful context in a refusal, not because
// anything decides on it.
func TelemetryCadence(secs uint32) (time.Duration, bool) {
	if secs == 0 {
		return DefaultTelemetryInterval, false
	}
	d := time.Duration(secs) * time.Second
	if d > maxPlausibleInterval {
		return DefaultTelemetryInterval, false
	}
	return d, true
}

// Metrics is one reading of the node's own telemetry.
type Metrics struct {
	// ChannelUtilization is the percentage of time the channel was busy.
	ChannelUtilization float64
	// AirUtilTx is the percentage of time this node spent transmitting.
	AirUtilTx float64
	// Uptime distinguishes a fresh reading from a repeat of a stale one. The
	// metrics can legitimately be unchanged; uptime cannot.
	Uptime time.Duration
	At     time.Time
}

// fresh reports whether m is a genuinely new reading rather than the same
// snapshot read twice.
func (m Metrics) fresh(prev Metrics) bool {
	return m.Uptime != prev.Uptime
}

// Heard is one packet the radio reported receiving.
//
// Note what is NOT here: duplicate receptions. Firmware deduplicates before the
// client API, so the rebroadcasts that R is made of are invisible from this
// side — which is exactly why R is measured from channel utilization rather
// than by counting packets (§7.8.2 puts duplicate counting in the serial debug
// log, where it is still visible).
type Heard struct {
	From     uint32
	ID       uint32
	HopStart uint32
	HopLimit uint32
	SNR      float32
	RSSI     int32
	Bytes    int
	Portnum  string
	At       time.Time
}

// Hops reports how many hops a packet travelled, and whether that is knowable.
// Firmware 2.2+ exposes hop_start, which is what makes the subtraction possible.
func (h Heard) Hops() (int, bool) {
	if h.HopStart == 0 {
		return 0, false
	}
	return int(h.HopStart) - int(h.HopLimit), true
}

// Node is the radio-facing half of a survey.
//
// It is an interface so the engine can be tested against synthetic meshes with
// known R — the measurement is arithmetic on two noisy series, and arithmetic
// that only ever runs against real hardware is arithmetic nobody has checked.
type Node interface {
	// Metrics returns the latest telemetry the node has reported.
	Metrics() (Metrics, bool)
	// Transmit puts one packet of n application bytes on the air.
	Transmit(ctx context.Context, n int, hopLimit uint32) error
	// Heard returns the census feed of received packets.
	Heard() <-chan Heard
	// Preset is the modem configuration, for airtime arithmetic.
	Preset() airtime.Preset
	// TelemetryIntervalSecs is what the node says about its own cadence.
	TelemetryIntervalSecs() uint32
}

// Config parameterises a survey.
type Config struct {
	// Baseline is how long to listen while transmitting nothing.
	Baseline time.Duration
	// Load is how long to transmit for, at each hop limit.
	Load time.Duration
	// Sample is how often to read the node's metrics.
	Sample time.Duration
	// Probe is how long preflight watches before deciding whether the node's
	// metrics move at all. Proportional to Sample rather than fixed, since a
	// survey sampling every ten minutes needs a correspondingly longer look
	// before "nothing moved" means anything.
	Probe time.Duration
	// HopLimits to sweep. §7.8.2 asks for 1, 3 and 5 — the sweep is the most
	// actionable output, because it prices what hop limit costs the commons.
	HopLimits []uint32
	// TargetDuty is the air_util_tx percentage to aim for during load. §7.8.2
	// says 1-2%: enough to rise above baseline noise, low enough to stay a good
	// neighbour.
	TargetDuty float64
	// PayloadBytes is the size of each load packet.
	PayloadBytes int
	// MaxAmbient refuses to start if the channel is already this busy. Someone
	// else needs the channel more than we need the measurement.
	MaxAmbient float64
	// AbortRise aborts if ambient utilization climbs this many points above
	// baseline mid-run, for the same reason.
	AbortRise float64
	// Clock is injected per §12.1.
	Clock clock.Clock
	// OnProgress receives human-readable progress, since a survey is measured
	// in hours and a silent hour looks like a hang.
	OnProgress func(string)
}

func (c *Config) applyDefaults() {
	if c.Baseline <= 0 {
		c.Baseline = 30 * time.Minute
	}
	if c.Load <= 0 {
		c.Load = 30 * time.Minute
	}
	if c.Sample <= 0 {
		c.Sample = time.Minute
	}
	if c.Probe <= 0 {
		// The longest refresh gap that still yields minPhaseSamples in the
		// shortest phase. Watching for exactly that means a probe which sees
		// nothing has proved the phase would fail, without guessing at a
		// window. Floored at two samples so the probe always gets to look
		// twice.
		shortest := c.Baseline
		if c.Load < shortest {
			shortest = c.Load
		}
		c.Probe = shortest / minPhaseSamples
		if c.Probe < 2*c.Sample {
			c.Probe = 2 * c.Sample
		}
	}
	if len(c.HopLimits) == 0 {
		c.HopLimits = []uint32{1, 3, 5}
	}
	if c.TargetDuty <= 0 {
		c.TargetDuty = 1.0
	}
	if c.PayloadBytes <= 0 {
		c.PayloadBytes = 200
	}
	if c.MaxAmbient <= 0 {
		c.MaxAmbient = 25
	}
	if c.AbortRise <= 0 {
		c.AbortRise = 15
	}
	if c.Clock == nil {
		c.Clock = clock.NewReal()
	}
}

// Errors a caller is expected to handle rather than merely report.
var (
	// ErrMetricsTooSlow means the node cannot be sampled often enough to
	// measure anything. It is the most likely reason a survey will not start.
	ErrMetricsTooSlow = errors.New("survey: the node updates its metrics too slowly to sample")
	// ErrChannelBusy means the mesh is already loaded. Not our turn.
	ErrChannelBusy = errors.New("survey: the channel is already too busy to survey politely")
	// ErrAborted means ambient traffic rose mid-run.
	ErrAborted = errors.New("survey: aborted because the channel got busier")
	// ErrNoFreshMetrics means the node stopped updating during a phase.
	ErrNoFreshMetrics = errors.New("survey: the node reported no fresh metrics during a phase")
)

// Phase is one measurement window.
type Phase struct {
	Name     string
	HopLimit uint32
	Started  time.Time
	Ended    time.Time
	// Samples holds only FRESH readings; repeats of a stale snapshot are
	// discarded, because averaging them in would fake precision the node never
	// provided.
	Samples []Metrics
	Stale   int
	// Sent and Airtime are what we put on the air during this phase.
	Sent    int
	Airtime time.Duration
}

// DutyCycle is the percentage of this phase we spent transmitting, computed
// from what we actually sent rather than from what the node reported.
//
// This is the denominator §7.8.1's arithmetic needs, and it is exact. The
// node's own air_util_tx cannot serve, and the reason was measured rather than
// assumed: the two figures live on completely different timescales.
//
// A burst of ~19s of airtime on a Heltec V3 (firmware 2.7.15) drove
// channel_utilization to 17.46% and it was back at ambient 90 seconds later —
// a window of a minute or less, since an hour-scale average could not exceed
// ~0.5% from that much airtime however it was arranged. Over the same burst
// air_util_tx went 0.356 -> 0.851 and was still 0.814 THIRTY MINUTES after the
// last packet.
//
// So the numerator is fast and air_util_tx is slow, and dividing one by the
// other compares two different windows. In a hop sweep that reads LOW in the
// first phase (the average has not filled) and HIGH in the last (it still
// carries the earlier ones), so R falls with hop limit — an artifact of the
// instrument that looks exactly like a property of the mesh. Measured: three
// phases of an identical 1.07% load reported 0.308, 1.011 and 1.353.
//
// The tradeoff, stated because it biases the answer: this counts what the
// application asked to send and misses protocol overhead, retries and the
// node's own telemetry broadcasts. So it slightly UNDER-states our occupancy
// and therefore slightly OVER-states R — the safe direction, since over-stating
// R under-spends the commons (§1.1), and a bounded bias rather than a drifting
// one.
func (p Phase) DutyCycle() float64 {
	span := p.Ended.Sub(p.Started)
	if span <= 0 {
		return 0
	}
	return p.Airtime.Seconds() / span.Seconds() * 100
}

// MeanChannel and MeanTx average the phase's fresh samples.
func (p Phase) MeanChannel() float64 { return mean(p.channelSeries()) }
func (p Phase) MeanTx() float64      { return mean(p.txSeries()) }
func (p Phase) SDChannel() float64   { return stddev(p.channelSeries()) }

func (p Phase) channelSeries() []float64 {
	out := make([]float64, len(p.Samples))
	for i, s := range p.Samples {
		out[i] = s.ChannelUtilization
	}
	return out
}

func (p Phase) txSeries() []float64 {
	out := make([]float64, len(p.Samples))
	for i, s := range p.Samples {
		out[i] = s.AirUtilTx
	}
	return out
}

// REstimate is R at one hop limit, with an honest interval.
type REstimate struct {
	HopLimit uint32
	// R is the point estimate. It is meaningless without Low and High.
	R         float64
	Low, High float64
	Confident bool
	Explain   string
	DeltaBusy float64
	// OurTransmit is the denominator R was computed from: our own duty cycle
	// over the phase, from Phase.Airtime.
	OurTransmit float64
	// ReportedTx is what the node's air_util_tx claimed over the same phase.
	// It does not feed R and is kept only for comparison — a wide gap is the
	// node's hour-scale average lagging, which is expected rather than alarming
	// and is why it is not the denominator.
	ReportedTx float64
}

// Report is everything a survey produced.
type Report struct {
	Started, Ended time.Time
	Preset         airtime.Preset
	Region         string
	// Cadence is the metric refresh rate preflight MEASURED, which is not
	// necessarily the interval the node declares. Recorded because §7.8.3 wants
	// these reports comparable across sysops, and two runs at different
	// cadences are not directly comparable.
	Cadence  time.Duration
	Baseline Phase
	// BaselineEnd repeats the baseline after the load phases, with the radio
	// silent again. See Drift.
	BaselineEnd Phase
	Loads       []Phase
	Estimates   []REstimate
	Census      Census
	Notes       []string
}

// Drift is how much the ambient channel moved between the opening and closing
// baselines, in percentage points.
//
// # Why a second baseline exists
//
// R is computed as (rise in channel utilization) ÷ (our own duty cycle),
// against a baseline measured once, up to two hours before the last load phase
// ends. That arithmetic assumes the mesh was equally busy the whole time. On a
// community mesh it is not: traffic climbs through the evening as people get
// home, and every point it climbs by is attributed to rebroadcasts of OUR
// packets — inflating R with no signal that anything is wrong. The AbortRise
// guard is 15 points, which is a runaway-channel tripwire, not a drift
// detector; a 0.5-point drift against a 1-point load is invisible to it and
// doubles the answer.
//
// Measuring the baseline again at the end does not correct for this — the
// correction would need to know WHEN the drift happened — but it does make it
// visible, which is the difference between an R with an error bar and an R with
// a bias nobody can see. Positive means the mesh got busier and R is
// over-stated.
//
// # Why this is now a plain subtraction
//
// It was not always. An earlier version subtracted an estimated residual from
// both baselines before comparing them, on the theory that the closing baseline
// was still carrying the load phase that had just ended — and it had to return
// a conservative floor, because using the measured R to compute that residual
// puts feedback in the loop. On the first hardware run that produced a reported
// range of -8.32 to +0.78 points against rises of 2 to 3 points, which is not a
// measurement of anything.
//
// The premise was wrong, and measuring it settled the matter. channel
// utilization is a FAST figure: a burst that drove it to 17.46% was back at
// ambient 90 seconds later, and across a 30-minute silent decay it averaged
// 1.89 against a pre-burst ambient of 1.97. There is no residual to subtract,
// because the channel measure has already forgotten. (air_util_tx is the slow
// one — still elevated half an hour on — which is exactly why it is not in this
// arithmetic either.)
//
// Both baselines are silent, so both read pure ambient, and the honest
// comparison is the direct one.
func (r *Report) Drift() (float64, bool) {
	if len(r.BaselineEnd.Samples) == 0 {
		return 0, false
	}
	return r.BaselineEnd.MeanChannel() - r.Baseline.MeanChannel(), true
}

// Check runs preflight alone: it decides whether this node can be surveyed at
// all, and puts nothing on the air doing it.
//
// It exists because that answer costs a couple of minutes while a survey costs
// hours, and until it did, the only way to discover a node could not be
// measured was to commit to the whole run and read the refusal.
func Check(ctx context.Context, node Node, cfg Config) (time.Duration, error) {
	cfg.applyDefaults()
	return preflight(ctx, node, cfg)
}

// Run performs a survey.
//
// It is deliberately conservative about starting: a survey transmits, and §7.8.3
// requires the load phase to obey the same civic constraints the governor does.
// Refusing to run on a busy channel is not an inconvenience, it is the feature.
func Run(ctx context.Context, node Node, cfg Config) (*Report, error) {
	cfg.applyDefaults()

	cadence, err := preflight(ctx, node, cfg)
	if err != nil {
		return nil, err
	}

	rep := &Report{
		Started: cfg.Clock.Now(),
		Preset:  node.Preset(),
		Cadence: cadence,
	}
	census := newCensus()
	stop := collect(ctx, node.Heard(), census)
	defer stop()

	progress(cfg, fmt.Sprintf("baseline: listening for %s, transmitting nothing", cfg.Baseline))
	base, err := measure(ctx, node, cfg, "baseline", 0, nil)
	if err != nil {
		return nil, err
	}
	rep.Baseline = base

	if base.MeanChannel() > cfg.MaxAmbient {
		return nil, fmt.Errorf("%w: %.1f%% busy, limit %.1f%%",
			ErrChannelBusy, base.MeanChannel(), cfg.MaxAmbient)
	}
	progress(cfg, fmt.Sprintf("baseline: channel %.2f%% busy (±%.2f), %d fresh samples",
		base.MeanChannel(), base.SDChannel(), len(base.Samples)))

	for _, hop := range cfg.HopLimits {
		progress(cfg, fmt.Sprintf("load: %s at hop limit %d, aiming for %.1f%% transmit",
			cfg.Load, hop, cfg.TargetDuty))

		load, err := measure(ctx, node, cfg, fmt.Sprintf("load hop=%d", hop), hop, &base)
		if err != nil {
			return rep, err
		}
		rep.Loads = append(rep.Loads, load)

		est := estimate(base, load, hop)
		rep.Estimates = append(rep.Estimates, est)
		progress(cfg, fmt.Sprintf("hop %d: R ≈ %.1f (%.1f–%.1f) — %s",
			hop, est.R, est.Low, est.High, est.Explain))
	}

	// The closing baseline runs for as long as the opening one, deliberately: a
	// shorter phase would have a wider standard error than the drift it is
	// trying to detect, which would answer the question with a shrug.
	progress(cfg, fmt.Sprintf("baseline: listening again for %s to measure ambient drift", cfg.Baseline))
	tail, err := measure(ctx, node, cfg, "baseline (closing)", 0, nil)
	if err != nil {
		// The estimates are already computed and are the point of the run, so a
		// failure here costs the drift check and nothing else. Returning the
		// report alongside the error lets the caller keep what was measured.
		return rep, err
	}
	rep.BaselineEnd = tail.afterSettling(channelSettle)
	if drift, ok := rep.Drift(); ok {
		progress(cfg, fmt.Sprintf("baseline: closed at %.2f%% busy, %+.2f points against the opening baseline",
			tail.MeanChannel(), drift))
	}

	stop()
	rep.Census = census.summarise()
	rep.Ended = cfg.Clock.Now()
	rep.Notes = notes(rep)
	return rep, nil
}

// channelSettle is how long the channel-utilization figure goes on carrying
// traffic that has already stopped.
//
// Measured, not assumed: a burst that drove the figure to 17.46% was back at
// ambient 90 seconds later. Two minutes is that with margin, at the one-minute
// resolution the metrics refresh at.
//
// It matters in exactly one place. The closing baseline starts the instant the
// last load phase ends, so its first readings still contain our own packets,
// and counting them makes a steady mesh look like it got busier.
const channelSettle = 2 * time.Minute

// afterSettling drops the samples taken before the channel figure had time to
// forget what came before this phase.
func (p Phase) afterSettling(d time.Duration) Phase {
	cut := p.Started.Add(d)
	kept := p.Samples[:0:0]
	for _, m := range p.Samples {
		if !m.At.Before(cut) {
			kept = append(kept, m)
		}
	}
	// Never hand back an empty phase: a short baseline that is all settling is
	// better reported as noisy than as absent.
	if len(kept) == 0 {
		return p
	}
	p.Samples = kept
	return p
}

// minPhaseSamples is how many fresh readings a phase needs before its mean and
// standard error are worth quoting.
//
// Below this the interval on R is drawn through so few points that it claims a
// precision the node never provided — which is the failure mode the original
// version of this check was written to prevent, and the part of it that was
// right.
const minPhaseSamples = 5

// probeCadence measures how often the node actually refreshes its metrics.
//
// Preflight cannot ask. The one field that looks like an answer —
// device_update_interval — is about broadcasting to the mesh, and has been
// observed disagreeing with the real refresh rate by a factor of sixty (see the
// package comment). So this watches, using the same freshness test the phases
// themselves use, and believes only what it sees.
//
// It wants TWO fresh readings rather than one. One proves only that a refresh
// happened somewhere inside the window; two bound the interval between them,
// which is the number every message downstream of here quotes.
func probeCadence(ctx context.Context, node Node, cfg Config) (time.Duration, int, error) {
	deadline := cfg.Clock.Now().Add(cfg.Probe)

	var prev Metrics
	if m, ok := node.Metrics(); ok {
		prev = m
	}

	var at []time.Time
	for cfg.Clock.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-cfg.Clock.After(cfg.Sample):
		}
		if m, ok := node.Metrics(); ok && m.fresh(prev) {
			at = append(at, cfg.Clock.Now())
			prev = m
		}
	}

	if len(at) < 2 {
		return 0, len(at), nil
	}
	// The mean gap across the whole window, not the first gap: any single gap
	// is quantised by our own sample interval and reads as anywhere between one
	// and two refresh periods.
	span := at[len(at)-1].Sub(at[0])
	return time.Duration(int64(span) / int64(len(at)-1)), len(at), nil
}

// preflight refuses surveys that cannot produce a number, and returns the
// refresh cadence it measured.
func preflight(ctx context.Context, node Node, cfg Config) (time.Duration, error) {
	if _, ok := node.Metrics(); !ok {
		return 0, fmt.Errorf("%w: the node has not reported any metrics yet", ErrMetricsTooSlow)
	}

	progress(cfg, fmt.Sprintf("checking how often the node refreshes its metrics (up to %s)", cfg.Probe))
	cadence, fresh, err := probeCadence(ctx, node, cfg)
	if err != nil {
		return 0, err
	}

	shortest := cfg.Baseline
	if cfg.Load < shortest {
		shortest = cfg.Load
	}

	if fresh < 2 {
		// Both causes are named because this evidence cannot separate them. A
		// window this short sees nothing whether the telemetry module is off or
		// merely slower than the window, and asserting the first is how the
		// previous version of this check sent a sysop after a setting that does
		// not control the thing they were trying to fix.
		declared, _ := TelemetryCadence(node.TelemetryIntervalSecs())
		return 0, fmt.Errorf("%w: watched for %s and saw %d fresh readings, so a %s phase "+
			"cannot yield the %d this needs\n"+
			"either the node's telemetry module is off — enable Meshtastic app → Module "+
			"Settings → Telemetry → Device Metrics — or it refreshes slower than that, in "+
			"which case lengthen --baseline and --load until it has time to\n"+
			"(the node declares a %s interval, but that governs telemetry BROADCAST to the "+
			"mesh rather than how often it refreshes the metrics read here, so tuning it "+
			"is not the fix)",
			ErrMetricsTooSlow, cfg.Probe, fresh, shortest, minPhaseSamples, declared)
	}

	// Reachable only because Probe is floored at two samples: below that floor
	// a probe that saw two readings has already proved the cadence is fast
	// enough. With the floor, a short phase can still fail here.
	if got := int(shortest / cadence); got < minPhaseSamples {
		return 0, fmt.Errorf("%w: it refreshes about every %s, which is %d readings in a %s phase and %d are needed\n"+
			"either lengthen --baseline and --load to at least %s, or speed the node's telemetry up",
			ErrMetricsTooSlow, cadence.Round(time.Second), got, shortest, minPhaseSamples,
			(time.Duration(minPhaseSamples) * cadence).Round(time.Minute))
	}

	progress(cfg, fmt.Sprintf("metrics refresh about every %s — %d readings per %s phase",
		cadence.Round(time.Second), int(shortest/cadence), shortest))
	return cadence, nil
}

// measure runs one phase, transmitting if hopLimit is non-zero.
func measure(ctx context.Context, node Node, cfg Config, name string, hopLimit uint32, base *Phase) (Phase, error) {
	dur := cfg.Baseline
	if hopLimit != 0 {
		dur = cfg.Load
	}

	p := Phase{Name: name, HopLimit: hopLimit, Started: cfg.Clock.Now()}
	deadline := p.Started.Add(dur)

	// Stop transmitting before the phase ends, and keep sampling through the
	// gap. The channel figure looks back about a minute, so a packet sent in
	// the last seconds has most of its effect AFTER the phase boundary — the
	// mean would miss it while the duty-cycle denominator still counted the
	// packet, biasing R downward. Downward is the unsafe direction: under-
	// stating R over-spends the commons (§1.1).
	txDeadline := deadline.Add(-channelSettle)

	// Pace transmissions to hit the target duty cycle. One packet costs its
	// airtime; sending one every airtime/(duty) seconds averages to that duty.
	var txEvery time.Duration
	one := node.Preset().Packet(cfg.PayloadBytes)
	if hopLimit != 0 {
		txEvery = time.Duration(float64(one) * 100 / cfg.TargetDuty)
	}
	nextTx := p.Started

	var prev Metrics
	if m, ok := node.Metrics(); ok {
		prev = m
	}

	for cfg.Clock.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-cfg.Clock.After(cfg.Sample):
		}

		if m, ok := node.Metrics(); ok {
			if m.fresh(prev) {
				p.Samples = append(p.Samples, m)
				prev = m
			} else {
				p.Stale++
			}
		}

		// Abort if someone else started needing the channel.
		//
		// Both terms are phase means rather than the latest sample, because the
		// channel figure is fast and therefore spiky: a single reading that
		// happens to contain one of our packets can sit ten points above the
		// mean and would trip this on our own traffic.
		//
		// Our own contribution is discounted from what we KNOW we sent, not
		// from air_util_tx. That average spans about an hour, so early in a
		// phase it discounts almost nothing and the guard mistakes our own load
		// for somebody else's — measured against the fake mesh, it false-aborted
		// at "ambient 28.4%" while the channel was carrying our own packets.
		//
		// The subtraction still assumes R = 1, so what remains includes our own
		// rebroadcasts and reads high. That is the safe direction for a
		// politeness guard — it gives the channel up early rather than late —
		// and is why the threshold is a runaway tripwire at 15 points rather
		// than anything finer.
		// minPhaseSamples before this may fire: the channel figure is spiky, and
		// a mean over one or two readings is not evidence that anyone needs the
		// channel. It false-aborted at "ambient 17.9%" on a mesh sitting at 2%.
		if base != nil && len(p.Samples) >= minPhaseSamples {
			elapsed := cfg.Clock.Now().Sub(p.Started)
			var ours float64
			if elapsed > 0 {
				ours = p.Airtime.Seconds() / elapsed.Seconds() * 100
			}
			ambient := p.MeanChannel() - ours
			if ambient > base.MeanChannel()+cfg.AbortRise {
				return p, fmt.Errorf("%w: ambient %.1f%% against a %.1f%% baseline",
					ErrAborted, ambient, base.MeanChannel())
			}
		}

		for hopLimit != 0 && cfg.Clock.Now().Before(txDeadline) && !cfg.Clock.Now().Before(nextTx) {
			if err := node.Transmit(ctx, cfg.PayloadBytes, hopLimit); err != nil {
				return p, fmt.Errorf("transmitting load: %w", err)
			}
			p.Sent++
			p.Airtime += one
			nextTx = nextTx.Add(txEvery)
		}
	}

	p.Ended = cfg.Clock.Now()
	if len(p.Samples) == 0 {
		return p, fmt.Errorf("%w: %s produced %d stale readings and no fresh ones",
			ErrNoFreshMetrics, name, p.Stale)
	}
	return p, nil
}

// estimate computes R for one load phase against the baseline.
func estimate(base, load Phase, hop uint32) REstimate {
	delta := load.MeanChannel() - base.MeanChannel()
	// Our own duty cycle, computed rather than read off the node. The numerator
	// is a FAST measure (see Phase.DutyCycle), so the denominator has to
	// describe the same window, and air_util_tx does not.
	tx := load.DutyCycle()

	est := REstimate{
		HopLimit:    hop,
		DeltaBusy:   delta,
		OurTransmit: tx,
		ReportedTx:  load.MeanTx() - base.MeanTx(),
	}
	if tx <= 0 {
		est.Explain = "nothing was transmitted during this phase"
		return est
	}
	est.R = delta / tx

	// The interval comes from the noise in the two channel-utilization means:
	// this is the term that decides whether the answer means anything, since a
	// mesh with bursty neighbour traffic can swamp a 1% load entirely.
	se := math.Sqrt(sqr(sem(base.channelSeries())) + sqr(sem(load.channelSeries())))
	est.Low = (delta - 1.96*se) / tx
	est.High = (delta + 1.96*se) / tx
	if est.Low < 0 {
		est.Low = 0
	}

	switch {
	case delta < 2*se:
		est.Explain = "the rise is inside the noise; treat this as an upper bound, not a measurement"
	case est.R < 1:
		est.Explain = "below 1, which is not physical — the baseline was probably noisy or the load too small"
	default:
		est.Confident = true
		est.Explain = fmt.Sprintf("channel rose %.2f points for %.2f points of our own transmission", delta, tx)
	}
	return est
}

func progress(cfg Config, s string) {
	if cfg.OnProgress != nil {
		cfg.OnProgress(s)
	}
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var sum float64
	for _, x := range xs {
		sum += sqr(x - m)
	}
	return math.Sqrt(sum / float64(len(xs)-1))
}

// sem is the standard error of the mean.
func sem(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	return stddev(xs) / math.Sqrt(float64(len(xs)))
}

func sqr(x float64) float64 { return x * x }
