package survey

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/rng"
)

// fakeNode is a synthetic mesh with a KNOWN flood multiplier.
//
// This is the only way to check the arithmetic. On real hardware R is exactly
// the quantity nobody knows, so a survey that "looked plausible" against a
// radio would tell us nothing about whether the estimator works.
type fakeNode struct {
	preset    airtime.Preset
	trueR     float64
	ambient   float64 // baseline channel utilization, percent
	noise     float64 // per-sample jitter, percentage points
	telemSecs uint32
	// refresh is how often the node rewrites its own nodedb entry. Zero means
	// continuously, which is what most of these tests want. It is deliberately
	// INDEPENDENT of telemSecs: on real hardware those two disagreed by a
	// factor of sixty, and a fake that ties them together cannot reproduce the
	// bug this models.
	refresh time.Duration
	clk     *clock.Virtual
	rnd     rng.Source

	mu       sync.Mutex
	txAir    float64 // our cumulative transmit percentage
	uptime   time.Duration
	heard    chan Heard
	sent     int
	ambientF func(time.Time) float64
}

func newFakeNode(clk *clock.Virtual, trueR, ambient float64) *fakeNode {
	return &fakeNode{
		preset:    airtime.LongFast,
		trueR:     trueR,
		ambient:   ambient,
		telemSecs: 60,
		clk:       clk,
		rnd:       rng.NewSeeded(7),
		heard:     make(chan Heard, 256),
	}
}

func (f *fakeNode) Metrics() (Metrics, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Uptime advances with the clock, which is what marks a reading as fresh —
	// quantised to the refresh interval, so a node between refreshes hands back
	// the same snapshot exactly as the firmware does.
	f.uptime = f.clk.Now().Sub(time.Unix(0, 0))
	if f.refresh > 0 {
		f.uptime = f.uptime.Truncate(f.refresh)
	}

	ambient := f.ambient
	if f.ambientF != nil {
		ambient = f.ambientF(f.clk.Now())
	}
	jitter := (f.rnd.Float64() - 0.5) * 2 * f.noise

	// The channel carries the ambient traffic plus our own transmissions and
	// their rebroadcasts — which is exactly the relationship §7.8.1 inverts.
	return Metrics{
		ChannelUtilization: ambient + f.txAir*f.trueR + jitter,
		AirUtilTx:          f.txAir,
		Uptime:             f.uptime,
		At:                 f.clk.Now(),
	}, true
}

func (f *fakeNode) Transmit(ctx context.Context, n int, hop uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent++
	// Each packet adds its airtime to the running percentage. The window here
	// is the whole run, which is close enough for an estimator test.
	elapsed := f.clk.Now().Sub(time.Unix(0, 0)).Seconds()
	if elapsed > 0 {
		f.txAir = float64(f.sent) * f.preset.Packet(n).Seconds() / elapsed * 100
	}
	return nil
}

func (f *fakeNode) Heard() <-chan Heard           { return f.heard }
func (f *fakeNode) Preset() airtime.Preset        { return f.preset }
func (f *fakeNode) TelemetryIntervalSecs() uint32 { return f.telemSecs }

// runVirtual drives a survey on a virtual clock, advancing time from another
// goroutine so the engine's waits complete.
func runVirtual(t *testing.T, node Node, cfg Config, clk *clock.Virtual) (*Report, error) {
	t.Helper()
	cfg.Clock = clk

	done := make(chan struct{})
	var rep *Report
	var err error
	go func() {
		defer close(done)
		rep, err = Run(context.Background(), node, cfg)
	}()

	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case <-done:
			return rep, err
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("survey did not finish on the virtual clock")
		}
		clk.Advance(10 * time.Second)
		time.Sleep(time.Millisecond)
	}
}

// The estimator has to recover a flood multiplier it was not told.
func TestSurveyRecoversAKnownR(t *testing.T) {
	for _, trueR := range []float64{2, 4, 8} {
		clk := clock.NewVirtual(time.Unix(0, 0))
		node := newFakeNode(clk, trueR, 3.0)

		rep, err := runVirtual(t, node, Config{
			Baseline:   20 * time.Minute,
			Load:       20 * time.Minute,
			Sample:     time.Minute,
			HopLimits:  []uint32{3},
			TargetDuty: 1.5,
		}, clk)
		if err != nil {
			t.Fatalf("R=%v: %v", trueR, err)
		}

		best, ok := rep.Best()
		if !ok {
			t.Fatalf("R=%v: no confident estimate", trueR)
		}
		if math.Abs(best.R-trueR) > 0.5 {
			t.Errorf("true R = %v, estimated %.2f (%.2f-%.2f)", trueR, best.R, best.Low, best.High)
		}
	}
}

