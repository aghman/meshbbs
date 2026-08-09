package cli

import (
	"context"
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
// Two, not three, because each has to earn its place. `hello` is the smallest
// program that is recognisably a door, for someone starting out. `guess` is a
// real game that uses levels one, two and three, so the API has proof-of-life
// rather than proof-of-concept. A third would be a variation.
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
}{
	{"hello", "hello", 1, false},
	{"guess", "guess", 3, true},
}

func newDoorExamplesCmd(e *env) *cobra.Command {
	var announceArea string

	cmd := &cobra.Command{
		Use:   "examples",
		Short: "Install the bundled reference doors, to prove the plumbing works",
		Long: `Install the reference doors that ship with meshbbs.

They run from this binary, so there is nothing to download and nothing to
build. 'hello' is the smallest program that is recognisably a door. 'guess' is
a game that reads its session, keeps a saved game and a shared high score, and
announces a new record — levels 1 to 3 of the door API.

Use them to prove a new installation works before pointing it at somebody
else's software.`,
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
				for _, ex := range exampleDoors {
					d := doorRow(ex.name, self, ex.arg, cwd, ex.apiLevel)
					if ex.announce {
						d.AnnounceArea = announceArea
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
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&announceArea, "announce-area", "",
		"local area the 'guess' door may post a new record to; must not be federated")
	return cmd
}

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
