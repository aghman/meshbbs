package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/aghman/meshbbs/internal/door/example"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// Reference doors (§9.1).
//
// "Ship two or three reference doors with the binary so the API has
// proof-of-life." They are SUBCOMMANDS of meshbbs rather than separate
// programs, which keeps §10's single-static-binary story intact — there is
// still one artifact per platform — and means they are launched through exactly
// the machinery a third-party door goes through: a pseudo-terminal, an
// environment with nothing inherited, a session descriptor, and a socket.
//
// Three, because each has to earn its place. `hello` is the smallest program
// that is recognisably a door, for someone starting out. `guess` is a real game
// that uses levels one, two and three, so the API has proof-of-life rather than
// proof-of-concept. `arena` is the only one that uses §9.5's league operations,
// and it is here because no single-board door can demonstrate them: they need a
// federated door area, a grant separate from the announce one, and a second BBS
// at the far end. A sysop debugging a league wants that proven with software
// that is not also the suspect.
//
// Hidden from the help, because typing one at a shell prompt gets you a program
// that says it was not launched by a BBS. `meshbbs door examples` installs them,
// which is the discoverable half.

func newDoorExampleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "door-example <name>",
		Short:  "Run a bundled reference door (launched by the BBS, not by hand)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return example.Run(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	return cmd
}

// exampleDoors is what `door examples` installs.
var exampleDoors = []struct {
	name     string
	arg      string
	apiLevel int
	announce bool
	league   bool
}{
	{"hello", "hello", 1, false, false},
	{"guess", "guess", 3, true, false},
	{"arena", "arena", 3, false, true},
}

func newDoorExamplesCmd(e *env) *cobra.Command {
	var announceArea, leagueArea string

	cmd := &cobra.Command{
		Use:   "examples",
		Short: "Install the bundled reference doors, to prove the plumbing works",
		Long: `Install the reference doors that ship with meshbbs.

They run from this binary, so there is nothing to download and nothing to
build. 'hello' is the smallest program that is recognisably a door. 'guess' is
a game that reads its session, keeps a saved game and a shared high score, and
announces a new record — levels 1 to 3 of the door API. 'arena' fights one
round, reports the result to an inter-BBS league, and prints the standings it
has heard from the other boards (§9.5).

Use them to prove a new installation works before pointing it at somebody
else's software.

The two areas they want are not interchangeable. --announce-area must be a
LOCAL area, because a door may not spend the mesh's airtime on its own say-so.
--league-area must be a FEDERATED door area, because crossing boards is the
entire feature. Either can be left out; the door that wanted it says so and
keeps working.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			self, err := os.Executable()
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			return withDoorStore(cmd, e, func(ctx context.Context, st *store.Store) error {
				out := cmd.OutOrStdout()
				// Checked BEFORE anything is installed, so a mistyped league
				// leaves the doors table as it was rather than half-updated with
				// a grant that points at nothing.
				if err := checkLeagueArea(ctx, st, leagueArea); err != nil {
					return err
				}

				for _, ex := range exampleDoors {
					d := doorRow(ex.name, self, ex.arg, cwd, ex.apiLevel)
					if ex.announce {
						d.AnnounceArea = announceArea
					}
					if ex.league {
						d.LeagueArea = leagueArea
						// A per-hour of zero refuses everything, so the row has
						// to carry a real number even when no league area is set
						// — otherwise a sysop who adds one later finds the door
						// rate-limited out of its first fight and no flag
						// explains why.
						d.LeaguePerHour = exampleLeaguePerHour
					}
					if err := st.PutDoor(ctx, d, cliActor); err != nil {
						return err
					}
					fmt.Fprintf(out, "Installed %s (API level %d).\n", d.Name, d.APILevel)
				}
				if announceArea == "" {
					fmt.Fprintf(out, "\nNo announce area set, so 'guess' will keep its high "+
						"score and not post about it.\nGive it one with --announce-area, "+
						"pointing at a LOCAL area: a door may not\nspend the mesh's airtime.\n")
				}
				if leagueArea == "" {
					// The same shape as the announce note above, because it is
					// the same situation: the sysop has not chosen a
					// destination, which is a different thing from a rate limit
					// of zero, and the door is told which.
					fmt.Fprintf(out, "\nNo league area set, so 'arena' will fight and have "+
						"nowhere to report it.\nGive it one with --league-area, pointing at a "+
						"FEDERATED door area:\n\n  meshbbs area create arena --kind door --federated\n\n"+
						"Every board in the league needs that area under the same name — the\n"+
						"name is what derives the wire tag, and there is no registry to ask.\n")
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&announceArea, "announce-area", "",
		"local area the 'guess' door may post a new record to; must not be federated")
	cmd.Flags().StringVar(&leagueArea, "league-area", "",
		"federated door area the 'arena' door reports game results to (§9.5)")
	return cmd
}

// checkLeagueArea refuses a league that could never work, and says how to make
// one that would.
//
// The runtime refuses all three of these itself, so this check adds no safety —
// it moves the answer from "the door prints something odd on a winter evening
// when somebody finally plays it" to "the install command said so". That is the
// whole job of `door examples`: a sysop runs it to find out whether the plumbing
// is right, and a command that cheerfully installs a grant pointing at nothing
// has failed at exactly that.
func checkLeagueArea(ctx context.Context, st *store.Store, name string) error {
	if name == "" {
		return nil
	}
	area, err := st.GetDoorArea(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("there is no area called %q.\n"+
			"A league is a federated door area; make one with:\n"+
			"  meshbbs area create %s --kind door --federated", name, name)
	}
	if errors.Is(err, store.ErrWrongAreaKind) {
		// Worth its own message: the name is spent, so the remedy is a
		// different name rather than a flag.
		return fmt.Errorf("%w.\nA league carries DOOR_EVENT records and nothing else, "+
			"so it cannot share a name with\na forum or a file area — pick another name for the league", err)
	}
	if err != nil {
		return err
	}
	if !area.Federated {
		return fmt.Errorf("%s is a door area but it is local only, so a league cannot "+
			"cross boards from it.\nPut it on the mesh with:\n  meshbbs area federate %s",
			area.Name, area.Name)
	}
	return nil
}

// exampleLeaguePerHour is how often the arena door may report a result.
//
// The same default `door add --league-per-hour` uses, deliberately: a reference
// door that ran under a different budget from the one a sysop gets by default
// would prove the plumbing works at a rate nothing else runs at.
const exampleLeaguePerHour = 6

// doorRow describes a reference door to the doors table.
//
// Wall clock is generous but present, because it is required and because a
// reference door is the first thing a sysop runs — one that could hang forever
// would teach exactly the wrong lesson about what a door is allowed to do.
func doorRow(name, self, arg, cwd string, apiLevel int) store.Door {
	return store.Door{
		Name: name, Path: self, Args: []string{"door-example", arg}, Cwd: cwd,
		DropfileType:    store.DropfileNone,
		WallClock:       30 * time.Minute,
		APILevel:        apiLevel,
		AnnouncePerHour: 4,
		StateQuota:      4096,
		Enabled:         true,
	}
}
