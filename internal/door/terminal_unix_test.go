//go:build !windows

package door

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// platformHelper adds the door modes that need a Unix ioctl.
func platformHelper(mode, _ string) bool {
	if mode != "winsize" {
		return false
	}
	// A full-screen door redraws on SIGWINCH, which is the whole reason the
	// runner forwards window changes at all.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	report := func() {
		rows, cols, err := pty.Getsize(os.Stdin)
		if err != nil {
			fmt.Printf("SIZEERR %v\r\n", err)
			return
		}
		fmt.Printf("SIZE %dx%d\r\n", cols, rows)
	}
	report()
	for range winch {
		report()
	}
	return true
}

// A door has to be told when the window changes, or a full-screen one draws to
// the size it was launched at for the rest of the session.
func TestResizeReachesTheDoor(t *testing.T) {
	sizes := make(chan Size, 1)
	h := newHarness(t, realClock(), "winsize", "")
	h.sess.Resize = sizes

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := h.mgr.Run(ctx, h.spec, h.session()); err != nil {
			t.Errorf("run: %v", err)
		}
	}()

	// The size the door was launched at.
	h.await(t, "SIZE 80x24", 15*time.Second)

	sizes <- Size{Width: 132, Height: 50}
	h.await(t, "SIZE 132x50", 15*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the session ended")
	}
}

// A terminal is an io.Reader, and an io.Reader signals the end of its stream
// with io.EOF.
//
// A pty does not: when the last process holding the slave side exits, Linux
// fails the master's read with EIO. Passing that up would make io.Copy report a
// failure for the most ordinary thing a door can do — finish.
//
// This test only has teeth on Linux, and that is worth knowing rather than
// discovering. The BSDs revoke the terminal instead and the master reaches a
// genuine end of stream, so on a Mac the translation is unreachable and
// removing it changes nothing observable. The CI Linux runner is what holds the
// line here.
func TestTerminalReportsEOFWhenTheDoorExits(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	term, err := startTerminal(Spec{
		Name: "eof", Path: self, Dir: t.TempDir(),
		Env:       []string{helperEnv + "=exit", helperArgEnv + "=0"},
		WallClock: time.Minute,
	}, Session{Term: "xterm", Width: 80, Height: 24})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	copied := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, term)
		copied <- err
	}()

	select {
	case err := <-copied:
		if err != nil {
			t.Errorf("copying the door's output ended with %v, want a clean end of stream", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the terminal never reported end of stream")
	}
}
