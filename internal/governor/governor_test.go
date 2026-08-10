package governor

import (
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func newTestGovernor(t *testing.T, mut func(*Config)) (*Governor, *clock.Virtual) {
	t.Helper()
	clk := clock.NewVirtual(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC))
	cfg := Config{
		Preset:    airtime.LongFast,
		Region:    "US",
		Instances: DefaultInstanceCount,
		Clock:     clk,
	}
	if mut != nil {
		mut(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g, clk
}

// The number §7.6 states outright: at a 5% ceiling shared 50 ways and R=4, an
// instance gets about eleven full packets a day.
func TestDailyAllowanceMatchesTheDesign(t *testing.T) {
	g, clk := newTestGovernor(t, nil)

	sent := 0
	for i := 0; i < 24*60; i++ {
		clk.Advance(time.Minute)
		for g.Allow(233, ClassForum) {
			sent++
		}
	}
	if sent < 8 || sent > 14 {
		t.Errorf("sent %d full packets in a day, design says about 11", sent)
	}
}

// Cost is charged per PACKET, not per byte. §7.6's stated reason for this is
// backwards — airtime is affine, not superlinear — and the correction matters:
// a byte-denominated budget under-prices small packets so badly that a chatty
// protocol could stay inside it and still take the channel apart.
func TestSmallPacketsAreNotCheap(t *testing.T) {
	one, _ := newTestGovernor(t, nil)
	many, _ := newTestGovernor(t, nil)

	if !one.Allow(233, ClassForum) {
		t.Fatal("a full packet was refused from a full bucket")
	}
	small := 0
	for many.Allow(1, ClassForum) {
		small++
	}
	// A byte budget would allow 233 one-byte packets for the price of one full
	// one. Airtime allows barely more of them than of full packets.
	if small > 20 {
		t.Errorf("%d one-byte packets fit the same budget as one full packet", small)
	}
}

// Every class the classifier can produce needs a reserve entry.
//
// A missing one reads as 0.0, which is ClassControl's share — so a class added
// as a new BOTTOM rung would silently become the highest-priority traffic on
// the mesh, and the drain-order test below would still pass because a prefix of
// one is a prefix. This is the check that fails instead.
func TestEveryClassReservesMoreThanTheOneAboveIt(t *testing.T) {
	ladder := []Class{ClassControl, ClassDM, ClassForum, ClassFileCatalog, ClassDoorEvent}
	for i, c := range ladder {
		if _, ok := reserve[c]; !ok {
			t.Errorf("%v has no reserve entry, so it would reserve nothing and "+
				"outrank the roster", c)
			continue
		}
		if i > 0 && !(reserve[c] > reserve[ladder[i-1]]) {
			t.Errorf("%v reserves %.2f and %v reserves %.2f: the lower class must give way first",
				ladder[i-1], reserve[ladder[i-1]], c, reserve[c])
		}
	}
}

// Under backpressure, drop from the bottom: the last of the budget belongs to
// the traffic that keeps the federation converging.
func TestClassesFallOutInPriorityOrder(t *testing.T) {
	g, _ := newTestGovernor(t, nil)

	// Drain most of the bucket with control traffic, which reserves nothing.
	for i := 0; i < 100 && g.Allow(233, ClassControl); i++ {
	}

	if g.Allow(233, ClassFileCatalog) {
		t.Error("a file catalog got the last of the budget")
	}
	if g.Allow(233, ClassForum) {
		t.Error("a forum post got the last of the budget")
	}

	// The stronger property: at EVERY level of the bucket, the set of allowed
	// classes is a prefix of the priority order. Classes never come back before
	// their betters, whatever the numbers happen to be.
	//
	// Asserting the invariant rather than a threshold matters here: the reserve
	// fractions are tuning, and tuning should be free to change without a test
	// having to be rewritten to match it.
	g2, clk := newTestGovernor(t, nil)
	ladder := []Class{ClassControl, ClassDM, ClassForum, ClassFileCatalog, ClassDoorEvent}

	for step := 0; step < 60; step++ {
		var allowed []bool
		for _, c := range ladder {
			allowed = append(allowed, g2.Would(100, c))
		}
		for i := 1; i < len(allowed); i++ {
			if allowed[i] && !allowed[i-1] {
				t.Fatalf("at %d%% of the bucket, %s is allowed but %s is not",
					int(100*float64(g2.tokens)/float64(g2.capacity)), ladder[i], ladder[i-1])
			}
		}
		if !g2.Allow(100, ClassControl) {
			clk.Advance(30 * time.Minute)
		}
	}
}

// §7.6: above ~40% channel utilization, transmit nothing but DMs.
func TestBusyChannelSilencesEverythingButDMs(t *testing.T) {
	g, _ := newTestGovernor(t, nil)
	g.Observe(45, 0.1)

	if g.Allow(100, ClassControl) {
		t.Error("control traffic went out on a 45% busy channel")
	}
	if g.Allow(100, ClassForum) {
		t.Error("a forum post went out on a 45% busy channel")
	}
	if !g.Allow(100, ClassDM) {
		t.Error("a DM was refused; §7.6 keeps DM traffic flowing")
	}
	if !g.Budget().Backpressure {
		t.Error("Budget() should report backpressure on a busy channel")
	}
}

// Above ~25%, back off exponentially: the refill rate falls with each
// consecutive busy reading and recovers when the channel clears.
func TestBackoffIsExponentialAndRecovers(t *testing.T) {
	g, clk := newTestGovernor(t, nil)

	// Drain, then accrue for an hour on a quiet channel.
	for g.Allow(233, ClassControl) {
	}
	clk.Advance(time.Hour)
	quiet := g.Budget().Available

	// Same again, but busy throughout.
	g2, clk2 := newTestGovernor(t, nil)
	for g2.Allow(233, ClassControl) {
	}
	for i := 0; i < 5; i++ {
		g2.Observe(30, 0.1)
	}
	clk2.Advance(time.Hour)
	busy := g2.Budget().Available

	if busy >= quiet {
		t.Errorf("a busy channel accrued %v, a quiet one %v — backoff did nothing", busy, quiet)
	}

	// Recovery: the penalty halves back toward 1.
	for i := 0; i < 10; i++ {
		g2.Observe(2, 0.1)
	}
	if g2.penalty != 1 {
		t.Errorf("penalty stuck at %.1f after the channel cleared", g2.penalty)
	}
}

// EU regions are limited to 10% duty over a rolling hour, and §7.6 says to
// track it locally rather than let the firmware reject us.
func TestRegionalDutyCycleIsTracked(t *testing.T) {
	// A huge ceiling so the token bucket cannot be what refuses.
	g, clk := newTestGovernor(t, func(c *Config) {
		c.Region = "EU_868"
		c.CeilingPercent = MaxCeilingPercent
		c.Instances = 1
		c.Burst = time.Hour
	})

	var air time.Duration
	sent := 0
	for i := 0; i < 10_000; i++ {
		if !g.Allow(233, ClassControl) {
			break
		}
		air += airtime.LongFast.Packet(233)
		sent++
		clk.Advance(time.Second)
	}
	if air > 6*time.Minute+time.Second {
		t.Errorf("transmitted %v in an hour, over the 10%% EU limit", air)
	}
	if sent == 0 {
		t.Fatal("nothing was allowed at all")
	}
	if g.Stats().RefusedDuty == 0 {
		t.Error("the duty-cycle limiter never engaged")
	}

	// A region without a regulatory duty cycle is not limited this way.
	us, usClk := newTestGovernor(t, func(c *Config) {
		c.Region = "US"
		c.CeilingPercent = MaxCeilingPercent
		c.Instances = 1
		c.Burst = time.Hour
	})
	usAir := time.Duration(0)
	for i := 0; i < 10_000; i++ {
		if !us.Allow(233, ClassControl) {
			break
		}
		usAir += airtime.LongFast.Packet(233)
		usClk.Advance(time.Second)
	}
	if usAir <= 6*time.Minute {
		t.Errorf("US was limited to %v, as though it had an EU duty cycle", usAir)
	}
}

func TestQuietHoursSilenceEverything(t *testing.T) {
	g, clk := newTestGovernor(t, func(c *Config) {
		// 22:00 to 06:00, wrapping midnight.
		c.QuietHours = []Window{{Start: 22 * time.Hour, End: 6 * time.Hour}}
	})

	if !g.Allow(100, ClassControl) {
		t.Fatal("refused at midday")
	}
	clk.Set(time.Date(2026, 7, 27, 23, 30, 0, 0, time.UTC))
	if g.Allow(100, ClassControl) {
		t.Error("transmitted during quiet hours")
	}
	clk.Set(time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC))
	if g.Allow(100, ClassDM) {
		t.Error("quiet hours are absolute; a DM went out at 03:00")
	}
	clk.Set(time.Date(2026, 7, 28, 7, 0, 0, 0, time.UTC))
	if !g.Allow(100, ClassControl) {
		t.Error("still silent after quiet hours ended")
	}
}

func TestWindowWrapsMidnight(t *testing.T) {
	w := Window{Start: 22 * time.Hour, End: 6 * time.Hour}
	at := func(h, m int) time.Time { return time.Date(2026, 7, 27, h, m, 0, 0, time.UTC) }

	for _, in := range []time.Time{at(22, 0), at(23, 59), at(0, 0), at(5, 59)} {
		if !w.Contains(in) {
			t.Errorf("%s should be inside the window", in.Format("15:04"))
		}
	}
	for _, out := range []time.Time{at(6, 0), at(12, 0), at(21, 59)} {
		if w.Contains(out) {
			t.Errorf("%s should be outside the window", out.Format("15:04"))
		}
	}
}

// The ceiling is a promise to other people's meshes, so it is enforced in code
// rather than documented and hoped for.
func TestCeilingIsClampedInCode(t *testing.T) {
	g, _ := newTestGovernor(t, func(c *Config) { c.CeilingPercent = 60 })
	if g.cfg.CeilingPercent != MaxCeilingPercent {
		t.Errorf("ceiling = %.1f, want it clamped to %.1f", g.cfg.CeilingPercent, MaxCeilingPercent)
	}
	// Clamped, not rejected: an instance that refuses to start tempts a sysop
	// to remove the governor rather than fix the number.
	if g.Budget().PerDatagram == 0 {
		t.Error("a clamped governor should still work")
	}
}

// The allocation is the ceiling divided by the network size, which the node
// learns from the NODE roster as it grows.
func TestMoreInstancesMeansASmallerShare(t *testing.T) {
	g, _ := newTestGovernor(t, func(c *Config) { c.Instances = 10 })
	before := g.shareFraction()

	g.SetInstances(100)
	if after := g.shareFraction(); after >= before {
		t.Errorf("share did not shrink as the roster grew: %v then %v", before, after)
	}
	// And the bucket cannot hold more than the new capacity.
	if g.tokens > g.capacity {
		t.Error("tokens exceed capacity after the share shrank")
	}
}

// R is observed, never assumed downward: what a node can see is a floor,
// because firmware deduplicates most repeats before they reach us.
func TestREstimateOnlyRevisesUpward(t *testing.T) {
	g, clk := newTestGovernor(t, func(c *Config) { c.Burst = time.Hour })

	for i := 0; i < 40; i++ {
		clk.Advance(time.Minute)
		g.Allow(100, ClassControl)
	}
	start := g.R()

	// Few echoes: the implied R is below the default, and must not lower it.
	for i := 0; i < 3; i++ {
		g.NoteEcho()
	}
	if g.R() < start {
		t.Errorf("R fell to %.2f from %.2f on thin evidence", g.R(), start)
	}

	// Many echoes: R must climb.
	for i := 0; i < 400; i++ {
		g.NoteEcho()
	}
	if g.R() <= start {
		t.Errorf("R = %.2f after heavy rebroadcast; it should have climbed from %.2f", g.R(), start)
	}
}

func TestRCanBePinned(t *testing.T) {
	g, clk := newTestGovernor(t, func(c *Config) {
		c.FloodMultiplier = 7
		c.FloodMultiplierOverride = true
	})
	for i := 0; i < 40; i++ {
		clk.Advance(time.Minute)
		g.Allow(100, ClassControl)
	}
	for i := 0; i < 500; i++ {
		g.NoteEcho()
	}
	if g.R() != 7 {
		t.Errorf("R = %.1f despite being pinned", g.R())
	}
}

// A rogue or malfunctioning peer must not be able to flood us, and §7.6 says
// the sysop hears about it.
func TestPerPeerInboundQuota(t *testing.T) {
	var alerts []string
	g, clk := newTestGovernor(t, func(c *Config) {
		c.InboundQuotaPerHour = 1000
		c.OnAlert = func(s string) { alerts = append(alerts, s) }
	})

	peer := identity.NodeIDFromPublicKey(mustKey(t, 1))
	other := identity.NodeIDFromPublicKey(mustKey(t, 2))

	for i := 0; i < 10; i++ {
		if !g.NoteInbound(peer, 100) {
			t.Fatalf("refused inside quota at %d", i)
		}
	}
	if g.NoteInbound(peer, 1) {
		t.Error("accepted traffic over quota")
	}
	if len(alerts) != 1 {
		t.Errorf("alerts = %d, want exactly one per offending peer", len(alerts))
	}
	// One noisy peer must not silence a well-behaved one.
	if !g.NoteInbound(other, 100) {
		t.Error("a different peer was penalised for the first one's flood")
	}
	// The window rolls.
	clk.Advance(2 * time.Hour)
	if !g.NoteInbound(peer, 100) {
		t.Error("the quota window never rolled over")
	}
}

// "Sysops must not have to compute this."
func TestExplainIsInHumanTerms(t *testing.T) {
	g, _ := newTestGovernor(t, nil)
	s := g.Explain()

	for _, want := range []string{"full packets", "short posts", "R="} {
		if !strings.Contains(s, want) {
			t.Errorf("Explain() is missing %q: %s", want, s)
		}
	}
	// It must say whether R is a guess, because acting on an unmeasured number
	// is different from acting on a measured one.
	if !strings.Contains(s, "unmeasured") {
		t.Errorf("Explain() does not flag R as unmeasured: %s", s)
	}
}

// A governor that has just started must be able to announce itself; starting
// empty would leave a fresh instance unreachable for hours.
func TestStartsWithBudget(t *testing.T) {
	g, _ := newTestGovernor(t, nil)
	if !g.Allow(105, ClassControl) {
		t.Error("a freshly started node could not send its first packet")
	}
}

func mustKey(t *testing.T, seed uint64) []byte {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k.Public
}