// The sweep is §7.8.2's most actionable output: it prices hop limit.
func TestSweepShowsHopLimitCost(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 2, 2.0)

	// R climbs with hop limit, as it does on a real mesh.
	byHop := map[uint32]float64{1: 2, 3: 5, 5: 9}
	orig := node.Transmit
	_ = orig
	node.ambientF = func(time.Time) float64 { return 2.0 }

	var rep *Report
	var err error
	func() {
		// Swap trueR as each phase begins by watching the hop limit through a
		// wrapper node.
		w := &hopVaryingNode{fakeNode: node, byHop: byHop}
		rep, err = runVirtual(t, w, Config{
			Baseline:   15 * time.Minute,
			Load:       15 * time.Minute,
			Sample:     time.Minute,
			HopLimits:  []uint32{1, 3, 5},
			TargetDuty: 1.5,
		}, clk)
	}()
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.Estimates) != 3 {
		t.Fatalf("got %d estimates", len(rep.Estimates))
	}
	for i := 1; i < len(rep.Estimates); i++ {
		if rep.Estimates[i].R <= rep.Estimates[i-1].R {
			t.Errorf("R did not climb with hop limit: %.1f at hop %d, %.1f at hop %d",
				rep.Estimates[i-1].R, rep.Estimates[i-1].HopLimit,
				rep.Estimates[i].R, rep.Estimates[i].HopLimit)
		}
	}

	var sb strings.Builder
	rep.Write(&sb, 50)
	if !strings.Contains(sb.String(), "Hop limit is the lever") {
		t.Error("the report does not draw the hop-limit conclusion")
	}
}

// hopVaryingNode makes the synthetic mesh's R depend on the hop limit.
type hopVaryingNode struct {
	*fakeNode
	byHop map[uint32]float64
}

func (h *hopVaryingNode) Transmit(ctx context.Context, n int, hop uint32) error {
	h.fakeNode.mu.Lock()
	if r, ok := h.byHop[hop]; ok {
		h.fakeNode.trueR = r
	}
	h.fakeNode.mu.Unlock()
	return h.fakeNode.Transmit(ctx, n, hop)
}

// A node whose metrics never move cannot be surveyed, and the survey has to say
// so before it transmits rather than after an hour of identical readings.
func TestPreflightRefusesANodeThatDoesNotRefresh(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 2)
	node.refresh = time.Hour // telemetry module effectively off

	_, err := runVirtual(t, node, Config{
		Baseline: 10 * time.Minute, Load: 10 * time.Minute, Sample: time.Minute,
	}, clk)
	if !errors.Is(err, ErrMetricsTooSlow) {
		t.Fatalf("err = %v, want ErrMetricsTooSlow", err)
	}
	// The remedy is the telemetry module, not the interval. Naming the interval
	// here is what sent a sysop chasing a setting that does not control this.
	if !strings.Contains(err.Error(), "Device Metrics") {
		t.Errorf("error does not name the module to enable: %v", err)
	}
}

// The regression this check exists for: a node that DECLARES a slow interval
// but refreshes quickly must be surveyed, not refused.
//
// This is the real hardware case — a Heltec V3 declaring 3600s while rewriting
// its own entry every 60s. The previous preflight read the declaration and
// refused, which blocked a measurement the node was perfectly capable of.
func TestPreflightAcceptsAFastNodeDeclaringASlowInterval(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 2)
	node.telemSecs = 3600      // what the node says about its mesh broadcasts
	node.refresh = time.Minute // what it actually does

	rep, err := runVirtual(t, node, Config{
		Baseline: 20 * time.Minute, Load: 20 * time.Minute, Sample: time.Minute,
		HopLimits: []uint32{3},
	}, clk)
	if err != nil {
		t.Fatalf("survey refused a samplable node: %v", err)
	}
	if rep.Cadence < 30*time.Second || rep.Cadence > 2*time.Minute {
		t.Errorf("measured cadence = %s, want about 1m", rep.Cadence)
	}
}

// A slow-but-moving node is not refused outright: it is refused for THIS phase
// length, and told that longer phases are the other way through.
func TestPreflightOffersLongerPhasesToASlowNode(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 2)
	node.refresh = 5 * time.Minute

	_, err := runVirtual(t, node, Config{
		Baseline: 10 * time.Minute, Load: 10 * time.Minute, Sample: time.Minute,
	}, clk)
	if !errors.Is(err, ErrMetricsTooSlow) {
		t.Fatalf("err = %v, want ErrMetricsTooSlow", err)
	}
	if !strings.Contains(err.Error(), "lengthen") {
		t.Errorf("error does not offer the longer-phase remedy: %v", err)
	}
}

