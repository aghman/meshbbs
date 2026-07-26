package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// This file asserts the properties that must hold at EVERY instant of a
// simulated run, not merely at the end of one (design §12.3).
//
// The distinction is the whole point. A final check catches states that survive
// to the end; the bugs that actually bite are transient. A version vector that
// briefly runs ahead of the records behind it, a record replaced and replaced
// back, a burst that blows the airtime budget and is then amortised away — all
// of them are invisible to an assertion at the finish line, and all of them are
// real defects. Between two events the federation is quiescent and
// single-threaded, so that gap is the only place a consistent global snapshot
// of fifty nodes exists at all.
//
// TestTheInvariantCheckerHasTeeth deliberately violates each rule and confirms
// it is caught. That test exists because a checker nobody has seen fail is
// indistinguishable from a checker that cannot fail — a lesson this repo
// learned the expensive way when a canonicality fuzz target turned out to be
// structurally unable to detect the bug class it was written for.

// nodeSnapshot is what a node looked like after the previous event.
type nodeSnapshot struct {
	// slot maps (origin, seq) to the record ID filed there. Records are
	// immutable, so once a slot is occupied its ID must never change.
	slot map[slotKey]record.ID
	// high is the per-origin contiguous high-water mark, which must never
	// decrease.
	high map[identity.NodeID]uint64
	// count is the number of records held, which must never decrease.
	count int
}

type slotKey struct {
	origin identity.NodeID
	seq    uint64
}

// invariantChecker audits the whole federation after every event.
type invariantChecker struct {
	t    *testing.T
	f    *federation
	prev map[identity.NodeID]*nodeSnapshot
	// lastDeep is when each node last had an expensive audit.
	lastDeep map[identity.NodeID]time.Time

	// onViolation reports a breach. It defaults to t.Errorf and is replaced in
	// TestTheInvariantCheckerHasTeeth so a deliberately induced violation can be
	// asserted on rather than failing the test that induced it.
	//
	// A function rather than a substitute *testing.T because Go has no virtual
	// dispatch on concrete types: embedding *testing.T and shadowing Errorf does
	// not intercept anything the checker calls.
	onViolation func(string)

	// checks counts audits performed, so a test can prove the checker actually
	// ran rather than silently doing nothing.
	checks int
	// deepChecks counts the expensive audits, so the sampling can be seen to be
	// doing real work rather than skipping everything.
	deepChecks int
	// peakShare is the highest windowed utilisation observed, reported so a
	// passing run still shows how much headroom it had.
	peakShare float64
	// failed stops the flood after the first violation: one root cause
	// typically violates the same invariant on every subsequent event, and
	// thousands of identical failures bury the first.
	failed bool

	// maxAirtimeShare bounds channel utilisation over a rolling window. Zero
	// disables the check.
	maxAirtimeShare float64
	// airtimeWindow is the width of that window.
	//
	// A ROLLING window, not cumulative-from-zero, because cumulative is the
	// wrong measurement twice over. Early in a run it is dominated by startup —
	// a normal publish burst in the first ten minutes reads as a huge share of
	// ten minutes — and late in a run it is dominated by history, so a thirty-day
	// soak would average away a day of pathological chatter and report nothing.
	// What §1.1 actually constrains is sustained load on a shared band, which is
	// what a trailing window measures. An hour also matches how regulatory duty
	// cycles are defined.
	airtimeWindow time.Duration
	// samples are (time, cumulative airtime) points, pruned to the window.
	samples []airtimeSample
}

type airtimeSample struct {
	at      time.Time
	airtime time.Duration
}

func newInvariantChecker(t *testing.T, f *federation) *invariantChecker {
	t.Helper()
	c := &invariantChecker{
		t:             t,
		f:             f,
		prev:          map[identity.NodeID]*nodeSnapshot{},
		lastDeep:      map[identity.NodeID]time.Time{},
		airtimeWindow: time.Hour,
	}
	c.onViolation = func(msg string) { t.Error(msg) }
	f.net.AfterEach(c.check)
	return c
}

// fail reports the first violation and silences the rest.
func (c *invariantChecker) fail(format string, args ...any) {
	if c.failed {
		return
	}
	c.failed = true
	c.onViolation(fmt.Sprintf("invariant violated at t+%s: %s",
		c.f.net.Now().Sub(c.f.net.cfg.Start).Round(time.Millisecond),
		fmt.Sprintf(format, args...)))
}

func (c *invariantChecker) check() {
	if c.failed {
		return
	}
	c.checks++

	for _, n := range c.f.nodes {
		c.checkNode(n)
	}
	c.checkAirtime()
}

