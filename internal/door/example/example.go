// Package example holds the reference doors that ship with meshbbs (§9.1).
//
// A package of its own rather than part of the command tree, because two
// binaries need them: the real one, where `meshbbs door-example guess` is what
// the doors table points at, and the SSH test binary, which launches a
// reference door over a real connection to prove the contract end to end. The
// command tree cannot be imported by internal/sshd — internal/cli imports it
// already — so the doors live where both can reach them.
//
// # Why these three
//
// §9.1 asks for "two or three reference doors so the API has proof-of-life".
// Each has to earn its place. Hello is the smallest program that is
// recognisably a door, for somebody starting out. Guess is a real game that
// uses levels one, two and three, so the API has proof-of-life rather than
// proof-of-concept.
//
// Arena is the third, and it is not the variation this comment used to say a
// third would be. It is the only one that exercises §9.5 — event.emit and
// event.poll — and those are the two operations no single-board door can
// demonstrate: a league needs a federated door area, a grant separate from the
// announce one, and another BBS at the far end. Shipping a door that uses them
// is how a sysop finds out whether their league works before a third party's
// game is the thing under suspicion.
//
// # What a door author should take from these
//
// Not the game. The shape: open the connection, ask what level you were
// granted, and treat every refusal as an answer rather than a fault. Guess
// works at level 1, saves scores if it has 2, and announces if it has 3 — a
// door that assumes its grant and crashes without it is a door a sysop cannot
// safely install. Arena adds the two habits a league needs: keep your own read
// cursor, because the BBS keeps none for you, and say so out loud when the log
// tells you results went missing.
package example

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/aghman/meshbbs/internal/door"
)

// Names are the reference doors this package can run.
var Names = []string{"hello", "guess", "arena"}

// Run plays a reference door by name.
func Run(name string, stdin io.Reader, stdout io.Writer) error {
	switch name {
	case "hello":
		return runHelloDoor(stdin, stdout)
	case "guess":
		return runGuessDoor(stdin, stdout)
	case "arena":
		return runArenaDoor(stdin, stdout)
	}
	return fmt.Errorf("no reference door called %q", name)
}

// ---------------------------------------------------------------------------
// hello — the smallest door that is recognisably one
// ---------------------------------------------------------------------------

// runHelloDoor greets the caller by name and leaves.
//
// Level 1 and nothing else. It exists to answer one question for a sysop and
// one for a door author: does a door start here, and what is the least I have
// to write?
func runHelloDoor(stdin io.Reader, stdout io.Writer) error {
	c, err := door.Open()
	if err != nil {
		return err
	}
	defer c.Close()

	sess, err := c.Session()
	if err != nil {
		return err
	}

	who := sess.Handle
	if who == "" {
		who = "stranger"
	}
	fmt.Fprintf(stdout, "\r\nHello, %s.\r\n", who)
	fmt.Fprintf(stdout, "You are on node %d, at %dx%d.\r\n", sess.Node, sess.Width, sess.Height)
	if sess.TimeLimited {
		fmt.Fprintf(stdout, "You have %d seconds left on this call.\r\n", sess.TimeRemainingSecs)
	} else {
		fmt.Fprintf(stdout, "You have no time limit on this call.\r\n")
	}
	fmt.Fprintf(stdout, "\r\nThat is the whole door. Press enter.\r\n")

	_, _ = bufio.NewReader(stdin).ReadString('\n')
	return nil
}

// ---------------------------------------------------------------------------
// guess — a real game, using levels 1 to 3
// ---------------------------------------------------------------------------

const (
	guessBestKey   = "best"
	guessRecordKey = "record"
	guessMax       = 100
)