// The INT32_MAX sentinel a stock Heltec reports must read as "default", not as
// an interval of 68 years.
func TestTelemetryCadenceSentinel(t *testing.T) {
	if d, ok := TelemetryCadence(2147483647); ok || d != DefaultTelemetryInterval {
		t.Errorf("sentinel read as %v (configured=%v)", d, ok)
	}
	if d, ok := TelemetryCadence(60); !ok || d != time.Minute {
		t.Errorf("60s read as %v (configured=%v)", d, ok)
	}
	if _, ok := TelemetryCadence(0); ok {
		t.Error("zero should read as the firmware default")
	}
}

// §7.8.3: refuse to run if the channel is already busy. Someone else needs it
// more than we need the measurement.
func TestSurveyRefusesABusyChannel(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 40) // 40% ambient

	_, err := runVirtual(t, node, Config{
		Baseline: 10 * time.Minute, Load: 10 * time.Minute, Sample: time.Minute,
		HopLimits: []uint32{3},
	}, clk)
	if !errors.Is(err, ErrChannelBusy) {
		t.Fatalf("err = %v, want ErrChannelBusy", err)
	}
}

// And abort if it gets busy mid-run, for the same reason.
func TestSurveyAbortsWhenTheChannelGetsBusier(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 2)
	start := clk.Now()
	node.ambientF = func(now time.Time) float64 {
		if now.Sub(start) > 12*time.Minute {
			return 45 // someone else started using the mesh
		}
		return 2
	}

	_, err := runVirtual(t, node, Config{
		Baseline: 10 * time.Minute, Load: 20 * time.Minute, Sample: time.Minute,
		HopLimits: []uint32{3},
	}, clk)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
}

// A load that does not rise above the channel's own noise must not produce a
// confident number. Inventing one is worse than reporting nothing, because the
// governor would be tuned to it.
func TestNoisyChannelYieldsAnUpperBoundNotAnAnswer(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 5)
	node.noise = 8 // swamps a 1% load

	rep, err := runVirtual(t, node, Config{
		Baseline: 15 * time.Minute, Load: 15 * time.Minute, Sample: time.Minute,
		HopLimits: []uint32{3}, TargetDuty: 0.5,
	}, clk)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rep.Best(); ok {
		t.Error("reported a confident R from a load buried in noise")
	}

	var sb strings.Builder
	rep.Write(&sb, 50)
	if !strings.Contains(sb.String(), "No usable R was measured") {
		t.Errorf("report claims a result it does not have:\n%s", sb.String())
	}
}

// The report's job is not to state R but to say what R means for this instance.
func TestBudgetMatchesTheDesignsWorkedExample(t *testing.T) {
	// §7.8.3: "at R = 5.2 and a 5% mesh ceiling shared 50 ways, your instance
	// can originate about 8 full packets/day".
	b := DeriveBudget(airtime.LongFast, 5.2, 50, MeshCeilingPercent)
	if b.FullPackets < 7 || b.FullPackets > 9 {
		t.Errorf("full packets/day = %.1f, design says about 8", b.FullPackets)
	}
	if b.ChannelSecondsPerDay < 80 || b.ChannelSecondsPerDay > 90 {
		t.Errorf("channel seconds/day = %.1f, expected ~86", b.ChannelSecondsPerDay)
	}
	// Doubling R must halve the allowance: the linear scaling §14 warns about.
	half := DeriveBudget(airtime.LongFast, 10.4, 50, MeshCeilingPercent)
	if math.Abs(half.FullPackets*2-b.FullPackets) > 0.5 {
		t.Errorf("doubling R gave %.2f packets, not half of %.2f", half.FullPackets, b.FullPackets)
	}
}

// A two-node mesh cannot measure R, and the report must say so rather than
// letting a sysop act on a number that means nothing.
func TestReportWarnsAboutATwoNodeMesh(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 2, 1)
	go func() {
		node.heard <- Heard{From: 0xB0B, ID: 1, HopStart: 3, HopLimit: 3, At: clk.Now(), Portnum: "TEXT_MESSAGE_APP"}
	}()

	rep, err := runVirtual(t, node, Config{
		Baseline: 10 * time.Minute, Load: 10 * time.Minute, Sample: time.Minute,
		HopLimits: []uint32{3}, TargetDuty: 1.5,
	}, clk)
	if err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	rep.Write(&sb, 50)
	if !strings.Contains(sb.String(), "only one other radio") {
		t.Errorf("report does not warn about a two-node mesh:\n%s", sb.String())
	}
}

