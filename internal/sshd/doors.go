package sshd

import (
	"context"
	"fmt"
	"io"
	"time"

	"os"

	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/tui"
	"github.com/charmbracelet/ssh"
)

// Running a door over SSH (§9.1, §9.3).
//
// This is what the connection mux was built for. The door needs the terminal
// for as long as it runs, and it cannot be handed over by cancelling Bubble
// Tea's input reader — see the long note in mux.go for why that does not work
// over SSH. So the mux lends the connection, the runner bridges it to a
// pseudo-terminal, and the borrow ends when the door does.

// doorLauncher gives a door an SSH session's terminal.
type doorLauncher struct {
	mux *connMux
	mgr *door.Manager
	// windows carries the client's window-size changes, so a full-screen door
	// redraws when the user resizes rather than staying at the size it started.
	windows <-chan ssh.Window
	// bbsName and sysopName go into a dropfile (§9.2). They belong to the
	// instance, so the launcher holds them rather than every session carrying
	// a copy through the model.
	bbsName   string
	sysopName string
}

var _ tui.DoorLauncher = (*doorLauncher)(nil)

// CanRun is always true here: this front end owns a real terminal, which is the
// whole difference between it and the browser.
func (l *doorLauncher) CanRun() (bool, string) { return true, "" }

// Launch lends the connection to a door and blocks until it is finished.
func (l *doorLauncher) Launch(ctx context.Context, d store.Door, sess tui.DoorSession) (string, error) {
	spec, err := specFor(d)
	if err != nil {
		return "", err
	}

	// Window changes are forwarded for the door's lifetime only. The channel is
	// the session's and outlives the door, so this reads from it into a channel
	// of our own and stops when the door does — otherwise every door ever run
	// on this session would still be listening.
	runCtx, stopWindows := context.WithCancel(ctx)
	defer stopWindows()
	sizes := make(chan door.Size, 1)
	if l.windows != nil {
		go func() {
			for {
				select {
				case <-runCtx.Done():
					return
				case w, ok := <-l.windows:
					if !ok {
						return
					}
					select {
					case sizes <- door.Size{Width: w.Width, Height: w.Height}:
					case <-runCtx.Done():
						return
					}
				}
			}
		}()
	}

	var res door.Result
	err = l.mux.Borrow(func(rw io.ReadWriter) error {
		var runErr error
		res, runErr = l.mgr.Run(ctx, spec, door.Session{
			RW:            rw,
			Term:          sess.Term,
			Width:         sess.Width,
			Height:        sess.Height,
			Node:          sess.Node,
			Resize:        sizes,
			Nick:          sess.Nick,
			RealName:      sess.RealName,
			ANSI:          sess.ANSI,
			Encoding:      sess.Encoding,
			Sysop:         sess.Sysop,
			BBSName:       l.bbsName,
			SysopName:     l.sysopName,
			TimeRemaining: sessionTimeRemaining(sess, spec.WallClock),
		})
		return runErr
	})
	if err != nil {
		return "", err
	}
	return doorOutcome(d.Name, res), nil
}

// sessionTimeRemaining is what a door is told it has left (§9.1).
//
// The minimum of two real limits, not the friendlier of them: the session's own
// clock, and the door's wall-clock bound, either of which will genuinely end
// the game. Reporting only the door's limit would have a player saving at the
// wrong moment when the session is the shorter of the two, and reporting only
// the session's would do the same in reverse.
func sessionTimeRemaining(sess tui.DoorSession, wallClock time.Duration) func() time.Duration {
	return func() time.Duration {
		if sess.TimeRemaining == nil {
			return wallClock
		}
		left, limited := sess.TimeRemaining()
		if !limited || left > wallClock {
			return wallClock
		}
		return left
	}
}

// doorOutcome is the line the user sees when they get the menu back.
func doorOutcome(name string, res door.Result) string {
	switch res.Stop {
	case door.StopTimeLimit:
		return fmt.Sprintf("%s ran out of time and was stopped.", name)
	case door.StopSessionEnded:
		return fmt.Sprintf("%s was stopped.", name)
	}
	if res.ExitCode != 0 {
		// Named rather than smoothed over: a door that exits non-zero has
		// usually failed to start something, and the number is what its author
		// will ask for.
		return fmt.Sprintf("%s ended with status %d.", name, res.ExitCode)
	}
	return fmt.Sprintf("%s ended.", name)
}

// specFor turns a door row into something the runner will accept.
func specFor(d store.Door) (door.Spec, error) {
	spec := door.Spec{
		Name:          d.Name,
		Path:          d.Path,
		Args:          d.Args,
		Dir:           d.Cwd,
		Env:           passthroughEnv(d.EnvPassthrough),
		Dropfile:      d.DropfileType,
		CPULimit:      d.CPULimit,
		MemLimit:      d.MemLimit,
		MaxConcurrent: d.MaxConcurrent,
		NodeLock:      d.NodeLock,
		WallClock:     d.WallClock,
		Grant: door.Grant{
			Level:           d.APILevel,
			AnnounceArea:    d.AnnounceArea,
			LeagueArea:      d.LeagueArea,
			LeaguePerHour:   d.LeaguePerHour,
			AnnouncePerHour: d.AnnouncePerHour,
			StateQuota:      d.StateQuota,
		},
	}
	return spec, nil
}

// lookupEnv is os.LookupEnv, named here so that the one place this package
// reads the server's environment is easy to find.
func lookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

// passthroughEnv resolves the server environment variables a door was
// explicitly allowed to see (§11.5, env_passthrough).
//
// A name with no value in the server's environment is dropped rather than
// passed as empty: a door checking whether a variable is set should get the
// same answer the server would.
func passthroughEnv(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if v, ok := lookupEnv(n); ok {
			out = append(out, n+"="+v)
		}
	}
	return out
}
