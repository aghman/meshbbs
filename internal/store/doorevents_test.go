package store

import (
	"context"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

func leagueStore(t *testing.T) (*Store, context.Context, record.AreaTag) {
	t.Helper()
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateDoorArea(ctx, "lordleague", "", true); err != nil {
		t.Fatal(err)
	}
	return st, ctx, record.AreaTagFor("lordleague")
}

func putDoorEvent(t *testing.T, st *Store, ctx context.Context, seed uint64, seq uint64, area record.AreaTag, actor string) *record.Record {
	t.Helper()
	key, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, Node{ID: key.ID(), PublicKey: key.Public}); err != nil {
		t.Fatal(err)
	}
	r, err := record.NewDoorEventRecord(key, seq, 1_765_000_000, area, record.DoorEventBody{
		Game:   "lord",
		Events: []record.DoorEvent{{Kind: 1, Actor: actor}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r
}

// The cursor is LOCAL ARRIVAL order, and this is the case that makes it matter:
// a record with a low (origin, seq) arriving after one with a high one.
//
// On a mesh that is ordinary — a bundle repaired an hour late, a peer back from
// a week offline. A door cursoring on (origin, seq) would step past the late
// arrival and never ask again.
func TestPollDeliversLateArrivalsInArrivalOrder(t *testing.T) {
	st, ctx, area := leagueStore(t)

	// Peer A's seq 5 lands first; peer A's seq 2 is repaired afterwards.
	putDoorEvent(t, st, ctx, 41, 5, area, "late-high")
	first, cursor, _, err := st.DoorEventsSince(ctx, area, "lord", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Actor != "late-high" {
		t.Fatalf("first poll returned %+v", first)
	}

	putDoorEvent(t, st, ctx, 41, 2, area, "repaired-low")

	// A cursor over (origin, seq) would consider seq 2 already seen. Over
	// arrival order it is new, which is what a door needs.
	next, _, _, err := st.DoorEventsSince(ctx, area, "lord", cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 {
		t.Fatalf("a record repaired out of order was not delivered: %+v", next)
	}
	if next[0].Actor != "repaired-low" {
		t.Errorf("delivered %q, want the repaired record", next[0].Actor)
	}
}

// Polling with the returned cursor must deliver each record exactly once.
func TestPollDeliversEachRecordOnce(t *testing.T) {
	st, ctx, area := leagueStore(t)
	for i := 1; i <= 5; i++ {
		putDoorEvent(t, st, ctx, 42, uint64(i), area, "alice")
	}

	seen := 0
	cursor := int64(0)
	for i := 0; i < 10; i++ {
		batch, next, _, err := st.DoorEventsSince(ctx, area, "lord", cursor, 0)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(batch)
		if next == cursor {
			break
		}
		cursor = next
	}
	if seen != 5 {
		t.Errorf("saw %d events across repeated polls, want 5", seen)
	}

	// And a poll at the end returns nothing rather than repeating.
	again, _, _, err := st.DoorEventsSince(ctx, area, "lord", cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("polling at the cursor repeated %d events", len(again))
	}
}

// The arrival counter must never hand out a number twice, even after the newest
// record is deleted. This is the rowid trap migration 0010 exists for: SQLite
// reuses max(rowid)+1, so the next insert would take the deleted record's
// number and every door cursored past it would silently miss it.
func TestArrivalNumbersAreNeverReused(t *testing.T) {
	st, ctx, area := leagueStore(t)

	first := putDoorEvent(t, st, ctx, 43, 1, area, "alice")
	_, cursor, _, err := st.DoorEventsSince(ctx, area, "lord", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Retention, or a sysop with sqlite3, removes the newest record.
	id := first.ID()
	if _, err := st.db.ExecContext(ctx, `DELETE FROM records WHERE id = ?`, id[:]); err != nil {
		t.Fatal(err)
	}

	// The next arrival must be numbered ABOVE the deleted one, not in its place.
	putDoorEvent(t, st, ctx, 43, 2, area, "bob")
	next, _, _, err := st.DoorEventsSince(ctx, area, "lord", cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].Actor != "bob" {
		t.Fatalf("a record inserted after a delete was invisible to a cursor at %d: %+v",
			cursor, next)
	}
	if next[0].Cursor <= cursor {
		t.Errorf("arrival number %d reuses or precedes the deleted record's %d",
			next[0].Cursor, cursor)
	}
}

// A door whose cursor points below what is still held has to be told, or it
// shows an incomplete league table and calls it complete.
func TestPollReportsPrunedEvents(t *testing.T) {
	st, ctx, area := leagueStore(t)
	for i := 1; i <= 3; i++ {
		putDoorEvent(t, st, ctx, 44, uint64(i), area, "alice")
	}
	// Prune everything below the newest, as retention would.
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM records WHERE local_seq < (SELECT MAX(local_seq) FROM records)`); err != nil {
		t.Fatal(err)
	}

	_, _, truncated, err := st.DoorEventsSince(ctx, area, "lord", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("a cursor behind the oldest surviving record was not told about the gap")
	}

	// A door that is caught up is not warned.
	_, cursor, _, err := st.DoorEventsSince(ctx, area, "lord", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, stillTruncated, _ := st.DoorEventsSince(ctx, area, "lord", cursor, 0); stillTruncated {
		t.Error("a caught-up door was told it had missed something")
	}
}

// A poll for one game must not return another's, since one league area can
// carry several.
func TestPollFiltersByGame(t *testing.T) {
	st, ctx, area := leagueStore(t)
	key, err := identity.GenerateNodeKey(rng.TestSecret(45))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, Node{ID: key.ID(), PublicKey: key.Public}); err != nil {
		t.Fatal(err)
	}
	for i, game := range []string{"lord", "tw2002", "lord"} {
		r, err := record.NewDoorEventRecord(key, uint64(i+1), 1, area, record.DoorEventBody{
			Game:   game,
			Events: []record.DoorEvent{{Kind: 1, Actor: "alice"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	lord, _, _, err := st.DoorEventsSince(ctx, area, "lord", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lord) != 2 {
		t.Errorf("polling for lord returned %d events, want 2", len(lord))
	}

	all, _, _, err := st.DoorEventsSince(ctx, area, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("polling for every game returned %d, want 3", len(all))
	}
}

// A league event for a game nothing here plays is KEPT.
//
// This is the receive-side decision, and it is deliberate rather than
// accidental. Two reasons to hold a record no local door can interpret: a door
// installed tomorrow should be able to catch up, and an instance that carries a
// league it does not play is how a multi-hop league reaches boards that cannot
// hear each other — the FTN pass-through pattern.
//
// Nothing about it is free, which is why the cost is surfaced rather than
// hidden: a record held is a record served to peers that ask.
func TestEventsForAnUnplayedGameAreKept(t *testing.T) {
	st, ctx, area := leagueStore(t)

	// No door is installed at all, let alone one that knows "tw2002".
	putDoorEventGame(t, st, ctx, 51, 1, area, "tw2002", "someone")

	events, cursor, _, err := st.DoorEventsSince(ctx, area, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("an event for an unplayed game was discarded: %+v", events)
	}
	if cursor == 0 {
		t.Error("no cursor was returned for a kept event")
	}

	// And a door installed later sees it, which is the point of keeping it.
	later, _, _, err := st.DoorEventsSince(ctx, area, "tw2002", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 {
		t.Errorf("a door arriving later could not catch up: %+v", later)
	}
}

// The summary must show a league nobody plays, because that is the case a sysop
// would otherwise never learn about.
func TestLeaguesReportsCarriedButUnplayedLeagues(t *testing.T) {
	st, ctx, area := leagueStore(t)
	putDoorEventGame(t, st, ctx, 52, 1, area, "tw2002", "alice")
	putDoorEventGame(t, st, ctx, 52, 2, area, "lord", "bob")

	leagues, err := st.Leagues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leagues) != 1 {
		t.Fatalf("summarised %d leagues", len(leagues))
	}
	l := leagues[0]
	if l.Area != "lordleague" || !l.Federated {
		t.Errorf("summary is %+v", l)
	}
	if len(l.Doors) != 0 {
		t.Errorf("found doors %v for a league with none installed", l.Doors)
	}
	if l.Received != 2 || l.Events != 2 {
		t.Errorf("received %d events in %d records, want 2 and 2", l.Events, l.Received)
	}
	// Both games are named, discovered from the record bodies rather than from
	// any registry — nothing registers a game.
	if len(l.Games) != 2 {
		t.Errorf("games = %v, want both", l.Games)
	}

	// Once a door points at it, the league is no longer unplayed.
	if err := st.PutDoor(ctx, Door{
		Name: "lord", Path: "/bin/echo", Cwd: t.TempDir(), APILevel: 3,
		LeagueArea: "lordleague", LeaguePerHour: 6, Enabled: true,
		WallClock: time.Hour, DropfileType: "none",
	}, "sysop"); err != nil {
		t.Fatal(err)
	}
	leagues, err = st.Leagues(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leagues[0].Doors) != 1 || leagues[0].Doors[0] != "lord" {
		t.Errorf("doors = %v, want [lord]", leagues[0].Doors)
	}
}

func putDoorEventGame(t *testing.T, st *Store, ctx context.Context, seed, seq uint64, area record.AreaTag, game, actor string) {
	t.Helper()
	key, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, Node{ID: key.ID(), PublicKey: key.Public}); err != nil {
		t.Fatal(err)
	}
	r, err := record.NewDoorEventRecord(key, seq, 1_765_000_000, area, record.DoorEventBody{
		Game:   game,
		Events: []record.DoorEvent{{Kind: 1, Actor: actor}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutRecord(ctx, r); err != nil {
		t.Fatal(err)
	}
}
