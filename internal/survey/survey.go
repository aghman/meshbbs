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
// hardware disproved, is that they can be sampled on demand: a Heltec V3 on
// firmware 2.7.15 refreshes its self-reported metrics only on the telemetry
// interval, which ships as an "unset" sentinel meaning the 30-minute default.
// Watched for four minutes, the numbers did not move once, and a self-addressed
// telemetry request is answered with a routing error rather than metrics.
//
// So a survey cannot sample every minute the way §7.8.2 describes unless the
// node is told to update more often. Preflight checks for that and refuses to
// run rather than producing a curve drawn through four identical readings.
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
// It returns false when the node is on the firmware default, which is the case
// that matters: at 30 minutes, no survey shorter than several hours can sample
// anything.
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
	R           float64
	Low, High   float64
	Confident   bool
	Explain     string
	DeltaBusy   float64
	OurTransmit float64
}

// Report is everything a survey produced.
type Report struct {
	Started, Ended time.Time
	Preset         airtime.Preset
	Region         string
	Baseline       Phase
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
// # Why one baseline is not enough
//
// R is computed as (rise in channel utilization) ÷ (rise in our own transmit),
// against a baseline measured once, up to two hours before the last load phase
// ends. That arithmetic silently assumes the mesh was equally busy the whole
// time. On a community mesh it is not: traffic climbs through the evening as
// people get home, and every point it climbs by is attributed to rebroadcasts
// of OUR packets — inflating R with no signal that anything is wrong. The
// existing AbortRise guard is 15 points, which is a runaway-channel tripwire,
// not a drift detector; a 0.5-point drift against a 1-point load is invisible
// to it and doubles the answer.
//
// Measuring the baseline again at the end does not correct for this — the
// correction would need to know WHEN the drift happened — but it does make it
// visible, which is the difference between an R with an error bar and an R with
// a bias nobody can see. Positive means the mesh got busier and R is
// over-stated.
//
// # Why the two baselines are not compared directly
//
// The obvious implementation — closing mean minus opening mean — reports drift
// on a mesh that did not move, and the estimator test proved it: a steady
// synthetic mesh came back at +3.17 points. The cause is our OWN transmissions.
// air_util_tx is a rolling average, so when the closing baseline starts the
// node is still carrying the load phase that just ended, and channel
// utilization still carries its rebroadcasts. The closing baseline is therefore
// biased upward by exactly the thing the survey was measuring.
//
// Waiting for the average to decay would work and would cost another hour of
// silence. Subtracting the residual is free and uses a quantity already
// sampled: each phase knows its own mean transmit, and channel utilization is
// ambient + transmit × R by the same identity §7.8.1 inverts to get R. So both
// baselines are reduced to their ambient component before being compared, and
// what remains is the mesh's own traffic.
//
// # Why this returns a floor and not an answer
//
// Using the measured R to subtract the residual has feedback in it, and the
// direction is unhelpful: drift inflates R, an inflated R subtracts more than
// the residual is worth, and the drift shrinks. Measured on the synthetic mesh,
// a true ambient rise of 1.50 points reported as 0.31 — the correction ate 80%
// of the signal it was there to expose.
//
// So this is deliberately the CONSERVATIVE end. DriftRaw is the other end, with
// nothing subtracted, biased upward by however much of our own load average had
// not yet decayed. The truth is between them, and the report prints both rather
// than choosing: a warning that fires on the floor cannot cry wolf on a steady
// mesh, and a reader who sees a wide gap between the two knows the closing
// baseline started too soon after the load.
//
// Correcting this properly means waiting for air_util_tx to decay before
// sampling — Meshtastic averages it over an hour, so that is an hour of extra
// silence per run. Worth doing if drift ever turns out to be the thing limiting
// a measurement; not worth it before that is known.
func (r *Report) Drift() (float64, bool) {
	if len(r.BaselineEnd.Samples) == 0 {
		return 0, false
	}
	// Without a confident estimate, assume no multiplication: that subtracts
	// the least, so an unmeasurable run cannot manufacture a drift warning.
	rr := 1.0
	if best, ok := r.Best(); ok {
		rr = best.R
	}
	ambient := func(p Phase) float64 { return p.MeanChannel() - rr*p.MeanTx() }
	return ambient(r.BaselineEnd) - ambient(r.Baseline), true
}

// DriftRaw is the uncorrected difference between the two baselines.
//
// The upper end of the range Drift floors: it still contains whatever part of
// our own load the node's rolling transmit average had not yet forgotten.
func (r *Report) DriftRaw() (float64, bool) {
	if len(r.BaselineEnd.Samples) == 0 {
		return 0, false
	}
	return r.BaselineEnd.MeanChannel() - r.Baseline.MeanChannel(), true
}

// Run performs a survey.
//
// It is deliberately conservative about starting: a survey transmits, and §7.8.3
// requires the load phase to obey the same civic constraints the governor does.
// Refusing to run on a busy channel is not an inconvenience, it is the feature.
func Run(ctx context.Context, node Node, cfg Config) (*Report, error) {
	cfg.applyDefaults()

	if err := preflight(node, cfg); err != nil {
		return nil, err
	}

	rep := &Report{
		Started: cfg.Clock.Now(),
		Preset:  node.Preset(),
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
	rep.BaselineEnd = tail
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

// preflight refuses surveys that cannot produce a number.
func preflight(node Node, cfg Config) error {
	cadence, configured := TelemetryCadence(node.TelemetryIntervalSecs())
	if !configured || cadence > cfg.Sample {
		// This is the check hardware taught us to make. Without it the survey
		// runs for an hour and reports a confident R computed from the same
		// cached reading sampled sixty times.
		return fmt.Errorf("%w: it refreshes every %s but the survey samples every %s\n"+
			"set Telemetry → device metrics update interval to %.0fs on the node "+
			"(Meshtastic app → Module Settings → Telemetry), then run this again",
			ErrMetricsTooSlow, cadence, cfg.Sample, cfg.Sample.Seconds())
	}
	if _, ok := node.Metrics(); !ok {
		return fmt.Errorf("%w: the node has not reported any metrics yet", ErrMetricsTooSlow)
	}
	return nil
}

// measure runs one phase, transmitting if hopLimit is non-zero.
func measure(ctx context.Context, node Node, cfg Config, name string, hopLimit uint32, base *Phase) (Phase, error) {
	dur := cfg.Baseline
	if hopLimit != 0 {
		dur = cfg.Load
	}

	p := Phase{Name: name, HopLimit: hopLimit, Started: cfg.Clock.Now()}
	deadline := p.Started.Add(dur)

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
		if base != nil && len(p.Samples) > 0 {
			latest := p.Samples[len(p.Samples)-1]
			ambient := latest.ChannelUtilization - latest.AirUtilTx
			if ambient > base.MeanChannel()+cfg.AbortRise {
				return p, fmt.Errorf("%w: ambient %.1f%% against a %.1f%% baseline",
					ErrAborted, ambient, base.MeanChannel())
			}
		}

		for hopLimit != 0 && !cfg.Clock.Now().Before(nextTx) {
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
	tx := load.MeanTx() - base.MeanTx()

	est := REstimate{HopLimit: hop, DeltaBusy: delta, OurTransmit: tx}
	if tx <= 0 {
		est.Explain = "our own transmissions did not register in the node's telemetry"
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
