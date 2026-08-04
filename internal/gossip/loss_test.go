package gossip

import (
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/gossiptest"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

// lossOut is an Outbox that records the loss rate it is told.
type lossOut struct {
	rate float64
	set  int
}

func (o *lossOut) SendMessage(identity.NodeID, []byte) error          { return nil }
func (o *lossOut) SendRecords(record.AreaTag, []*record.Record) error { return nil }
func (o *lossOut) Budget() link.Budget                                { return link.Budget{} }
func (o *lossOut) SetLossRate(r float64)                              { o.rate, o.set = r, o.set+1 }
func (o *lossOut) LossRate() float64                                  { return o.rate }

// Compile-time proof that the fake still satisfies the capability. Without it
// the type assertion in noteDelivery just stops matching and every assertion
// about adaptation passes vacuously — which is how this test first went green
// while measuring nothing.
var _ LossAdapter = (*lossOut)(nil)

func lossEngine(t *testing.T, area record.AreaTag) (*Engine, *lossOut) {
	t.Helper()
	out := &lossOut{}
	st := gossiptest.NewStore(area)
	e := New(identity.NodeID{1}, st, out, clock.NewVirtual(time.Unix(1_800_000_000, 0)),
		rng.NewSeeded(1), DefaultConfig())
	return e, out
}

// A peer whose high-water mark does not move after we broadcast is the only
// signal a broadcast sender gets that anything was lost — there are no NACKs in
// the steady state (§7.2 item 4). The estimate must climb on it.
func TestLossEstimateClimbsWhenBundlesDoNotLand(t *testing.T) {
	area := record.AreaTag{'l', 'o', 's', 's'}
	e, out := lossEngine(t, area)
	peer := identity.NodeID{2}

	p := &peerState{
		id:         peer,
		lastDigest: map[record.AreaTag]AreaState{area: {Tag: area, Count: 10}},
		vectors:    nil,
		awaiting:   map[record.AreaTag]awaitedPush{},
	}
	e.peers[peer] = p

	for i := 0; i < 5; i++ {
		e.noteAwaited(area, 3)
		// Their count has NOT moved: the bundle did not reach them.
		e.settleAwaited(p, AreaState{Tag: area, Count: 10})
	}

	if e.LossEstimate() <= 0 {
		t.Fatalf("estimate stayed at %v after five missed bundles", e.LossEstimate())
	}
	if out.set == 0 {
		t.Error("the outbox was never told the new rate")
	}
	if out.rate != e.LossEstimate() {
		t.Errorf("outbox has %v, engine has %v", out.rate, e.LossEstimate())
	}
	if e.Stats().BundlesMissed != 5 {
		t.Errorf("BundlesMissed = %d, want 5", e.Stats().BundlesMissed)
	}
}

// And it must come back down, which matters more than it sounds: §7.2's cost
// table shows assuming 50% loss on a clean link sending 50 symbols where 16
// would do — three times the channel time, charged to every node on the
// frequency. An estimate that only ratchets up is a permanent tax.
func TestLossEstimateDecaysWhenBundlesLand(t *testing.T) {
	area := record.AreaTag{'l', 'o', 's', 's'}
	e, _ := lossEngine(t, area)
	peer := identity.NodeID{2}
	p := &peerState{
		id:         peer,
		lastDigest: map[record.AreaTag]AreaState{area: {Tag: area, Count: 10}},
		awaiting:   map[record.AreaTag]awaitedPush{},
	}
	e.peers[peer] = p

	for i := 0; i < 5; i++ {
		e.noteAwaited(area, 3)
		e.settleAwaited(p, AreaState{Tag: area, Count: 10})
	}
	peak := e.LossEstimate()
	if peak <= 0 {
		t.Fatal("nothing to decay from")
	}

	count := uint16(10)
	for i := 0; i < 40; i++ {
		p.lastDigest[area] = AreaState{Tag: area, Count: count}
		e.noteAwaited(area, 3)
		count += 3 // they got it
		e.settleAwaited(p, AreaState{Tag: area, Count: count})
	}
	if got := e.LossEstimate(); got >= peak {
		t.Errorf("estimate %v did not fall from its peak %v on a clean link", got, peak)
	}
}

// The estimate is bounded. §7.2's table stops at 50%, and past that the repair
// count climbs steeply for a link that is barely a link — better to stop paying
// and let want-repair carry the stragglers.
func TestLossEstimateIsBounded(t *testing.T) {
	area := record.AreaTag{'l', 'o', 's', 's'}
	e, _ := lossEngine(t, area)
	peer := identity.NodeID{2}
	p := &peerState{
		id:         peer,
		lastDigest: map[record.AreaTag]AreaState{area: {Tag: area, Count: 1}},
		awaiting:   map[record.AreaTag]awaitedPush{},
	}
	e.peers[peer] = p

	for i := 0; i < 200; i++ {
		e.noteAwaited(area, 1)
		e.settleAwaited(p, AreaState{Tag: area, Count: 1})
	}
	if got := e.LossEstimate(); got > lossCeiling {
		t.Errorf("estimate ran to %v, above the %v ceiling", got, lossCeiling)
	}
}

// A peer we cannot measure must not be guessed at. One whose count we have
// never heard, or whose count has saturated, produces no evidence either way —
// and inventing some is how an estimator learns the wrong thing.
func TestUnmeasurablePeersAreNotGuessedAt(t *testing.T) {
	area := record.AreaTag{'l', 'o', 's', 's'}
	e, out := lossEngine(t, area)

	unheard := &peerState{
		id:         identity.NodeID{2},
		lastDigest: map[record.AreaTag]AreaState{}, // never sent us a digest
		awaiting:   map[record.AreaTag]awaitedPush{},
	}
	saturated := &peerState{
		id:         identity.NodeID{3},
		lastDigest: map[record.AreaTag]AreaState{area: {Tag: area, Count: maxSaturatedCount}},
		awaiting:   map[record.AreaTag]awaitedPush{},
	}
	e.peers[unheard.id] = unheard
	e.peers[saturated.id] = saturated

	e.noteAwaited(area, 5)
	if len(unheard.awaiting) != 0 {
		t.Error("expected nothing of a peer that has never reported a count")
	}
	if len(saturated.awaiting) != 0 {
		t.Error("expected a gain from a peer whose count cannot rise")
	}
	if out.set != 0 {
		t.Errorf("the estimate moved on no evidence (%d updates)", out.set)
	}
}

// A missed bundle must never make the sender send FEWER repair symbols.
//
// It could. The outbox begins at its configured starting point — §7.2's "α
// starts at the observed mesh loss rate" — and an engine that began at zero
// would answer the first miss by pushing 0.05 down to an outbox already sitting
// at 0.10, quietly halving the redundancy at the exact moment evidence said to
// raise it. Found by probing the simulator, where engine and outbox were
// visibly disagreeing (est=0.000 while outbox=0.100) on every node that had not
// yet observed anything.
func TestAMissNeverLowersTheRepairRate(t *testing.T) {
	area := record.AreaTag{'s', 'e', 'e', 'd'}
	out := &lossOut{rate: 0.10} // the configured starting point
	st := gossiptest.NewStore(area)
	e := New(identity.NodeID{1}, st, out, clock.NewVirtual(time.Unix(1_800_000_000, 0)),
		rng.NewSeeded(1), DefaultConfig())

	if e.LossEstimate() != 0.10 {
		t.Fatalf("engine started at %v, not the outbox's %v", e.LossEstimate(), 0.10)
	}

	peer := identity.NodeID{2}
	p := &peerState{
		id:         peer,
		lastDigest: map[record.AreaTag]AreaState{area: {Tag: area, Count: 10}},
		awaiting:   map[record.AreaTag]awaitedPush{},
	}
	e.peers[peer] = p

	e.noteAwaited(area, 3)
	e.settleAwaited(p, AreaState{Tag: area, Count: 10}) // did not land

	if got := out.LossRate(); got < 0.10 {
		t.Errorf("a lost bundle lowered the repair rate to %v from 0.10", got)
	}
}
