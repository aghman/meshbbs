package webd

import (
	"context"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/tui"
)

// Doors in the browser (§9.1, [D16]).
//
// # Why there is nothing to run here
//
// A door is a program writing raw bytes at a terminal, and this front end does
// not have one. [D16] removed the terminal emulator deliberately: the model
// emits a typed Screen and the browser renders real HTML from it, which is what
// makes the web version more readable than a screenshot of a terminal rather
// than a worse copy of one. Putting xterm.js back for doors alone would
// reintroduce the dependency, a second input path carrying raw bytes alongside
// key names, and a class of screen the renderer cannot reason about.
//
// So the browser lists the doors and says how to play them. That is a
// deliberate choice between two honest options and not the third, dishonest
// one: hiding the menu entry, which would leave a browser user unable to
// discover that this BBS has door games at all — the exact drift webui.md §2
// exists to prevent, differing in what EXISTS rather than in how it is drawn.
type doorLauncher struct {
	// sshHost and sshPort are what a caller types to get a terminal. They come
	// from the SSH listener's own configuration, so the command printed here is
	// one that works rather than one that looks plausible.
	sshHost string
	sshPort int
}

var _ tui.DoorLauncher = (*doorLauncher)(nil)

// CanRun is always false: see the note above.
func (l *doorLauncher) CanRun() (bool, string) {
	return false, "Door games need a terminal, which a browser tab is not. " +
		"They are listed here so you know what this BBS has; connect over SSH to play."
}

// Launch never runs anything. It answers with the command that will.
func (l *doorLauncher) Launch(_ context.Context, d store.Door, sess tui.DoorSession) (string, error) {
	return fmt.Sprintf("%s needs a terminal. Connect with: %s",
		d.Name, l.sshCommand(sess.Nick)), nil
}

// sshCommand is the invitation, with the user's own nick in it.
func (l *doorLauncher) sshCommand(nick string) string {
	var b strings.Builder
	b.WriteString("ssh ")
	if nick != "" {
		b.WriteString(nick)
		b.WriteString("@")
	}
	host := l.sshHost
	if host == "" {
		// Better than inventing one: a caller who is already in a browser knows
		// which host they typed, and a wrong hostname is worse than a gap.
		host = "<this bbs>"
	}
	b.WriteString(host)
	if l.sshPort != 0 && l.sshPort != 22 {
		fmt.Fprintf(&b, " -p %d", l.sshPort)
	}
	return b.String()
}
