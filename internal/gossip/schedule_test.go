package gossip

import (
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/rng"
)

// The reference digest: ten areas, one mesh packet.
const refDigest = 103

// §7.3's whole argument is that a 30-minute digest interval at 50 nodes
// consumes ~11% of the channel — more than the entire 5% budget. Reproduce that
// number, because every mitigation in the section is justified by it.
func TestTheDigestStormIsReal(t *testing.T) {
	c := DefaultSchedule()

	for _, tc := range []struct {
		peers int
		want  float64 // design's table
	}{
		{5, 0.011}, {20, 0.045}, {50, 0.112},
	} {
		got := c.ControlUtilisation(tc.peers, refDigest, 30*time.Minute)
		t.Logf("%2d peers at a 30-minute interval: %.1f%% of the channel (design says %.1f%%)",
			tc.peers, got*100, tc.want*100)
		// Within 20% of the design's figure; it used a slightly different
		// airtime-per-byte, so an exact match would be a coincidence.
		if got < tc.want*0.8 || got > tc.want*1.2 {
			t.Errorf("%d peers: computed %.1f%% utilisation, design's table says %.1f%%",
				tc.peers, got*100, tc.want*100)
		}
	}

	// The headline: at 50 nodes this exceeds the entire §1.1 airtime budget.
	if got := c.ControlUtilisation(50, refDigest, 30*time.Minute); got <= 0.05 {
		t.Errorf("50 nodes at 30 minutes computed as %.1f%%, which would not exceed the 5%% budget "+
			"— the premise of every mitigation in §7.3", got*100)
	}
}

// The clamp is the mitigation that actually binds. Whatever interval it
// produces must hold control traffic inside the configured share.
func TestIntervalRespectsTheAirtimeBudget(t *testing.T) {
	c := DefaultSchedule()

	for _, peers := range []int{1, 5, 10, 20, 50, 100} {
		interval := c.Interval(peers, refDigest)
		util := c.ControlUtilisation(peers, refDigest, interval)
		t.Logf("%3d peers → %-8s interval, %.2f%% of the channel",
			peers, interval.Round(time.Minute), util*100)

		if util > c.ControlShare*1.001 { // rounding slack only
			t.Errorf("%d peers: interval %s allows %.2f%% control traffic, over the %.2f%% budget",
				peers, interval, util*100, c.ControlShare*100)
		}
	}
}

// §7.3's prose claimed "around 2-3 hours" at 50 nodes. It is not: the section's
// own utilisation table contradicts it, and 1% of the channel buys about five
// hours. This test pins the real number so the prose cannot drift back.
func TestFiftyNodeIntervalIsAboutFiveHours(t *testing.T) {
	c := DefaultSchedule()
	got := c.Interval(50, refDigest)

	if got < 4*time.Hour || got > 6*time.Hour {
		t.Errorf("50 peers gives a %s interval; expected roughly 5 hours at a 1%% control share", got)
	}
	// And confirm the design's stated 2-3 hours would have blown the budget.
	if util := c.ControlUtilisation(50, refDigest, 2*time.Hour); util <= c.ControlShare {
		t.Errorf("a 2-hour interval at 50 peers uses %.2f%%, which is inside the %.2f%% budget "+
			"— then the prose was right and this test is wrong", util*100, c.ControlShare*100)
	} else {
		t.Logf("a 2-hour interval at 50 peers would use %.2f%%, %.1fx the %.2f%% budget",
			util*100, util/c.ControlShare, c.ControlShare*100)
	}
}

// Both §7.3 rules are linear in peer count, so one of them is nearly always
// redundant. Worth knowing which, because only the clamp has physical meaning.
func TestTheHeuristicIsAlmostAlwaysRedundant(t *testing.T) {
	c := DefaultSchedule()
	heuristicWins := 0
	for _, peers := range []int{1, 2, 5, 10, 20, 50, 100, 200} {
		perPeerHeuristic := float64(c.Base) / float64(c.PeerDivisor)
		perPeerClamp := float64(c.DigestAirtime(refDigest)) / c.ControlShare
		if perPeerHeuristic > perPeerClamp {
			heuristicWins++
		}
		_ = peers
	}
	perPeerHeuristic := time.Duration(float64(c.Base) / float64(c.PeerDivisor))
	perPeerClamp := time.Duration(float64(c.DigestAirtime(refDigest)) / c.ControlShare)
	t.Logf("per peer: heuristic contributes %s, airtime clamp requires %s",
		perPeerHeuristic.Round(time.Second), perPeerClamp.Round(time.Second))

	// Not an error either way — the point is that the two are within a whisker
	// of each other, so the interval is effectively set by the clamp alone and
	// small changes in digest size decide the outcome.
	ratio := float64(perPeerClamp) / float64(perPeerHeuristic)
	if ratio < 0.5 || ratio > 2 {
		t.Errorf("the two §7.3 rules differ by %.1fx; one now dominates completely "+
			"and the doc's claim that both are needed should be revisited", ratio)
	}
}

// A small mesh must not be slowed to a large mesh's rhythm.
func TestSmallMeshStaysResponsive(t *testing.T) {
	c := DefaultSchedule()
	small := c.Interval(3, refDigest)
	large := c.Interval(50, refDigest)
	if small >= large {
		t.Errorf("3 peers gives %s and 50 gives %s; the interval is not scaling", small, large)
	}
	if small > time.Hour {
		t.Errorf("a three-peer mesh waits %s between digests, which is far too slow", small)
	}
}

