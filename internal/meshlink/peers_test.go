package meshlink

import (
	"errors"
	"testing"
	"time"
)

func announcement(t *testing.T, seed uint64, radio uint32, at time.Time) Announcement {
	t.Helper()
	key := testKey(t, seed)
	a, err := DecodeAnnounce(EncodeAnnounce(key, radio, at), radio)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPeerTableBindsBothWays(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()
	a := announcement(t, 1, 0xAAAA, now)

	if err := tbl.learn(a, now); err != nil {
		t.Fatal(err)
	}
	if radio, ok := tbl.radioFor(a.ID); !ok || radio != 0xAAAA {
		t.Errorf("radioFor = %#x, %v", radio, ok)
	}
	if id, ok := tbl.idFor(0xAAAA); !ok || id != a.ID {
		t.Errorf("idFor = %s, %v", id.Short(), ok)
	}
}

// A node's radio is not part of its identity (§6.1.1): sysops replace failed
// hardware without becoming a new instance.
func TestRebindingMovesForwardInTime(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()

	first := announcement(t, 1, 0xAAAA, base)
	second := announcement(t, 1, 0xBBBB, base.Add(time.Hour))
	if err := tbl.learn(first, base); err != nil {
		t.Fatal(err)
	}
	if err := tbl.learn(second, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if radio, _ := tbl.radioFor(first.ID); radio != 0xBBBB {
		t.Errorf("radio = %#x, want the newer binding", radio)
	}
	// The stale reverse mapping must go, or a packet from the old radio would
	// still be attributed to this node.
	if _, ok := tbl.idFor(0xAAAA); ok {
		t.Error("the old radio is still bound")
	}
}

// The replay that the timestamp exists to stop: an attacker rebroadcasting a
// peer's OLD announcement to drag it back to a radio it has left.
func TestOlderAnnouncementCannotRebind(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()

	current := announcement(t, 1, 0xBBBB, base.Add(time.Hour))
	stale := announcement(t, 1, 0xAAAA, base)
	if err := tbl.learn(current, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tbl.learn(stale, base.Add(2*time.Hour)); !errors.Is(err, ErrStaleAnnounce) {
		t.Fatalf("err = %v, want ErrStaleAnnounce", err)
	}
	if radio, _ := tbl.radioFor(current.ID); radio != 0xBBBB {
		t.Errorf("a replayed announcement moved the binding to %#x", radio)
	}
}

// Equal timestamps are not new information. Accepting them would let a captured
// frame flap a binding between two radios indefinitely.
func TestRepeatedAnnouncementIsNotNews(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()
	a := announcement(t, 1, 0xAAAA, now)

	if err := tbl.learn(a, now); err != nil {
		t.Fatal(err)
	}
	if err := tbl.learn(a, now); !errors.Is(err, ErrStaleAnnounce) {
		t.Errorf("err = %v, want ErrStaleAnnounce", err)
	}
}

// A radio number follows the hardware, and hardware gets reflashed and handed
// on. Whoever announced last owns it.
func TestRadioReuseEvictsThePreviousHolder(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()

	old := announcement(t, 1, 0xAAAA, base)
	newer := announcement(t, 2, 0xAAAA, base.Add(time.Minute))
	if err := tbl.learn(old, base); err != nil {
		t.Fatal(err)
	}
	if err := tbl.learn(newer, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if id, _ := tbl.idFor(0xAAAA); id != newer.ID {
		t.Errorf("radio is bound to %s, want the newer claimant", id.Short())
	}
	if _, ok := tbl.radioFor(old.ID); ok {
		t.Error("the evicted node still has a radio binding")
	}
}

// A node with a wildly wrong clock must not be able to pin its binding forever.
func TestFarFutureAnnouncementIsRejected(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()
	future := announcement(t, 1, 0xAAAA, now.Add(48*time.Hour))

	if err := tbl.learn(future, now); !errors.Is(err, ErrStaleAnnounce) {
		t.Fatalf("err = %v, want the announcement rejected", err)
	}
	// But ordinary clock drift is tolerated: Meshtastic nodes without GPS or a
	// phone attached genuinely do run hours out.
	near := announcement(t, 1, 0xAAAA, now.Add(2*time.Hour))
	if err := tbl.learn(near, now); err != nil {
		t.Errorf("a node two hours fast was rejected: %v", err)
	}
}

func TestBindingsAreSortedAndCopied(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tbl := newPeerTable()
	for seed := uint64(1); seed <= 5; seed++ {
		if err := tbl.learn(announcement(t, seed, uint32(seed), now), now); err != nil {
			t.Fatal(err)
		}
	}
	got := tbl.bindings()
	if len(got) != 5 {
		t.Fatalf("got %d bindings", len(got))
	}
	for i := 1; i < len(got); i++ {
		if !less(got[i-1].ID, got[i].ID) {
			t.Errorf("bindings are not sorted at %d", i)
		}
	}
}

// One missed announcement must not turn into a conversation that costs more
// airtime than the traffic it is about.
func TestAskRateLimitsQuestions(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := newAskRate(15 * time.Minute)

	if !r.allow(0xAAAA, now) {
		t.Fatal("first question refused")
	}
	if r.allow(0xAAAA, now.Add(time.Minute)) {
		t.Error("asked the same radio again within the window")
	}
	// A different radio is a different question.
	if !r.allow(0xBBBB, now.Add(time.Minute)) {
		t.Error("a different radio was refused")
	}
	if !r.allow(0xAAAA, now.Add(16*time.Minute)) {
		t.Error("refused after the window elapsed")
	}
}
