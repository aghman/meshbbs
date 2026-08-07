package sshd

import (
	"context"
	"fmt"

	"github.com/aghman/meshbbs/internal/term"
	"github.com/aghman/meshbbs/internal/tui"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/bubbletea"
)

// sessionMiddleware builds the per-connection handler.
func (s *Server) sessionMiddleware() wish.Middleware {
	return bubbletea.MiddlewareWithProgramHandler(s.programHandler, termenvProfile)
}

// termenvProfile is the colour profile assumed for sessions. ANSI256 is the
// safe middle ground: modern terminals handle it, and legacy BBS clients
// degrade to their 16 colours rather than receiving truecolor escapes they
// cannot parse.
const termenvProfile = 2 // termenv.ANSI256

// programHandler builds the Bubble Tea program for one session.
func (s *Server) programHandler(sess ssh.Session) *tea.Program {
	pty, winCh, active := sess.Pty()
	if !active {
		// No PTY means a scripted client (or SFTP, which is handled
		// elsewhere). A BBS is a terminal application, so say so plainly
		// rather than presenting a broken screen.
		fmt.Fprintln(sess, "meshbbs needs an interactive terminal.")
		fmt.Fprintln(sess, "Connect without -T, or use `sftp` for file transfer.")
		_ = sess.Exit(1)
		return nil
	}

	d, ok := decisionFrom(sess)
	if !ok {
		fmt.Fprintln(sess, "internal error: no authentication result")
		_ = sess.Exit(1)
		return nil
	}

	// §5.4: guess the encoding, but let it be overridden. The guess is wrong
	// often enough that signup offers the choice.
	env := map[string]string{}
	for _, kv := range sess.Environ() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	encoding := term.DetectEncoding(pty.Term, env)

	width, height := pty.Window.Width, pty.Window.Height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}

	model := tui.New(tui.Config{
		Service:    s.svc,
		Store:      s.store,
		Presence:   presenceAdapter{s.presence},
		Chat:       s.chat,
		Clock:      s.opts.Clock,
		Location:   s.opts.Location,
		Themes:     s.opts.Themes,
		ThemeName:  s.opts.DefaultTheme,
		Encoding:   encoding,
		Width:      width,
		Height:     height,
		SessionID:  sess.Context().SessionID(),
		Remote:     sess.RemoteAddr().String(),
		WebEnabled: s.opts.WebEnabled,
		WebURL:     s.opts.WebURL,
		SSHPort:    s.opts.Port,
		Intent:     tui.Intent(d.Intent),
		Nick:       d.Nick,
		User:       d.User,
		PublicKey:  d.PublicKey,
		KeyFP:      d.Fingerprint,
		AuthNote:   d.Reason,
		Logger:     s.log,
		Ctx:        context.Background(),
	})

	_ = winCh
	return tea.NewProgram(model,
		tea.WithInput(sess),
		tea.WithOutput(sess),
		tea.WithAltScreen(),
	)
}

// TUI adapts the tracker to the interface the session layer needs.
//
// Exported so the WEB front end joins this same tracker rather than starting a
// parallel one. That is what puts a browser user in [W] Who's Online next to
// everyone else, and it is most of what makes the web feel like part of the BBS
// instead of an adjacent website (webui.md §9).
func (p *Presence) TUI() tui.PresenceTracker { return presenceAdapter{p} }

// presenceAdapter adapts the server's Presence to the interface the TUI needs,
// so the tui package does not import sshd (which would be a cycle).
type presenceAdapter struct{ p *Presence }

func (a presenceAdapter) Join(id, nick, remote string, guest bool) int {
	return a.p.Join(id, nick, remote, guest).Node
}
func (a presenceAdapter) Leave(id string)              { a.p.Leave(id) }
func (a presenceAdapter) SetLocation(id, where string) { a.p.SetLocation(id, where) }
func (a presenceAdapter) List() []tui.Peer {
	out := make([]tui.Peer, 0)
	for _, s := range a.p.List() {
		out = append(out, tui.Peer{
			Node: s.Node, Nick: s.Nick, Guest: s.Guest, Where: s.Where,
		})
	}
	return out
}
