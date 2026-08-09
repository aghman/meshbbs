package cli

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/aghman/meshbbs/internal/door"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// newDevDoorRunCmd runs one door against the operator's own terminal.
//
// It exists because the hard part of door support is not the BBS: it is whether
// a pseudo-terminal behaves on the machine in front of you (§9.3), and that is
// the one thing a test suite on somebody else's platform cannot tell you. This
// takes the whole BBS out of the picture — no database, no session, no capability
// grant — so that when a door misbehaves there are only two things it can be.
//
// It is also the only way to try ConPTY by hand. The Windows runner in CI proves
// the code paths execute; it cannot tell anyone whether the screen looks right.
func newDevDoorRunCmd(e *env) *cobra.Command {
	var (
		dir       string
		wallClock time.Duration
		doorEnv   []string
	)

	cmd := &cobra.Command{
		Use:   "door-run <executable> [args...]",
		Short: "Run one door against this terminal, with no BBS around it",
		Long: `Run a door directly, bridged to this terminal.

The door gets a real pseudo-terminal, the working directory and environment it
would get from the BBS, and the wall-clock limit you pass. It does NOT get a
session descriptor or the door API socket: this command answers "does this
program run here", not "does this door work".

Your terminal is put into raw mode for the duration and restored afterwards,
including if the door crashes.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if dir == "" {
				dir = filepath.Dir(path)
			}
			dir, err = filepath.Abs(dir)
			if err != nil {
				return err
			}

			fd := int(os.Stdin.Fd())
			if !term.IsTerminal(fd) {
				return fmt.Errorf("door-run needs a terminal; it is bridging one to the door")
			}

			width, height, err := term.GetSize(fd)
			if err != nil {
				width, height = 80, 24
			}

			// Raw mode, because the door owns the keyboard now: line
			// discipline, echo and Ctrl+C all belong to it rather than to the
			// shell that launched this.
			state, err := term.MakeRaw(fd)
			if err != nil {
				return fmt.Errorf("put the terminal into raw mode: %w", err)
			}
			defer func() { _ = term.Restore(fd, state) }()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()

			mgr := door.New(e.clock, slog.New(slog.NewTextHandler(os.Stderr,
				&slog.HandlerOptions{Level: slog.LevelDebug})))

			res, err := mgr.Run(ctx, door.Spec{
				Name:      filepath.Base(path),
				Path:      path,
				Args:      args[1:],
				Dir:       dir,
				Env:       doorEnv,
				WallClock: wallClock,
			}, door.Session{
				RW:     rawTerminal{},
				Term:   os.Getenv("TERM"),
				Width:  width,
				Height: height,
				Node:   1,
			})
			if err != nil {
				return err
			}

			// Restore before reporting, or the summary prints down a staircase.
			_ = term.Restore(fd, state)
			fmt.Fprintf(cmd.OutOrStdout(), "\r\n%s: exit %d after %s (%s)\n",
				filepath.Base(path), res.ExitCode, res.Runtime.Round(time.Millisecond), res.Stop)
			return nil
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "",
		"working directory for the door (default: the directory holding it)")
	cmd.Flags().DurationVar(&wallClock, "limit", 10*time.Minute,
		"wall-clock limit; the door is killed when it runs out")
	cmd.Flags().StringArrayVar(&doorEnv, "env", nil,
		"KEY=VALUE passed to the door, repeatable (it inherits nothing else)")
	return cmd
}

// rawTerminal is the operator's own terminal as a door session.
//
// Deliberately not os.Stdin/os.Stdout as an io.ReadWriter pair built inline:
// the door's output must go to stdout while its input comes from stdin, and a
// struct that embeds *os.File twice would resolve both to whichever came first.
type rawTerminal struct{}

func (rawTerminal) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (rawTerminal) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