// deepInterval bounds how much SIMULATED TIME may pass before the expensive
// checks run on a node that appears unchanged.
//
// The cheap checks — record count and advertised high-water marks — run after
// every single event and catch anything that changes the shape of a node's
// state. The expensive ones re-verify every signature and re-parse every record
// against its signed bytes, which is an Ed25519 verification per record per
// node. Run unconditionally over a seven-simulated-day scenario that is roughly
// 8.5 MILLION verifications and ten minutes of wall clock, which means nobody
// runs the long simulations, which are the ones that find the bugs.
//
// So a deep audit happens whenever a node's state actually changed, plus a
// periodic sweep to catch in-place mutation — which changes neither count nor
// vector, and is exactly what a tampered record looks like.
//
// The sweep is keyed to simulated time rather than an event count on purpose.
// An event-count sweep scales with how chatty a scenario is, so a quiet
// thirty-day soak gets audited as often as a busy one-hour test and pays for it
// in wall clock. Keyed to time, cost scales with what is being simulated: the
// seven-day backfill scenario went from 95,027 deep audits to a few hundred.
const deepInterval = time.Hour

func (c *invariantChecker) checkNode(n *node) {
	area := c.f.area

	// Cheap pass, every event.
	count := n.store.Total()
	vec := n.store.Vector(area)

	prev := c.prev[n.id]
	changed := prev == nil || prev.count != count

	if prev != nil {
		// MONOTONICITY: a node never forgets.
		//
		// Anti-entropy's convergence argument rests on state only ever growing.
		// If a node can lose a record it already held, two peers can hand it
		// back and forth forever, and every "converged" assertion elsewhere is
		// measuring a moment rather than a fixed point.
		if count < prev.count {
			c.fail("node %s went from %d records to %d — a node must never forget",
				n.id.Short(), prev.count, count)
			return
		}
		for origin, was := range prev.high {
			if now := vec.Get(origin); now < was {
				c.fail("node %s: high-water mark for %s went backwards, %d to %d",
					n.id.Short(), origin.Short(), was, now)
				return
			}
			if vec.Get(origin) != was {
				changed = true
			}
		}
	}

	high := make(map[identity.NodeID]uint64, vec.Len())
	for _, origin := range vec.Origins() {
		high[origin] = vec.Get(origin)
		if prev != nil {
			if _, known := prev.high[origin]; !known {
				changed = true
			}
		}
	}

	deep := changed || c.f.net.Now().Sub(c.lastDeep[n.id]) >= deepInterval
	if !deep {
		c.prev[n.id] = &nodeSnapshot{slot: prev.slot, high: high, count: count}
		return
	}
	c.deepChecks++
	c.lastDeep[n.id] = c.f.net.Now()

	entries := n.store.Entries(area)
	slot := make(map[slotKey]record.ID, len(entries))
	for _, e := range entries {
		slot[slotKey{e.Origin, e.Seq}] = e.ID
	}

	if prev != nil {
		// IMMUTABILITY: an occupied slot never changes its occupant.
		//
		// A record is content-addressed and signed (§6.2), so (origin, seq)
		// names exactly one record forever. A different ID in an occupied slot
		// means either equivocation was accepted or something rewrote history —
		// and note the record COUNT is unchanged in that case, so the
		// monotonicity check above cannot see it.
		for key, wasID := range prev.slot {
			nowID, still := slot[key]
			if !still {
				c.fail("node %s: record %s at (%s, %d) disappeared",
					n.id.Short(), wasID, key.origin.Short(), key.seq)
				return
			}
			if nowID != wasID {
				c.fail("node %s: slot (%s, %d) changed from record %s to %s — "+
					"records are immutable, so this is accepted equivocation or a rewrite",
					n.id.Short(), key.origin.Short(), key.seq, wasID, nowID)
				return
			}
		}
	}

	// VECTOR HONESTY: the advertised high-water mark matches the log.
	//
	// The vector is maintained incrementally by Apply while the records are the
	// ground truth, so the two can drift. A vector claiming more than the log
	// holds makes every peer skip records it needs and believe it is up to date;
	// one claiming less causes endless redundant transfers. Both look like a
	// flaky radio from the outside.
	for origin, claimed := range high {
		if actual := n.store.ContiguousHighWater(area, origin); claimed != actual {
			c.fail("node %s: vector claims %s is contiguous to %d, but the log is contiguous to %d",
				n.id.Short(), origin.Short(), claimed, actual)
			return
		}
	}

	// SIGNATURE INTEGRITY: everything held still verifies, and every parsed
	// field still matches the bytes that were signed.
	//
	// Deliberately redundant with Apply's admission check, because it tests a
	// different thing: that nothing was mutated AFTER admission. An invariant
	// covering only the admission path cannot distinguish "we never accepted a
	// forgery" from "we accepted one and quietly rewrote it".
	if err := n.store.VerifyAll(area); err != nil {
		c.fail("node %s: %v", n.id.Short(), err)
		return
	}

	c.prev[n.id] = &nodeSnapshot{slot: slot, high: high, count: count}
}

