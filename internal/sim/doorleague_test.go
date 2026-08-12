package sim

import (
	"context"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/bsmp"
	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

// An inter-BBS league, end to end, over the simulated mesh (§9.5).
//
// # Why this is not in gossip_test.go, and why it uses the real store
//
// Everything else in this package federates through gossiptest.Store, for the
// reason that package's own header gives: fifty SQLite databases would make a
// thirty-day soak impossible to run and impossible to replay. This test cannot
// use it, because the thing under test is not convergence — that is proven
// several times over next door — but whether what converged is still a league
// when it lands. Only the real store has DoorEventsSince, the local arrival
// cursor it reads, and the area-kind check that decides whether a peer's
// DOOR_EVENT is allowed into the log at all.
//
// So it is TWO instances rather than fifty. Two is the smallest number that can
// have a league, and the cost of the real store is why it is not more.
//
// # What would be broken in production if this failed
//
// The §9.5 payoff itself: a door reports a result on one board and a door on
// another board never sees it. Every piece of that path has unit tests — the
// codec, the queue, the flusher, the poll, the sync engine — and each one can
// pass while the composition fails, because the failures live in the joins. An
// area whose tag matches but whose kind does not; a record that federates but
// is refused on arrival; a cursor that advances past a record it never
// returned. This is the test that holds them together.

// instance is one simulated BBS: a real store, a real service, and the shipped
// sync stack over the simulated radio.
type instance struct {
	key    identity.NodeKey
	id     identity.NodeID
	st     *store.Store
	svc    *bbs.Service
	engine *gossip.Engine
	link   *Link
	out    *bsmp.Outbox
	in     *bsmp.Inbox
}

// league is two instances that share one federated door area.
type league struct {
	net  *Network
	ctx  context.Context
	a, b *instance
	// area is the league's NAME. The tag is derived from it, which is the whole
	// coordination mechanism — there is no registry — and so getting both boards
	// to use the same name is exactly what a sysop has to do in real life.
	area string
	tag  record.AreaTag
}

// newLeague builds two instances that both carry the same federated door area.
func newLeague(t *testing.T, cfg Config, area string) *league {
	t.Helper()
	ctx := context.Background()

	dict, err := bundle.NewRawDictionary(0, []byte("meshbbs door event arena league"))
	if err != nil {
		t.Fatal(err)
	}
	dicts, err := bundle.NewDictionarySet(dict)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dicts.Close)

	net := New(cfg)
	l := &league{net: net, ctx: ctx, area: area, tag: record.AreaTagFor(area)}

	for i, slot := range []**instance{&l.a, &l.b} {
		// Seeded keys, so a failure replays from the seed like everything else
		// in this package. Production uses crypto/rand and never this.
		key, err := identity.GenerateNodeKey(rng.TestSecret(uint64(i) + 1))
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenMemory(ctx, net.Clock())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })

		// Both boards create the league under the same name and federate it.
		// A board that had it local-only, or as a forum, would refuse the
		// records on arrival — which is a real failure mode and one of the
		// things this fixture is asserting is set up correctly.
		if _, err := st.CreateDoorArea(ctx, area, "the arena", true); err != nil {
			t.Fatal(err)
		}

		n := &instance{key: key, id: key.ID(), st: st}
		n.svc = bbs.New(st, key, net.Clock())
		n.link = net.NewLink(n.id, 0)

		out, err := bsmp.NewOutbox(bsmp.Config{
			Self: n.id, Link: n.link, Dictionary: dict, LossRate: cfg.LossRate,
		})
		if err != nil {
			t.Fatal(err)
		}
		n.out = out
		*slot = n
	}

	// Each board knows the other's public key.
	//
	// In production this arrives as NODE records through the same anti-entropy
	// machinery (§6.1.2), minutes behind on a lossy mesh; seeding it keeps this
	// test about the league rather than about roster bootstrap. It is not
	// optional decoration — GossipStore.Apply verifies every inbound record
	// against the origin's stored key and quarantines what it cannot check.
	for _, pair := range [][2]*instance{{l.a, l.b}, {l.b, l.a}} {
		me, peer := pair[0], pair[1]
		for _, k := range []identity.NodeKey{me.key, peer.key} {
			if err := me.st.PutNode(l.ctx, store.Node{ID: k.ID(), PublicKey: k.Public}); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, n := range []*instance{l.a, l.b} {
		n := n
		gstore, err := store.NewGossipStore(ctx, n.st, func(err error) {
			// The adapter's errors are refusals as often as faults — an unknown
			// origin, a record in the wrong kind of area — and both are exactly
			// what a failure here would need explaining. Logged rather than
			// failed, so a diagnosis survives to the end of the run.
			t.Logf("node %s: record store: %v", n.id.Short(), err)
		})
		if err != nil {
			t.Fatal(err)
		}
		n.engine = gossip.New(n.id, gstore, n.out, net.Clock(),
			net.Rand("engine-"+n.id.Short()), gossip.DefaultConfig())
		in, err := bsmp.NewInbox(bsmp.InboxConfig{
			Engine: n.engine, Dictionaries: dicts, Clock: net.Clock(),
		})
		if err != nil {
			t.Fatal(err)
		}
		n.in = in
		// A locally written record reaches the mesh the way it does in
		// production: the service hands it to the engine. Nothing in this test
		// puts a record on the wire by hand.
		n.svc.SetPublisher(n.engine)
	}

	// Driven from the event loop, never from a goroutine — §12.1's third rule.
	for _, n := range []*instance{l.a, l.b} {
		n := n
		net.Every(5*time.Second, func() {
			n.link.Pump(func(dg link.Datagram) {
				if err := n.in.Deliver(dg); err != nil {
					t.Errorf("node %s: %v", n.id.Short(), err)
				}
			})
			if err := n.engine.Tick(); err != nil {
				t.Errorf("node %s tick: %v", n.id.Short(), err)
			}
		})
	}
	return l
}

// holdsDoorEvents reports whether an instance has n events on the league.
func (l *league) holdsDoorEvents(n *instance, game string, want int) func() bool {
	return func() bool {
		events, _, _, err := n.st.DoorEventsSince(l.ctx, l.tag, game, 0, 0)
		if err != nil {
			return false
		}
		return len(events) >= want
	}
}

// flush publishes everything queued on an instance, the way the federation tick
// does.
//
// The batching POLICY — how long to wait, when to give up — is internal/
// doorevent's and is tested there. What this borrows is only the ordering the
// flusher uses: read the queue, publish exactly that snapshot, then forget it.
// Publishing first and dequeuing second is deliberate there and here, because a
// failure between the two resends an event rather than losing it.
func (l *league) flush(t *testing.T, n *instance, game string) record.ID {
	t.Helper()
	queued, err := n.st.QueuedDoorEvents(l.ctx, l.area, game)
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) == 0 {
		t.Fatal("nothing was queued, so the door API accepted an event and dropped it")
	}
	id, err := n.svc.PublishDoorEvents(l.ctx, l.area, game, queued)
	if err != nil {
		t.Fatalf("publishing the batch: %v", err)
	}
	ids := make([]int64, 0, len(queued))
	for _, q := range queued {
		ids = append(ids, q.ID)
	}
	if err := n.st.DeleteQueuedDoorEvents(l.ctx, ids); err != nil {
		t.Fatal(err)
	}
	return id
}