func TestCensusCountsDirectAndRelayed(t *testing.T) {
	c := newCensus()
	now := time.Unix(1_800_000_000, 0)
	c.add(Heard{From: 1, HopStart: 3, HopLimit: 3, SNR: 5, At: now, Portnum: "A"})  // direct
	c.add(Heard{From: 1, HopStart: 3, HopLimit: 2, SNR: -2, At: now, Portnum: "A"}) // 1 hop
	c.add(Heard{From: 2, HopStart: 0, HopLimit: 0, At: now, Portnum: "B"})          // unknown

	got := c.summarise()
	if got.Packets != 3 || got.DirectCount != 1 || got.RelayedCount != 1 || got.UnknownHops != 1 {
		t.Errorf("census = %+v", got)
	}
	if len(got.Neighbours) != 2 || got.Neighbours[0].Radio != 1 {
		t.Errorf("neighbours = %+v", got.Neighbours)
	}
	if got.Neighbours[0].BestSNR != 5 || got.Neighbours[0].WorstSNR != -2 {
		t.Errorf("SNR range not tracked: %+v", got.Neighbours[0])
	}
}

// A mesh that gets busier while we transmit inflates R, and the survey has to
// say so.
//
// This is the failure the closing baseline exists for and the one most likely
// to happen in practice: ambient traffic climbs through the evening as people
// get home, every point it climbs by is attributed to rebroadcasts of our own
// packets, and nothing in a single-baseline run can tell the two apart. The
// AbortRise guard does not catch it — that trips at 15 points, and half a point
// against a one-point load is already a doubled answer.
func TestDriftIsMeasuredAndFlagged(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 2, 2.0)

	// Ambient climbs steadily for the whole run: +1.5 points between the two
	// baselines' midpoints.
	const rate = 6.0 / 2400.0
	node.ambientF = func(now time.Time) float64 {
		return 2.0 + rate*now.Sub(time.Unix(0, 0)).Seconds()
	}

	rep, err := runVirtual(t, node, Config{
		Baseline:   20 * time.Minute,
		Load:       20 * time.Minute,
		Sample:     time.Minute,
		HopLimits:  []uint32{3},
		TargetDuty: 1.5,
	}, clk)
	if err != nil {
		t.Fatal(err)
	}

	drift, ok := rep.Drift()
	if !ok {
		t.Fatal("no closing baseline was measured")
	}
	if drift <= 0 {
		t.Fatalf("ambient rose through the run but drift = %+.2f", drift)
	}

	var flagged bool
	for _, n := range rep.Notes {
		if strings.Contains(n, "busier over this run") {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("drift of %+.2f points was not flagged in the notes:\n%v", drift, rep.Notes)
	}

	// The conservative floor must not exceed the raw difference, or the two
	// ends of the range have swapped and the report reads as nonsense.
	raw, _ := rep.DriftRaw()
	if drift > raw {
		t.Errorf("drift floor %+.2f exceeds raw drift %+.2f", drift, raw)
	}

	// And the shareable report must carry it, since that is the artefact a
	// sysop sends to someone else who cannot see these notes.
	var sb strings.Builder
	rep.Write(&sb, DefaultInstanceCount)
	if !strings.Contains(sb.String(), "closing") {
		t.Errorf("report does not show the closing baseline:\n%s", sb.String())
	}
}

// A steady mesh must NOT be flagged, or the warning becomes noise everyone
// learns to skip past.
func TestSteadyMeshIsNotFlaggedForDrift(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(0, 0))
	node := newFakeNode(clk, 4, 3.0)

	rep, err := runVirtual(t, node, Config{
		Baseline:   20 * time.Minute,
		Load:       20 * time.Minute,
		Sample:     time.Minute,
		HopLimits:  []uint32{3},
		TargetDuty: 1.5,
	}, clk)
	if err != nil {
		t.Fatal(err)
	}

	drift, ok := rep.Drift()
	if !ok {
		t.Fatal("no closing baseline was measured")
	}
	if math.Abs(drift) > 0.2 {
		t.Errorf("steady mesh drifted %+.2f points", drift)
	}
	for _, n := range rep.Notes {
		if strings.Contains(n, "over this run") {
			t.Errorf("steady mesh was flagged for drift: %q", n)
		}
	}
}