// checkAirtime holds sustained load to §1.1's federation share of the channel.
func (c *invariantChecker) checkAirtime() {
	if c.maxAirtimeShare <= 0 {
		return
	}
	now := c.f.net.Now()
	c.samples = append(c.samples, airtimeSample{at: now, airtime: c.f.net.Stats().Airtime})

	// Drop everything older than the window, keeping one sample at or before its
	// start so the delta spans the full width.
	cutoff := now.Add(-c.airtimeWindow)
	keep := 0
	for i, s := range c.samples {
		if s.at.Before(cutoff) {
			keep = i
		} else {
			break
		}
	}
	c.samples = c.samples[keep:]

	oldest := c.samples[0]
	span := now.Sub(oldest.at)
	// Wait for a full window. A partial one has a small denominator, so a single
	// ordinary packet reads as a huge share and the check becomes noise.
	if span < c.airtimeWindow {
		return
	}

	share := float64(c.f.net.Stats().Airtime-oldest.airtime) / float64(span)
	if share > c.peakShare {
		c.peakShare = share
	}
	if share > c.maxAirtimeShare {
		c.fail("channel utilisation over the last %s is %.2f%%, above the %.2f%% federation budget",
			c.airtimeWindow, share*100, c.maxAirtimeShare*100)
	}
}

// report prints what the checker did, so a passing test still shows its work.
func (c *invariantChecker) report() {
	c.t.Helper()
	c.t.Logf("invariants audited after %d events across %d nodes (%d deep audits)",
		c.checks, len(c.f.nodes), c.deepChecks)
	if c.deepChecks == 0 {
		c.t.Error("no deep audit ever ran; signatures were never re-verified")
	}
	if c.maxAirtimeShare > 0 {
		c.t.Logf("peak %s channel utilisation: %.2f%% (budget %.2f%%)",
			c.airtimeWindow, c.peakShare*100, c.maxAirtimeShare*100)
	}
	if c.checks == 0 {
		c.t.Error("the invariant checker never ran; it is not wired to the event loop")
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// The ordinary case, audited continuously rather than at the finish line.
func TestInvariantsHoldThroughoutConvergence(t *testing.T) {
	cfg := DefaultConfig(101)
	cfg.LossRate = 0.20
	f := newFederation(t, cfg, 6, 0)

	c := newInvariantChecker(t, f)
	c.maxAirtimeShare = 0.05 // §1.1's federation budget

	// Twelve records over six hours: two per hour, inside the six-per-hour
	// ceiling TestSustainablePublishRate measures.
	//
	// The rate is deliberate, not arbitrary. This scenario originally published
	// all twelve within the first hour and the airtime invariant failed at
	// 5.40%. That was the invariant doing its job — LongFast cannot carry twelve
	// records an hour inside a 5% budget — so the scenario was corrected to a
	// rate the radio can actually sustain rather than the threshold being raised
	// to accommodate a network that cannot exist.
	for i := 0; i < 12; i++ {
		n := f.nodes[i%len(f.nodes)]
		f.net.After(time.Duration(i+1)*30*time.Minute, func() { f.publish(t, n, 1) })
	}

	ok := f.net.RunUntil(24*time.Hour, f.convergedOn(12))
	c.report()
	if !ok {
		t.Fatal("did not converge within a simulated day")
	}
}

// Partition and heal, audited continuously. Divergence is EXPECTED here — the
// two halves legitimately hold different records — so this pins down that the
// invariants distinguish "temporarily disagreeing" from "broken". A node in a
// partition must still never forget, never rewrite, and never lie in its vector.
func TestInvariantsHoldAcrossAPartition(t *testing.T) {
	cfg := DefaultConfig(103)
	cfg.LossRate = 0.15
	f := newFederation(t, cfg, 6, 0)

	c := newInvariantChecker(t, f)
	c.maxAirtimeShare = 0.05

	for i, n := range f.nodes {
		f.net.Partition(n.id, i%2)
	}
	f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 3) })
	f.net.After(2*time.Minute, func() { f.publish(t, f.nodes[1], 3) })
	f.net.Run(6 * time.Hour)

	f.net.Heal()
	ok := f.net.RunUntil(24*time.Hour, f.convergedOn(6))
	c.report()
	if !ok {
		t.Fatal("a healed partition did not converge within a simulated day")
	}
}