// The §9.5 payoff, proven rather than asserted: a fight reported on one board
// is readable by a door on another, having crossed a lossy simulated mesh as a
// signed, fountain-coded, batched record.
//
// A failure here means inter-BBS leagues do not work, whatever the unit tests
// of each stage say.
func TestADoorEventCrossesTheMesh(t *testing.T) {
	cfg := DefaultConfig(3)
	cfg.LossRate = 0.15
	l := newLeague(t, cfg, "arena")

	const game = "arena"

	// Reported through the door API HOST, not by writing a queue row: the host
	// is where the area is checked for kind and federation and where the target
	// is resolved, and a test that skipped it would prove the wire works for
	// events no door could ever have emitted.
	//
	// "bob@<node>" is the thing a league exists for — naming somebody who is not
	// on this board. It is a claim by this node rather than a verified fact, and
	// §9.5 says so; what has to survive the crossing is the claim exactly as
	// made.
	err := l.a.svc.Doors().QueueDoorEvent(l.ctx, door.DoorEventRequest{
		Door: "arena", Area: l.area, Game: game,
		Kind: 1, Actor: "alice",
		Target:  "bob@" + l.b.id.Compact(),
		Payload: []byte{7},
	})
	if err != nil {
		t.Fatalf("queueing the result: %v", err)
	}

	l.net.After(time.Minute, func() { l.flush(t, l.a, game) })

	if !l.net.RunUntil(6*time.Hour, l.holdsDoorEvents(l.b, game, 1)) {
		t.Fatalf("a door event did not reach the other board within six simulated hours "+
			"(it holds %d records on the league)", recordsHeld(t, l, l.b))
	}
	t.Logf("the event crossed in %s of simulated time; the mesh carried %d datagrams",
		l.net.Now().Sub(cfg.Start).Round(time.Second), l.net.Stats().Datagrams)

	// And a DOOR on the far board can read it. Through the host again, because
	// that is what renders node IDs into something a door can print and compare
	// — a door never handles raw eight-byte IDs (§6.1.4.1).
	batch, err := l.b.svc.Doors().PollDoorEvents(l.ctx, door.DoorEventPoll{
		Door: "arena", Area: l.area, Game: game,
	})
	if err != nil {
		t.Fatalf("polling the league on the far board: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("the far board's poll returned %d events, want 1", len(batch.Events))
	}
	ev := batch.Events[0]

	// The actor is the nick on the ORIGIN's board, and the origin is how you
	// know whose alice it is. Two boards in one league can each have one, so an
	// event that arrived without its origin would be unattributable.
	if ev.Actor != "alice" {
		t.Errorf("the event names actor %q, want alice", ev.Actor)
	}
	if ev.Origin != l.a.id.Compact() {
		t.Errorf("the event came from %q, want the reporting board %q", ev.Origin, l.a.id.Compact())
	}
	// The target survived as nick plus node, which is the part that makes a
	// league a league. Losing the node would leave "alice slew bob" meaning
	// whichever bob the reader happens to have.
	if ev.Target != "bob" {
		t.Errorf("the event names target %q, want bob", ev.Target)
	}
	if ev.TargetNode != l.b.id.Compact() {
		t.Errorf("the target is on node %q, want %q", ev.TargetNode, l.b.id.Compact())
	}
	if ev.Kind != 1 {
		t.Errorf("the event is kind %d, want 1", ev.Kind)
	}
	// The payload is opaque to every layer it crossed, which is exactly why it
	// is worth checking: nothing between the two doors had any reason to notice
	// it was wrong.
	payload, err := ev.PayloadBytes()
	if err != nil {
		t.Fatalf("the payload did not decode: %v", err)
	}
	if len(payload) != 1 || payload[0] != 7 {
		t.Errorf("the payload arrived as %v, want [7]", payload)
	}

	if batch.Truncated {
		t.Error("the poll reported a retention gap on a league one record old")
	}
	if batch.Cursor <= 0 {
		t.Errorf("the poll returned cursor %d; a door saving that would re-read "+
			"the whole league on every launch", batch.Cursor)
	}

	// A door that saves its cursor is not shown the same fight twice. This is
	// the property that lets a league table be a running tally instead of a
	// recount, and it is the reason the BBS keeps no read position of its own.
	again, err := l.b.svc.Doors().PollDoorEvents(l.ctx, door.DoorEventPoll{
		Door: "arena", Area: l.area, Game: game, After: batch.Cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Events) != 0 {
		t.Errorf("polling from the returned cursor replayed %d events", len(again.Events))
	}

	// The reporting board holds it too. Obvious, and worth one line: a league
	// where the originator cannot read its own results back would show every
	// board everyone's fights but its own.
	if !l.holdsDoorEvents(l.a, game, 1)() {
		t.Error("the reporting board cannot read back the event it published")
	}
}

// Two boards each reporting is the ordinary case, and the one where a naive
// implementation double-counts or drops half.
//
// It also exercises batching across the crossing: three events on one board
// become ONE record, and the far board has to see three again. §9.5's whole
// airtime argument rests on that ratio, and a receiver that unpacked a batch
// wrongly would make the saving imaginary.
func TestBothBoardsSeeEachOthersLeague(t *testing.T) {
	cfg := DefaultConfig(5)
	cfg.LossRate = 0.10
	l := newLeague(t, cfg, "arena")

	const game = "arena"

	for i, from := range []*instance{l.a, l.b} {
		to := l.b
		if from == l.b {
			to = l.a
		}
		// Three fights on the reporting board, one batch.
		for j := 0; j < 3; j++ {
			err := from.svc.Doors().QueueDoorEvent(l.ctx, door.DoorEventRequest{
				Door: "arena", Area: l.area, Game: game,
				Kind:    uint8(1 + j%2),
				Actor:   []string{"alice", "bob"}[i],
				Target:  "rival@" + to.id.Compact(),
				Payload: []byte{byte(j)},
			})
			if err != nil {
				t.Fatalf("queueing on board %d: %v", i, err)
			}
		}
		n := from
		l.net.After(time.Duration(i+1)*time.Minute, func() { l.flush(t, n, game) })
	}

	converged := func() bool {
		return l.holdsDoorEvents(l.a, game, 6)() && l.holdsDoorEvents(l.b, game, 6)()
	}
	if !l.net.RunUntil(12*time.Hour, converged) {
		t.Fatalf("the two boards did not agree on the league within twelve simulated hours "+
			"(they hold %d and %d records)", recordsHeld(t, l, l.a), recordsHeld(t, l, l.b))
	}

	// Six events in two records: the batching survived the crossing. If this
	// read six records, §9.5's measured 3.3x saving would be fiction — the
	// events would have been re-signed one by one somewhere along the way.
	for _, n := range []*instance{l.a, l.b} {
		if got := recordsHeld(t, l, n); got != 2 {
			t.Errorf("board %s holds %d DOOR_EVENT records for six events, want 2",
				n.id.Short(), got)
		}
	}

	// Each board sees both actors, which is the league being a league rather
	// than two boards talking to themselves.
	for _, n := range []*instance{l.a, l.b} {
		events, _, _, err := n.st.DoorEventsSince(l.ctx, l.tag, game, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]int{}
		for _, ev := range events {
			seen[ev.Actor]++
		}
		if seen["alice"] != 3 || seen["bob"] != 3 {
			t.Errorf("board %s counted %v, want three each from alice and bob",
				n.id.Short(), seen)
		}
	}
}

// recordsHeld counts DOOR_EVENT RECORDS, not events, by counting distinct
// cursors: every event out of one record shares one.
func recordsHeld(t *testing.T, l *league, n *instance) int {
	t.Helper()
	events, _, _, err := n.st.DoorEventsSince(l.ctx, l.tag, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, ev := range events {
		seen[ev.Cursor] = true
	}
	return len(seen)
}
