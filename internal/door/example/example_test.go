package example

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/door"
)

// The arena door's league bookkeeping (§9.5).
//
// The door itself needs a BBS at the other end of a socket and is exercised
// through the API's own tests; what is testable here is the part that decides
// what a player sees — the tally, its ordering, and its survival across
// invocations. Every failure below is a league table that lies to somebody.

func polled(actor, origin string, kind uint8, payload ...byte) door.PolledDoorEvent {
	ev := door.PolledDoorEvent{Actor: actor, Origin: origin, Kind: kind}
	if len(payload) > 0 {
		ev.Payload = base64.StdEncoding.EncodeToString(payload)
	}
	return ev
}

// A fight is counted once, against the actor, on the board the record came
// from.
//
// The origin is half the identity. Two boards in one league can each have an
// alice, and a door that keyed on the nick alone would merge them into one
// fighter with somebody else's record — which is worse than showing nothing,
// because it looks right.
func TestArenaCountsEachFighterOnTheirOwnBoard(t *testing.T) {
	var table []arenaFighter
	for _, ev := range []door.PolledDoorEvent{
		polled("alice", "AAAAAAAAAAAAA", arenaWon, 4),
		polled("alice", "AAAAAAAAAAAAA", arenaLost),
		polled("alice", "BBBBBBBBBBBBB", arenaWon, 2), // a different alice
		polled("bob", "BBBBBBBBBBBBB", arenaWon, 9),
	} {
		table = arenaRecord(table, ev)
	}

	if len(table) != 3 {
		t.Fatalf("counted %d fighters, want 3: %+v", len(table), table)
	}
	for _, want := range []arenaFighter{
		{Name: "alice", Node: "AAAAAAAAAAAAA", Wins: 1, Losses: 1, Best: 4},
		{Name: "alice", Node: "BBBBBBBBBBBBB", Wins: 1, Best: 2},
		{Name: "bob", Node: "BBBBBBBBBBBBB", Wins: 1, Best: 9},
	} {
		if !holds(table, want) {
			t.Errorf("the table is missing %+v; it holds %+v", want, table)
		}
	}
}

// An event this door does not understand is still an event.
//
// Kinds are door-defined and a league is other people's software: a board that
// added a third kind must not be able to break the table on every board that
// has not. The forward-compatible reading is to count the fight and skip the
// detail.
func TestArenaSurvivesAnUnknownKind(t *testing.T) {
	table := arenaRecord(nil, polled("alice", "AAAAAAAAAAAAA", 200, 3))
	if len(table) != 1 {
		t.Fatalf("an unknown kind produced %d rows, want 1", len(table))
	}
	if table[0].Wins != 0 || table[0].Losses != 0 {
		t.Errorf("an unknown kind was counted as a result: %+v", table[0])
	}
}

// A payload belongs to whoever wrote it, so an unreadable one must not stop the
// event being counted. This is the shape of an event from a board running a
// different version of the game.
func TestArenaToleratesAPayloadItCannotRead(t *testing.T) {
	ev := polled("alice", "AAAAAAAAAAAAA", arenaWon)
	ev.Payload = "not base64 at all!!"

	table := arenaRecord(nil, ev)
	if len(table) != 1 || table[0].Wins != 1 {
		t.Fatalf("a bad payload lost the fight: %+v", table)
	}
	if table[0].Best != 0 {
		t.Errorf("a bad payload was read as a margin of %d", table[0].Best)
	}
}

// The standings survive an invocation, because the BBS keeps no read position
// for a door: the cursor and the tally it produced are the door's own state, and
// re-reading from a saved cursor is only correct if the tally came back with it.
func TestArenaStandingsSurviveARoundTrip(t *testing.T) {
	want := []arenaFighter{
		{Name: "alice", Node: "AAAAAAAAAAAAA", Wins: 3, Losses: 1, Best: 12},
		{Name: "bob", Node: "BBBBBBBBBBBBB", Wins: 1, Losses: 2, Best: 5},
	}
	got := parseArenaTable(formatArenaTable(want))
	if len(got) != len(want) {
		t.Fatalf("round-tripped %d fighters, want %d", len(got), len(want))
	}
	for _, w := range want {
		if !holds(got, w) {
			t.Errorf("%+v did not survive the round trip; got %+v", w, got)
		}
	}
}

// A half-written line costs that line and not the table.
//
// A door can be killed on its wall clock mid-save, and a quota can truncate a
// value. Either leaves one bad line among good ones, and refusing the whole
// value would turn a lost tally into a lost league.
func TestArenaSkipsAnUnreadableLine(t *testing.T) {
	saved := "alice AAAAAAAAAAAAA 3 1 12\nbob BBBBBBB\ncarol CCCCCCCCCCCCC x 0 0\n" +
		"dave DDDDDDDDDDDDD 2 0 4\n"
	got := parseArenaTable(saved)
	if len(got) != 2 {
		t.Fatalf("parsed %d fighters out of two good lines: %+v", len(got), got)
	}
	if got[0].Name != "alice" || got[1].Name != "dave" {
		t.Errorf("the surviving lines are %+v", got)
	}
}

// The saved table is bounded, because a league is other people's boards and
// level-2 state has a quota. Unbounded, it would grow until a save failed —
// silently, on whichever launch crossed the line.
func TestArenaSavedStandingsAreBounded(t *testing.T) {
	var table []arenaFighter
	for i := 0; i < arenaMaxTracked*3; i++ {
		table = append(table, arenaFighter{
			Name: "fighter", Node: string(rune('a'+i%26)) + "AAAAAAAAAAAA", Wins: i,
		})
	}
	saved := formatArenaTable(table)
	if n := len(strings.Split(strings.TrimRight(saved, "\n"), "\n")); n != arenaMaxTracked {
		t.Fatalf("saved %d lines, want the cap of %d", n, arenaMaxTracked)
	}
	// And what it kept is the top of the table rather than an arbitrary slice:
	// dropping the leaders to keep the also-rans would make the cap visible as
	// a wrong answer instead of a short one.
	kept := parseArenaTable(saved)
	if kept[0].Wins != arenaMaxTracked*3-1 {
		t.Errorf("the best fighter (%d wins) was dropped; the table starts at %d",
			arenaMaxTracked*3-1, kept[0].Wins)
	}
}

// Equal records must sort the same way every launch, or the standings appear to
// reshuffle themselves for no reason a player can see.
func TestArenaOrderingIsTotal(t *testing.T) {
	table := []arenaFighter{
		{Name: "carol", Node: "CCCCCCCCCCCCC", Wins: 2, Losses: 1},
		{Name: "alice", Node: "AAAAAAAAAAAAA", Wins: 2, Losses: 1},
		{Name: "bob", Node: "BBBBBBBBBBBBB", Wins: 2, Losses: 0},
	}
	first := formatArenaTable(append([]arenaFighter(nil), table...))

	// The same set in a different arrival order has to render identically.
	shuffled := []arenaFighter{table[1], table[2], table[0]}
	if again := formatArenaTable(shuffled); again != first {
		t.Errorf("arrival order changed the standings:\n%s\nversus\n%s", first, again)
	}
	if !strings.HasPrefix(first, "bob ") {
		t.Errorf("fewest losses did not break the tie on wins:\n%s", first)
	}
}

func holds(table []arenaFighter, want arenaFighter) bool {
	for _, f := range table {
		if f == want {
			return true
		}
	}
	return false
}