// A node returning after a week is where monotonicity and vector honesty are
// most likely to break: it receives a large backfill out of order, so its
// contiguous high-water mark advances in jumps while records arrive in gaps.
func TestInvariantsHoldWhileBackfillingAReturningNode(t *testing.T) {
	cfg := DefaultConfig(107)
	cfg.LossRate = 0.15
	f := newFederation(t, cfg, 5, 0)

	c := newInvariantChecker(t, f)
	c.maxAirtimeShare = 0.05

	absent := f.nodes[4]
	f.net.SetUp(absent.id, false)
	for day := 0; day < 7; day++ {
		d := day
		f.net.After(time.Duration(d)*24*time.Hour+time.Hour, func() {
			f.publish(t, f.nodes[d%4], 2)
		})
	}
	f.net.Run(7 * 24 * time.Hour)

	want := f.nodes[0].store.Total()
	f.net.SetUp(absent.id, true)
	ok := f.net.RunUntil(24*time.Hour, f.convergedOn(want))
	c.report()
	if !ok {
		t.Fatalf("backfill did not complete; the returning node holds %d of %d",
			absent.store.Total(), want)
	}
}

// Heavy duplication and reordering, which is what a flooding mesh actually
// delivers. Idempotence is an invariant, not a nicety: if re-applying a record
// were not a no-op, the count would climb on duplicates and the vector would
// outrun the log.
func TestInvariantsHoldUnderDuplicationAndReordering(t *testing.T) {
	cfg := DefaultConfig(109)
	cfg.LossRate = 0.10
	cfg.DuplicateRate = 0.60
	cfg.ReorderRate = 0.40
	f := newFederation(t, cfg, 5, 0)

	c := newInvariantChecker(t, f)
	c.maxAirtimeShare = 0.05

	for i := 0; i < 3; i++ {
		n := f.nodes[i]
		f.net.After(time.Duration(i+1)*10*time.Minute, func() { f.publish(t, n, 4) })
	}

	ok := f.net.RunUntil(24*time.Hour, f.convergedOn(12))
	c.report()
	if !ok {
		t.Fatal("did not converge under heavy duplication and reordering")
	}
	for _, n := range f.nodes {
		if n.store.Rejected != 0 {
			t.Errorf("node %s rejected %d records; duplicates must be ignored, not refused",
				n.id.Short(), n.store.Rejected)
		}
	}
}