// Without a cap, a large mesh pushes the heartbeat out far enough that a
// returning node waits days for the safety net that would heal it.
func TestMaxIntervalCaps(t *testing.T) {
	c := DefaultSchedule()
	if got := c.Interval(100000, refDigest); got != c.MaxInterval {
		t.Errorf("an absurd peer count gave %s, expected the %s cap", got, c.MaxInterval)
	}
}

func newTestScheduler(t *testing.T, peers int) (*Scheduler, *clock.Virtual) {
	t.Helper()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0).UTC())
	return NewScheduler(DefaultSchedule(), clk, rng.NewSeeded(1), func() int { return peers }), clk
}

func TestSchedulerFiresOnInterval(t *testing.T) {
	s, clk := newTestScheduler(t, 5)

	if due, why := s.Due(); due {
		t.Fatalf("fired immediately; %q", why)
	}
	clk.Advance(2 * time.Hour)
	if due, why := s.Due(); !due {
		t.Fatalf("did not fire after two hours: %s", why)
	}
	s.MarkSent(refDigest)
	if due, _ := s.Due(); due {
		t.Fatal("fired again immediately after sending")
	}
}

// Mitigation 4: a digest that would carry no information is not sent.
func TestSuppressionWhenAPeerAlreadySaidIt(t *testing.T) {
	s, clk := newTestScheduler(t, 5)
	clk.Advance(3 * time.Hour)

	s.NoteHeard(true) // a peer announced state matching ours
	due, why := s.Due()
	if due {
		t.Fatal("sent a digest that would have carried no information")
	}
	if why == "" {
		t.Error("suppression gave no reason")
	}
	if _, suppressed, _ := s.Stats(); suppressed != 1 {
		t.Errorf("suppression count is %d, want 1", suppressed)
	}
}

// The converse, and the more important half: a peer that DIFFERS from us must
// not suppress our digest. Suppressing on disagreement would let two diverged
// nodes fall silent about their disagreement — permanent divergence, and
// silent.
func TestADisagreeingPeerDoesNotSuppress(t *testing.T) {
	s, clk := newTestScheduler(t, 5)
	clk.Advance(3 * time.Hour)

	s.NoteHeard(false)
	if due, why := s.Due(); !due {
		t.Fatalf("a peer that disagreed with us suppressed our digest: %s", why)
	}
}

// Mitigation 3: a node with normal traffic has already announced its state, so
// the standalone digest is genuinely just the idle-node heartbeat.
func TestPiggybackingDefersTheStandaloneDigest(t *testing.T) {
	s, clk := newTestScheduler(t, 5)

	// Piggyback repeatedly, each time just before the beat would fire.
	for i := 0; i < 10; i++ {
		clk.Advance(20 * time.Minute)
		s.NoteTransmitted(refDigest)
		if due, _ := s.Due(); due {
			t.Fatalf("iteration %d: sent a standalone digest despite recent piggybacked traffic", i)
		}
	}
	if sent, _, _ := s.Stats(); sent != 0 {
		t.Errorf("a busy node sent %d standalone digests; it should send none", sent)
	}
}

// Jitter must be symmetric. Asymmetric jitter shifts the mean interval, which
// silently invalidates the airtime budget the clamp just computed.
func TestJitterIsSymmetricAroundTheInterval(t *testing.T) {
	cfg := DefaultSchedule()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0).UTC())
	rnd := rng.NewSeeded(42)

	want := cfg.Interval(5, refDigest)
	var total time.Duration
	const samples = 2000
	var minGap, maxGap time.Duration = 1<<62 - 1, 0

	for i := 0; i < samples; i++ {
		s := NewScheduler(cfg, clk, rnd, func() int { return 5 })
		_, _, next := s.Stats()
		gap := next.Sub(clk.Now())
		total += gap
		if gap < minGap {
			minGap = gap
		}
		if gap > maxGap {
			maxGap = gap
		}
	}
	mean := total / samples
	drift := float64(mean-want) / float64(want)
	t.Logf("interval %s, jittered mean %s (%.2f%% drift), range %s..%s",
		want.Round(time.Second), mean.Round(time.Second), drift*100,
		minGap.Round(time.Second), maxGap.Round(time.Second))

	if drift < -0.02 || drift > 0.02 {
		t.Errorf("jitter shifted the mean interval by %.2f%%; the airtime budget assumed it would not", drift*100)
	}
	// And it must actually spread, or peers that boot together collide forever.
	if maxGap-minGap < time.Duration(float64(want)*0.2) {
		t.Errorf("jitter spread only %s across %d samples; peers will stay synchronised",
			maxGap-minGap, samples)
	}
}

// Two nodes that boot at the same instant must not stay locked together.
func TestJitterDesynchronisesPeersThatBootTogether(t *testing.T) {
	cfg := DefaultSchedule()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0).UTC())

	a := NewScheduler(cfg, clk, rng.NewSeeded(1), func() int { return 10 })
	b := NewScheduler(cfg, clk, rng.NewSeeded(2), func() int { return 10 })

	_, _, nextA := a.Stats()
	_, _, nextB := b.Stats()
	gap := nextA.Sub(nextB)
	if gap < 0 {
		gap = -gap
	}
	if gap < time.Minute {
		t.Errorf("two nodes booting together scheduled digests %s apart; they will collide", gap)
	}
	t.Logf("two nodes booting together are %s apart", gap.Round(time.Second))
}
