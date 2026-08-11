package store

import (
	"context"
	"testing"

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