// runGuessDoor plays guess-the-number.
//
// Uses level 1 to greet the player, level 2 to remember their best score and
// the shared record, and level 3 to announce a new record. Everything it does
// degrades: a door granted less than it hoped for should still be playable, so
// each level is attempted and its refusal handled rather than assumed away.
func runGuessDoor(stdin io.Reader, stdout io.Writer) error {
	c, err := door.Open()
	if err != nil {
		return err
	}
	defer c.Close()

	sess, err := c.Session()
	if err != nil {
		return err
	}
	in := bufio.NewReader(stdin)

	// crypto/rand, not the injected rng.Source domain code uses (§12.1) and not
	// math/rand either. A door is a separate program, so there is no simulation
	// to replay here — and the number's whole job is to be unguessable by the
	// person at the keyboard, which is the one property a seeded source cannot
	// offer. The determinism checker agrees, and is right to.
	target, err := secretNumber(guessMax)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\r\nGUESS — I am thinking of a number from 1 to %d.\r\n", guessMax)
	if sess.Handle != "" {
		fmt.Fprintf(stdout, "Good luck, %s.\r\n", sess.Handle)
	}

	best := readInt(c, door.ScopeMine, guessBestKey)
	if best > 0 {
		fmt.Fprintf(stdout, "Your best is %d guesses.\r\n", best)
	}
	record, recordHolder := readRecord(c)
	if record > 0 {
		fmt.Fprintf(stdout, "The record is %d, by %s.\r\n", record, recordHolder)
	}
	fmt.Fprint(stdout, "\r\n")

	tries := 0
	for {
		fmt.Fprint(stdout, "Your guess (or 'q'): ")
		line, err := in.ReadString('\n')
		if err != nil {
			return nil // the player hung up; not this door's problem to report
		}
		line = strings.TrimSpace(line)
		if line == "q" || line == "quit" {
			fmt.Fprintf(stdout, "\r\nIt was %d. Come back soon.\r\n", target)
			return nil
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > guessMax {
			fmt.Fprintf(stdout, "A number from 1 to %d, please.\r\n", guessMax)
			continue
		}

		tries++
		switch {
		case n < target:
			fmt.Fprint(stdout, "Higher.\r\n")
		case n > target:
			fmt.Fprint(stdout, "Lower.\r\n")
		default:
			fmt.Fprintf(stdout, "\r\nGot it in %s!\r\n", plural(tries, "guess", "guesses"))
			celebrate(c, stdout, sess.Handle, tries, best, record)
			return nil
		}
	}
}

// celebrate saves the score and, if it is a record, tells the board.
func celebrate(c *door.Client, stdout io.Writer, who string, tries, best, record int) {
	if best == 0 || tries < best {
		if err := c.StateSet(door.ScopeMine, guessBestKey, strconv.Itoa(tries)); err != nil {
			// Shown, not swallowed. A player whose score vanished should be
			// told why, and "the door has used up its allowance" is something
			// the sysop can act on.
			fmt.Fprintf(stdout, "(Could not save your score: %v)\r\n", err)
		} else {
			fmt.Fprint(stdout, "A personal best.\r\n")
		}
	}
	if record != 0 && tries >= record {
		return
	}

	if err := c.StateSet(door.ScopeShared, guessRecordKey,
		fmt.Sprintf("%d %s", tries, who)); err != nil {
		fmt.Fprintf(stdout, "(Could not save the record: %v)\r\n", err)
		return
	}
	fmt.Fprint(stdout, "That is a new board record.\r\n")

	if c.Level() < 3 {
		return
	}
	_, err := c.Announce("New GUESS record",
		fmt.Sprintf("%s found the number in %s.", who, plural(tries, "guess", "guesses")))
	switch {
	case err == nil:
		fmt.Fprint(stdout, "The board has been told.\r\n")
	case isDoorRefusal(err):
		// A refusal is the sysop's configuration, not a fault, and a player
		// does not need to see it.
	default:
		fmt.Fprintf(stdout, "(Could not announce it: %v)\r\n", err)
	}
}

func readInt(c *door.Client, scope, key string) int {
	v, ok, err := c.StateGet(scope, key)
	if err != nil || !ok {
		return 0
	}
	// Fields before the index, because a key that exists and holds nothing is a
	// state a door has to survive rather than panic on: level-2 state is a
	// key/value store the door itself writes, and a truncated or empty write is
	// exactly what a door crashing mid-save leaves behind.
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
}

// readRecord reads the shared record, which is "<tries> <holder>".
func readRecord(c *door.Client) (int, string) {
	v, ok, err := c.StateGet(door.ScopeShared, guessRecordKey)
	if err != nil || !ok {
		return 0, ""
	}
	tries, holder, _ := strings.Cut(v, " ")
	n, err := strconv.Atoi(tries)
	if err != nil {
		return 0, ""
	}
	if holder == "" {
		holder = "someone"
	}
	return n, holder
}

// ---------------------------------------------------------------------------
// arena — a league two boards can actually play (§9.5)
// ---------------------------------------------------------------------------

const (
	// arenaGame is the name every board in this league must type identically.
	// It is a key, matched exactly, and bounded at 16 bytes by the record codec
	// — not a title, and never rendered as one.
	arenaGame = "arena"

	// The door's own bookkeeping, in level-2 SHARED state rather than per-user.
	//
	// Shared because both of these are facts about the league, not about the
	// player at the keyboard: where this board has read to, and what it counted
	// on the way. Keeping a cursor per user would mean the first player of the
	// evening consumed the events and the second saw an empty table.
	arenaCursorKey = "league-cursor"
	arenaTableKey  = "league-table"

	// arenaMaxTracked bounds the standings this door keeps.
	//
	// A league is other people's boards, so the number of names arriving is not
	// something this door controls, and level-2 state has a quota — a table that
	// grew with the league would eventually fail to save, silently, on whichever
	// launch crossed the line. Twenty-four fits a screen and leaves the quota
	// room to spare; the ones dropped are the ones with the fewest fights.
	arenaMaxTracked = 24

	// arenaDie is the die both fighters roll. Twenty, because the game is not
	// the point.
	arenaDie = 20
)

// Event kinds, from the ACTOR's point of view.
//
// Door-defined and door-interpreted: the BBS never looks inside a kind, which
// is why a receiving board that does not recognise one must still render it
// rather than discard it. Two are enough here — a fight has a winner.
const (
	arenaWon  uint8 = 1
	arenaLost uint8 = 2
)

// runArenaDoor plays one fight and reports it to the league.
//
// # What this door is for
//
// Levels 1 to 3, like guess, plus the §9.5 league grant — which is a separate
// sysop decision from the announce area and fails separately, so the interesting
// path here is the refusal. A board with no league area gets a Forbidden from
// both emit and poll, and the honest ending is to say the board is not in a
// league rather than to print an empty table.
//
// # Why it reports before it reads
//
// Reporting is queueing: the batch leaves when the league area's share of the
// airtime budget allows, which is not now. So the result this player just
// produced is deliberately NOT in the table they are about to see, and pretending
// otherwise — by folding the local fight into the standings — would be a door
// showing a player something the rest of the league has not been told.
func runArenaDoor(stdin io.Reader, stdout io.Writer) error {
	c, err := door.Open()
	if err != nil {
		return err
	}
	defer c.Close()

	sess, err := c.Session()
	if err != nil {
		return err
	}
	in := bufio.NewReader(stdin)

	fmt.Fprint(stdout, "\r\nARENA — one fight, reported to the league.\r\n")
	if sess.Handle == "" {
		// The server refuses this too, and would say so clearly. Asking first
		// is not distrust of that refusal: a guest cannot be given an account
		// mid-fight, so the only thing playing would achieve is a result nobody
		// can be credited with.
		fmt.Fprint(stdout, "\r\nYou are a guest, and a league result has to belong to somebody.\r\n"+
			"Log in and come back.\r\n")
		return nil
	}
	if c.Level() < 3 {
		fmt.Fprint(stdout, "\r\nThis door was installed without the API level a league needs.\r\n"+
			"Ask the sysop for level 3 and a league area.\r\n")
		return nil
	}

	fmt.Fprint(stdout, "\r\nWho do you fight? A name for somebody on this board, or\r\n"+
		"'bob@pnw' for somebody on another one. Enter alone fights the dummy.\r\n\r\n> ")
	line, err := in.ReadString('\n')
	if err != nil {
		return nil // the player hung up; not this door's problem to report
	}
	opponent := strings.TrimSpace(line)

	won, margin, err := arenaFight(stdout, sess.Handle, opponent)
	if err != nil {
		return err
	}

	if !reportArena(c, stdout, opponent, won, margin) {
		return nil
	}
	showArenaStandings(c, stdout)
	return nil
}

// arenaFight rolls one fight and narrates it.
//
// A tie goes to the opponent, which avoids a re-roll loop and costs the game
// nothing it had.
func arenaFight(stdout io.Writer, me, opponent string) (won bool, margin int, err error) {
	mine, err := secretNumber(arenaDie)
	if err != nil {
		return false, 0, err
	}
	theirs, err := secretNumber(arenaDie)
	if err != nil {
		return false, 0, err
	}

	them := opponent
	if them == "" {
		them = "the dummy"
	}
	fmt.Fprintf(stdout, "\r\n%s rolls %d. %s rolls %d.\r\n", me, mine, them, theirs)

	won = mine > theirs
	margin = mine - theirs
	if !won {
		margin = theirs - mine
		fmt.Fprintf(stdout, "%s wins by %d.\r\n", them, margin)
		return false, margin, nil
	}
	fmt.Fprintf(stdout, "You win by %d.\r\n", margin)
	return true, margin, nil
}

// reportArena emits the result and reports whether the league is worth reading.
//
// The false return is "this board is not in a league": emit was refused for
// want of a league area, so poll will be refused for exactly the same reason,
// and asking again only produces a second copy of the same message. A rate
// limit is different — the standings are still there to read — so it returns
// true.
func reportArena(c *door.Client, stdout io.Writer, opponent string, won bool, margin int) bool {
	kind := arenaLost
	if won {
		kind = arenaWon
	}
	// One byte of payload, and it is a real use of the field rather than a
	// decoration: the margin is this game's own business, the BBS never looks
	// inside it, and showArenaStandings reads it back out at the far end.
	queued, notice, err := c.EmitEvent(arenaGame, kind, opponent, []byte{byte(margin)})
	if err == nil {
		if notice != "" {
			// §9.1.1's one-time notice. A door that swallows it is a door to
			// uninstall — the BBS cannot force this, which is exactly why a
			// reference door has to show what showing it looks like.
			fmt.Fprintf(stdout, "\r\n%s\r\n", notice)
		}
		if queued {
			// "Queued", not "sent". Nothing has been signed yet and the batch
			// leaves when the budget allows, so a door that says otherwise is
			// making a promise on the mesh's behalf.
			fmt.Fprint(stdout, "\r\nReported. It goes out with the next batch, whenever the\r\n"+
				"league's share of the airtime budget allows.\r\n")
		}
		return true
	}

	apiErr := refusal(err)
	switch {
	case apiErr == nil:
		// A transport failure, not an answer. Shown rather than swallowed: the
		// player should know their fight went nowhere.
		fmt.Fprintf(stdout, "\r\n(Could not report the result: %v)\r\n", err)
		return true

	case apiErr.Forbidden():
		fmt.Fprint(stdout, "\r\nThis board is not in an arena league, so there was nowhere to\r\n"+
			"report that and nothing to read back. Ask the sysop for a league\r\n"+
			"area — a FEDERATED door area, which is not the same thing as the\r\n"+
			"local area a door announces to.\r\n")
		return false

	case apiErr.RateLimited():
		fmt.Fprintf(stdout, "\r\nThis door has reported all it may this hour, so that fight\r\n"+
			"stays on this board. (%v)\r\n", err)
		return true

	case apiErr.Code == "bad_request":
		// Almost always a name the board could not resolve, which is the
		// player's typo rather than anything wrong. Compared as the documented
		// wire string because that is what a door in any other language has to
		// compare — the Go client offers helpers for the three refusals a door
		// usually branches on, and this is not one of them.
		fmt.Fprintf(stdout, "\r\nThe board would not take that result: %v\r\n"+
			"If you named an opponent on another board, try 'nick@node'.\r\n", err)
		return true

	default:
		fmt.Fprintf(stdout, "\r\n(The board refused the result: %v)\r\n", err)
		return true
	}
}

// showArenaStandings folds everything new on the league into the saved table and
// prints it.
//
// # Why the door keeps both a cursor and a tally
//
// The BBS tracks no per-door read position, deliberately (§9.5): the poll stays
// stateless, and two players in the door at once cannot fight over one position.
// So a door that wants a running table keeps both halves itself — where it read
// to, and what it had counted by then.
//
// Recomputing the whole table from cursor zero on every launch would work today
// and quietly stop working later: retention prunes old records, and the day it
// does, a from-scratch tally silently loses every fight older than the window.
// The saved tally is what survives that; Truncated is how the door finds out it
// happened.
func showArenaStandings(c *door.Client, stdout io.Writer) {
	cursor, table := readArenaTable(c)

	batch, err := c.PollEvents(arenaGame, cursor)
	if err != nil {
		fmt.Fprintf(stdout, "\r\n(Could not read the league: %v)\r\n", err)
		return
	}
	if batch.Truncated {
		// Never silent. The gap is permanent — no cursor will ever fetch those
		// records again — so the table below is missing fights, and a door that
		// prints it without saying so is presenting an incomplete league as a
		// complete one.
		fmt.Fprint(stdout, "\r\nSome results were pruned before this board read them.\r\n"+
			"The table below is missing fights that cannot be recovered.\r\n")
	}
	for _, ev := range batch.Events {
		table = arenaRecord(table, ev)
	}
	if len(batch.Events) > 0 || batch.Cursor != cursor {
		if err := saveArenaTable(c, batch.Cursor, table); err != nil {
			// The table still prints. What is lost is the memory of it, so the
			// next launch re-reads from an older cursor — wasteful, not wrong,
			// and worth saying because the cause is usually a quota a sysop can
			// raise.
			fmt.Fprintf(stdout, "\r\n(Could not save the standings: %v)\r\n", err)
		}
	}
	printArenaTable(stdout, table)
}

// arenaFighter is one line of the standings.
type arenaFighter struct {
	// Name is the nick as the fighter's OWN board knows them, and Node is that
	// board. Both are needed to identify somebody: two boards in one league can
	// each have an alice, and only the node ID tells them apart. It is the ID
	// rather than a local petname because petnames do not travel (§6.1.4.1).
	Name string
	Node string

	Wins   int
	Losses int
	// Best is the largest winning margin this fighter has reported, read out of
	// the door-defined payload.
	Best int
}

// arenaRecord folds one polled event into the table.
//
// # Why only the actor is counted
//
// Each board reports its own players' fights, so counting the actor counts every
// fight exactly once. Crediting the target as well would double-count any league
// where both boards report — and would let one board write another board's
// record, which is precisely the trust §9.5 declines to extend: a target is a
// CLAIM by the origin node, not a verified fact, and a league that needs more
// than that needs game-level design rather than a signature.
func arenaRecord(table []arenaFighter, ev door.PolledDoorEvent) []arenaFighter {
	if ev.Actor == "" {
		return table
	}
	// The payload belongs to whichever door wrote it, which in a league is not
	// necessarily this one. Anything unreadable or unexpected is skipped rather
	// than trusted: an event from a board running a different version of this
	// game is a normal thing to receive.
	margin := 0
	if p, err := ev.PayloadBytes(); err == nil && len(p) > 0 {
		margin = int(p[0])
	}

	for i := range table {
		if table[i].Name != ev.Actor || table[i].Node != ev.Origin {
			continue
		}
		switch ev.Kind {
		case arenaWon:
			table[i].Wins++
			if margin > table[i].Best {
				table[i].Best = margin
			}
		case arenaLost:
			table[i].Losses++
		default:
			// An unknown kind is not an error: kinds are door-defined and a
			// league member may have added one. It counts as a fight fought and
			// nothing more, which is the forward-compatible reading.
		}
		return table
	}

	f := arenaFighter{Name: ev.Actor, Node: ev.Origin}
	switch ev.Kind {
	case arenaWon:
		f.Wins, f.Best = 1, margin
	case arenaLost:
		f.Losses = 1
	}
	return append(table, f)
}

// printArenaTable draws the standings, best first.
func printArenaTable(stdout io.Writer, table []arenaFighter) {
	if len(table) == 0 {
		fmt.Fprint(stdout, "\r\nNothing has arrived on this league yet. Results cross the mesh\r\n"+
			"in batches and can take hours; check back.\r\n")
		return
	}
	sortArena(table)

	fmt.Fprint(stdout, "\r\nTHE LEAGUE\r\n")
	fmt.Fprintf(stdout, "%-16s %-14s %4s %4s %5s\r\n", "FIGHTER", "BOARD", "W", "L", "BEST")
	for i, f := range table {
		if i >= arenaMaxTracked {
			break
		}
		fmt.Fprintf(stdout, "%-16s %-14s %4d %4d %5d\r\n",
			clip(f.Name, 16), clip(f.Node, 14), f.Wins, f.Losses, f.Best)
	}
	fmt.Fprint(stdout, "\r\nA name here is a nick on the board beside it, not on this one.\r\n")
}

// sortArena orders the table, and orders it TOTALLY.
//
// Wins, then fewest losses, then name, then board. The last two are not
// decoration: without them two fighters with identical records would print in
// whatever order the log happened to arrive in, and the standings would appear
// to reshuffle themselves between launches for no reason a player could see.
func sortArena(table []arenaFighter) {
	sort.Slice(table, func(i, j int) bool {
		a, b := table[i], table[j]
		switch {
		case a.Wins != b.Wins:
			return a.Wins > b.Wins
		case a.Losses != b.Losses:
			return a.Losses < b.Losses
		case a.Name != b.Name:
			return a.Name < b.Name
		default:
			return a.Node < b.Node
		}
	})
}

// readArenaTable loads the cursor and the standings from level-2 state.
//
// A missing or unreadable value starts over from cursor zero rather than
// failing. Starting over costs a re-read of what the node still holds; refusing
// to run would cost the player the door.
func readArenaTable(c *door.Client) (int64, []arenaFighter) {
	cursor := int64(readInt(c, door.ScopeShared, arenaCursorKey))

	v, ok, err := c.StateGet(door.ScopeShared, arenaTableKey)
	if err != nil || !ok {
		return cursor, nil
	}
	return cursor, parseArenaTable(v)
}

// parseArenaTable reads the standings back out of level-2 state.
//
// A line that does not parse is SKIPPED rather than failing the read. Level-2
// state is a string this door wrote, and the ways it can come back wrong — a
// half-written value from a door killed on its wall clock, a value truncated by
// a quota — all produce one bad line among good ones. Losing that line costs a
// fighter's tally; refusing the whole value costs the league table.
func parseArenaTable(saved string) []arenaFighter {
	var table []arenaFighter
	for _, line := range strings.Split(saved, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 {
			continue
		}
		wins, err1 := strconv.Atoi(fields[2])
		losses, err2 := strconv.Atoi(fields[3])
		best, err3 := strconv.Atoi(fields[4])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		table = append(table, arenaFighter{
			Name: fields[0], Node: fields[1], Wins: wins, Losses: losses, Best: best,
		})
	}
	return table
}

// formatArenaTable renders the standings for level-2 state, best first and
// bounded.
//
// Space-separated, which is safe only because both fields that could contain
// one cannot: a nick is bounded and space-free by the rules of the board that
// issued it, and a node ID is base32. A format that had to quote would be a
// parser, and a door's saved state is not the place to grow one.
func formatArenaTable(table []arenaFighter) string {
	sortArena(table)
	if len(table) > arenaMaxTracked {
		// Dropped from the bottom, where "bottom" is the sort order above:
		// fewest wins. The quota is a real ceiling and a league is other
		// people's boards, so something has to go, and the least interesting
		// line is the honest choice.
		table = table[:arenaMaxTracked]
	}
	var sb strings.Builder
	for _, f := range table {
		fmt.Fprintf(&sb, "%s %s %d %d %d\n", f.Name, f.Node, f.Wins, f.Losses, f.Best)
	}
	return sb.String()
}

// saveArenaTable writes the cursor and the standings back.
//
// The cursor is written LAST, and that ordering is the whole of the crash
// story: a door that saved the cursor first and then failed to save the table
// would have recorded that it read events it did not count, and those fights
// would be gone from the standings forever. This way the same events are read
// twice and counted once more than they should be — which the next launch
// corrects, because the table it re-reads is the one that saved.
func saveArenaTable(c *door.Client, cursor int64, table []arenaFighter) error {
	if err := c.StateSet(door.ScopeShared, arenaTableKey, formatArenaTable(table)); err != nil {
		return err
	}
	return c.StateSet(door.ScopeShared, arenaCursorKey, strconv.FormatInt(cursor, 10))
}

// clip shortens a field to fit its column, since a nick from another board is
// bounded by that board's rules and this one's table is 80 columns.
//
// By runes rather than bytes. A nick that arrived from another board is that
// board's idea of a valid nick, so cutting at a byte offset would eventually
// split a multi-byte character and put a replacement glyph in the table.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// refusal returns the BBS's refusal, or nil if the error was a fault.
//
// Separate from isDoorRefusal, which answers the yes/no question guess needs.
// Arena has to tell two refusals apart — "no league here" ends the door, "too
// many this hour" does not — and that needs the code rather than a boolean.
func refusal(err error) *door.APIError {
	var apiErr *door.APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return nil
}

// isDoorRefusal reports whether the BBS said no rather than broke.
func isDoorRefusal(err error) bool {
	var apiErr *door.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Forbidden() || apiErr.RateLimited()
}

// secretNumber picks a number from 1 to max that the player cannot predict.
func secretNumber(max int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0, fmt.Errorf("pick a number: %w", err)
	}
	return int(n.Int64()) + 1, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