// A checker nobody has seen fail is indistinguishable from one that cannot
// fail. Each subtest breaks exactly one invariant and asserts it is caught.
//
// This is not paranoia. A canonicality fuzz target in this repo passed five
// million executions against a deliberately broken parser, because the property
// it asserted was structurally unable to observe the bug. The only way to know a
// check works is to watch it reject something.
func TestTheInvariantCheckerHasTeeth(t *testing.T) {
	// sabotage converges a small federation, applies mutate, runs on, and
	// returns whatever the checker complained about.
	sabotage := func(t *testing.T, tune func(*invariantChecker), mutate func(*federation)) string {
		t.Helper()
		cfg := DefaultConfig(211)
		cfg.LossRate = 0.10
		f := newFederation(t, cfg, 4, 0)

		var captured string
		c := newInvariantChecker(t, f)
		c.onViolation = func(msg string) {
			if captured == "" {
				captured = msg
			}
		}
		f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 4) })
		if !f.net.RunUntil(6*time.Hour, f.convergedOn(4)) {
			t.Fatal("the scenario did not converge before sabotage, so the test proves nothing")
		}
		// Nothing should have been reported by an honest run. This is what makes
		// the subtests below meaningful rather than "the checker complains about
		// everything".
		if captured != "" {
			t.Fatalf("the checker complained BEFORE sabotage: %s", captured)
		}

		// tune is applied here, not at construction: for the airtime case the
		// tightened budget IS the sabotage, and applying it earlier would breach
		// during the honest run above.
		if tune != nil {
			tune(c)
		}
		mutate(f)
		f.net.Run(2 * time.Hour)
		return captured
	}

	t.Run("forgetting a record", func(t *testing.T) {
		got := sabotage(t, nil, func(f *federation) {
			f.nodes[1].store.CorruptDropOne(f.area)
		})
		if got == "" {
			t.Error("a node losing a record was not caught by the monotonicity check")
		} else {
			t.Logf("caught: %s", got)
		}
	})

	t.Run("rewriting a slot", func(t *testing.T) {
		got := sabotage(t, nil, func(f *federation) {
			// A different but validly signed record, put in an occupied slot.
			// The record COUNT is unchanged, so only the immutability check can
			// see this.
			victim := f.nodes[1]
			impostor := f.nodes[2].key
			rep, err := record.New(impostor, record.Record{
				Origin: impostor.ID(), Seq: 1, TS: 1, Type: record.TypePost,
				Area: f.area, Body: []byte("impostor"),
			})
			if err != nil {
				t.Fatal(err)
			}
			victim.store.CorruptReplaceOne(f.area, rep)
		})
		if got == "" {
			t.Error("a rewritten record slot was not caught by the immutability check")
		} else {
			t.Logf("caught: %s", got)
		}
	})

	t.Run("a lying version vector", func(t *testing.T) {
		got := sabotage(t, nil, func(f *federation) {
			// Claim a high-water mark the log cannot support. Nothing is lost
			// and nothing is rewritten, so only the vector-honesty check sees it.
			f.nodes[1].store.Vector(f.area).Set(f.nodes[0].id, 9999)
		})
		if got == "" {
			t.Error("a vector claiming more than the log holds was not caught")
		} else {
			t.Logf("caught: %s", got)
		}
	})

	t.Run("a tampered record body", func(t *testing.T) {
		got := sabotage(t, nil, func(f *federation) {
			// Mutate a record in place, after it was admitted. Apply's check
			// cannot see this; only re-verification can.
			f.nodes[1].store.CorruptTamperOne(f.area)
		})
		if got == "" {
			t.Error("a record mutated after admission was not caught by re-verification")
		} else {
			t.Logf("caught: %s", got)
		}
	})

	t.Run("exceeding the airtime budget", func(t *testing.T) {
		got := sabotage(t, func(c *invariantChecker) {
			c.maxAirtimeShare = 0.0000001 // ordinary traffic must breach this
			c.airtimeWindow = time.Minute // so a full window elapses quickly
		}, func(f *federation) {})
		if got == "" {
			t.Error("blowing the airtime budget was not caught")
		} else {
			t.Logf("caught: %s", got)
		}
	})
}

// How much can a federation actually publish before it breaks the §1.1 budget?
//
// This test exists because TestInvariantsHoldThroughoutConvergence originally
// failed at 5.40%, publishing twelve records in its first hour. That was not a
// broken invariant or a bad threshold — it was the scenario asking the mesh to
// carry more than LongFast can carry. Rather than quietly lowering the rate
// until the test went green, measure the ceiling and state it.
//
// The number is small and that is the point. §1.1's whole argument is that
// airtime, not storage or CPU, is the binding constraint on this design, and a
// federation that feels roomy in a simulator is one whose simulator is lying
// about the radio.
func TestSustainablePublishRate(t *testing.T) {
	const budget = 0.05

	measure := func(recordsPerHour int) (peak float64, converged bool) {
		cfg := DefaultConfig(307)
		cfg.LossRate = 0.15
		f := newFederation(t, cfg, 6, 0)
		c := newInvariantChecker(t, f)
		c.maxAirtimeShare = 1 // measure only; never fail here
		c.onViolation = func(string) {}

		// Spread the hour's records evenly across six hours of publishing.
		const hours = 6
		total := recordsPerHour * hours
		gap := time.Duration(float64(time.Hour) / float64(recordsPerHour))
		for i := 0; i < total; i++ {
			n := f.nodes[i%len(f.nodes)]
			f.net.After(time.Duration(i+1)*gap, func() { f.publish(t, n, 1) })
		}
		converged = f.net.RunUntil(48*time.Hour, f.convergedOn(total))
		return c.peakShare, converged
	}

	t.Logf("LongFast, R=%.0f, 15%% loss, 6 nodes — sustained publish rate vs the %.0f%% budget:",
		DefaultConfig(0).FloodMultiplier, budget*100)

	ceiling := 0
	for _, rate := range []int{1, 2, 4, 6, 8, 12} {
		peak, converged := measure(rate)
		verdict := "within budget"
		if peak > budget {
			verdict = "OVER BUDGET"
		} else {
			ceiling = rate
		}
		if !converged {
			verdict += ", did not converge"
		}
		t.Logf("  %2d records/hour: peak %.2f%% of the channel — %s", rate, peak*100, verdict)
	}

	t.Logf("highest rate measured within budget: %d records/hour, federation-wide", ceiling)
	if ceiling == 0 {
		t.Error("even one record per hour breaks the airtime budget; something is badly wrong")
	}
}
