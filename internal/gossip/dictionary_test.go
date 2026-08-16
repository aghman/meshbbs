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

// quietOutbox accepts everything and remembers nothing. The floor is computed
// from what arrives, so nothing here needs to send.
type quietOutbox struct{}

func (quietOutbox) SendMessage(identity.NodeID, []byte) error          { return nil }
func (quietOutbox) SendRecords(record.AreaTag, []*record.Record) error { return nil }
func (quietOutbox) Budget() link.Budget {
	return link.Budget{Available: time.Hour, PerDatagram: time.Second}
}

// floorEngine builds an engine holding `ours` as its own dictionary level.
func floorEngine(t *testing.T, ours uint8) (*Engine, *clock.Virtual) {
	t.Helper()
	clk := clock.NewVirtual(time.Unix(1700000000, 0).UTC())
	cfg := DefaultConfig()
	cfg.Dictionary = ours
	return New(node(1), gossiptest.NewStore(record.AreaTagFor("general")),
		quietOutbox{}, clk, rng.NewSeeded(1), cfg), clk
}

// hear feeds the engine a digest from `from` advertising dictionary `dict`.
func hear(t *testing.T, e *Engine, from identity.NodeID, dict uint8) {
	t.Helper()
	d := &Digest{Dictionary: dict, Areas: []AreaState{
		{Tag: record.AreaTagFor("general"), Hash: [4]byte{1, 2, 3, 4}, Count: 1},
	}}
	if err := e.Receive(from, d.Encode()); err != nil {
		t.Fatalf("receive digest from %s: %v", from.Short(), err)
	}
}

// A node that has heard nobody compresses with its best.
//
// The opposite rule is the tempting one and it is a trap: treating an unknown
// peer as dictionary 0 would pin a fresh node at 0 permanently, because a node
// that has heard nobody would have nothing to raise it.
func TestFloorWithNoPeersIsOurOwn(t *testing.T) {
	e, _ := floorEngine(t, 1)
	got, _, constrained := e.DictionaryFloor()
	if got != 1 || constrained {
		t.Errorf("floor is %d (constrained=%v), want 1 and unconstrained", got, constrained)
	}
}

// One peer on an older build holds the whole mesh, because SendRecords
// broadcasts and there is only one set of bytes to choose.
func TestOnePeerPinsTheMesh(t *testing.T) {
	e, _ := floorEngine(t, 2)
	hear(t, e, node(2), 2)
	hear(t, e, node(3), 0) // the laggard
	hear(t, e, node(4), 2)

	got, holder, constrained := e.DictionaryFloor()
	if got != 0 {
		t.Errorf("floor is %d, want 0 — one peer at 0 constrains everyone", got)
	}
	if !constrained {
		t.Error("floor is below our own level but was not reported as constrained")
	}
	if holder != node(3) {
		t.Errorf("blamed %s, want the peer that actually advertised 0", holder.Short())
	}
}

// Upgrading the laggard raises the floor on its next digest, with no restart.
func TestRaisingTheLaggardRaisesTheFloor(t *testing.T) {
	e, _ := floorEngine(t, 2)
	hear(t, e, node(2), 2)
	hear(t, e, node(3), 0)

	if got, _, _ := e.DictionaryFloor(); got != 0 {
		t.Fatalf("floor is %d before the upgrade, want 0", got)
	}

	hear(t, e, node(3), 2) // same peer, now upgraded

	got, _, constrained := e.DictionaryFloor()
	if got != 2 || constrained {
		t.Errorf("floor is %d (constrained=%v) after the upgrade, want 2 and unconstrained", got, constrained)
	}
}

// A peer that has fallen past PeerTimeout stops voting, by the same definition
// of "gone" the digest interval already uses.
func TestTimedOutPeerStopsConstraining(t *testing.T) {
	e, clk := floorEngine(t, 2)
	hear(t, e, node(3), 0)

	if got, _, _ := e.DictionaryFloor(); got != 0 {
		t.Fatalf("a live peer at 0 should pin the floor to 0")
	}

	clk.Advance(DefaultConfig().PeerTimeout + time.Minute)

	got, _, constrained := e.DictionaryFloor()
	if got != 2 || constrained {
		t.Errorf("floor is %d (constrained=%v) after the peer timed out, want 2 and unconstrained",
			got, constrained)
	}
}

// A peer that has spoken but never sent a digest has said nothing about what it
// holds, and must not be read as holding dictionary 0.
//
// This is the bug the dictKnown flag exists to prevent: peerState is created by
// the first message of any type, so without it a peer whose first message is a
// vector request would silently drag the mesh to dictionary 0.
func TestPeerThatNeverSentADigestDoesNotVote(t *testing.T) {
	e, _ := floorEngine(t, 2)

	req := &VectorReq{Areas: []record.AreaTag{record.AreaTagFor("general")}}
	if err := e.Receive(node(5), req.Encode()); err != nil {
		t.Fatalf("receive vector request: %v", err)
	}

	got, _, constrained := e.DictionaryFloor()
	if got != 2 || constrained {
		t.Errorf("floor is %d (constrained=%v); silence is not a claim about dictionaries",
			got, constrained)
	}
}

// Our own digests carry our level, which is the half of §7.4 that lets anyone
// else compute a floor at all.
func TestOurDigestAdvertisesOurDictionary(t *testing.T) {
	e, _ := floorEngine(t, 2)
	if got := e.Digest().Dictionary; got != 2 {
		t.Errorf("our digest advertises dictionary %d, want 2", got)
	}
}
