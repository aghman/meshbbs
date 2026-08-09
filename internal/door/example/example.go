// Package example holds the reference doors that ship with meshbbs (§9.1).
//
// A package of its own rather than part of the command tree, because two
// binaries need them: the real one, where `meshbbs door-example guess` is what
// the doors table points at, and the SSH test binary, which launches a
// reference door over a real connection to prove the contract end to end. The
// command tree cannot be imported by internal/sshd — internal/cli imports it
// already — so the doors live where both can reach them.
//
// # Why these two
//
// §9.1 asks for "two or three reference doors so the API has proof-of-life".
// Each has to earn its place. Hello is the smallest program that is
// recognisably a door, for somebody starting out. Guess is a real game that
// uses levels one, two and three, so the API has proof-of-life rather than
// proof-of-concept. A third would be a variation on one of them.
//
// # What a door author should take from these
//
// Not the game. The shape: open the connection, ask what level you were
// granted, and treat every refusal as an answer rather than a fault. Guess
// works at level 1, saves scores if it has 2, and announces if it has 3 — a
// door that assumes its grant and crashes without it is a door a sysop cannot
// safely install.
package example

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/aghman/meshbbs/internal/door"
)

// Names are the reference doors this package can run.
var Names = []string{"hello", "guess"}

// Run plays a reference door by name.
func Run(name string, stdin io.Reader, stdout io.Writer) error {
	switch name {
	case "hello":
		return runHelloDoor(stdin, stdout)
	case "guess":
		return runGuessDoor(stdin, stdout)
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
	n, err := strconv.Atoi(strings.Fields(v)[0])
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
