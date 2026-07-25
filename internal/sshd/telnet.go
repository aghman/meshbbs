package sshd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/term"
	"github.com/aghman/meshbbs/internal/theme"
	"github.com/aghman/meshbbs/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

// Telnet exists because the ANSI terminal clients BBS people actually use —
// SyncTERM, NetRunner, MagiTerm — are telnet-only ([D12]).
//
// It is OFF by default and warns loudly when enabled, because credentials
// cross the wire in plaintext. Guest-only is the recommended middle setting
// and the default when it is turned on: browsing a public message base over
// plaintext costs nothing, while typing a password over it is a real loss.
type TelnetOptions struct {
	Bind      string
	Port      int
	GuestOnly bool
	Themes    *theme.Set
	Theme     string
	Chat      *tui.ChatRoom
	Presence  *Presence
	Logger    *slog.Logger
}

// TelnetServer serves the BBS over a raw socket.
type TelnetServer struct {
	opts  TelnetOptions
	svc   *bbs.Service
	store *store.Store
	ln    net.Listener
	log   *slog.Logger

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// NewTelnetServer builds the listener.
func NewTelnetServer(svc *bbs.Service, st *store.Store, opts TelnetOptions) *TelnetServer {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &TelnetServer{
		opts: opts, svc: svc, store: st, log: opts.Logger,
		conns: map[net.Conn]struct{}{},
	}
}

// ListenAndServe runs until the context is cancelled.
func (t *TelnetServer) ListenAndServe(ctx context.Context) error {
	addr := net.JoinHostPort(t.opts.Bind, fmt.Sprint(t.opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("telnet listen: %w", err)
	}
	t.ln = ln

	// §11.3 and [D12]: the warning goes in the log every start, not just in
	// the docs, so a sysop who enabled this months ago is reminded.
	t.log.Warn("TELNET IS ENABLED — passwords and messages cross the network in plaintext",
		"addr", addr, "guest_only", t.opts.GuestOnly)
	if !t.opts.GuestOnly {
		t.log.Warn("telnet is accepting logins; consider guest_only, which lets people " +
			"browse without ever sending a password in the clear")
	}

	go func() {
		<-ctx.Done()
		ln.Close()
		t.mu.Lock()
		for c := range t.conns {
			c.Close()
		}
		t.mu.Unlock()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go t.handle(ctx, conn)
	}
}

// Close stops the listener.
func (t *TelnetServer) Close() error {
	if t.ln != nil {
		return t.ln.Close()
	}
	return nil
}

func (t *TelnetServer) handle(ctx context.Context, conn net.Conn) {
	t.mu.Lock()
	t.conns[conn] = struct{}{}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.conns, conn)
		t.mu.Unlock()
		conn.Close()
	}()

	// Negotiate character-at-a-time with no echo, which is what a full-screen
	// TUI needs. IAC WILL ECHO, IAC WILL SUPPRESS-GO-AHEAD, IAC DO NAWS.
	_, _ = conn.Write([]byte{
		255, 251, 1, // IAC WILL ECHO
		255, 251, 3, // IAC WILL SUPPRESS_GO_AHEAD
		255, 253, 31, // IAC DO NAWS (window size)
	})

	fmt.Fprint(conn, "\r\n")
	fmt.Fprint(conn, "*** This is a PLAINTEXT connection. ***\r\n")
	fmt.Fprint(conn, "Anything you type, including a password, can be read by anyone\r\n")
	fmt.Fprint(conn, "on the network path. For a private connection use SSH instead.\r\n\r\n")

	if !t.opts.GuestOnly {
		fmt.Fprint(conn, "Logins are permitted here, but consider registering over SSH:\r\n")
		fmt.Fprint(conn, "    ssh new@<this host>\r\n\r\n")
	}

	// Telnet sessions are guest-only in this build. Accepting a password over
	// plaintext is exactly the thing [D12] warns about, and the SSH path is
	// one command away — so the honest move is to offer reading, not logins.
	sessionID := fmt.Sprintf("telnet-%p", conn)
	model := tui.New(tui.Config{
		Service: t.svc, Store: t.store,
		Presence: presenceAdapter{t.opts.Presence},
		Chat:     t.opts.Chat,
		Themes:   t.opts.Themes, ThemeName: t.opts.Theme,
		// Telnet clients are the legacy ANSI ones, so assume CP437 unless a
		// user later says otherwise (§5.4).
		Encoding: term.EncodingCP437,
		Width:    80, Height: 24,
		SessionID: sessionID,
		Remote:    conn.RemoteAddr().String(),
		Intent:    tui.IntentGuest,
		Nick:      "guest",
		Ctx:       ctx,
	})

	p := tea.NewProgram(model,
		tea.WithInput(&telnetReader{conn: conn}),
		tea.WithOutput(conn),
	)
	if _, err := p.Run(); err != nil {
		t.log.Debug("telnet session ended", "err", err)
	}
}

// telnetReader strips telnet IAC command sequences from the byte stream.
//
// Without this, a client's negotiation bytes arrive as keystrokes and the TUI
// sees garbage — the classic symptom being a menu that reacts to keys nobody
// pressed.
type telnetReader struct {
	conn net.Conn
	buf  []byte
}

func (r *telnetReader) Read(p []byte) (int, error) {
	tmp := make([]byte, len(p))
	n, err := r.conn.Read(tmp)
	if n == 0 {
		return 0, err
	}

	out := p[:0]
	for i := 0; i < n; i++ {
		c := tmp[i]
		if c != 255 { // not IAC
			out = append(out, c)
			continue
		}
		// IAC: skip the command, and its option byte for WILL/WONT/DO/DONT.
		if i+1 >= n {
			break
		}
		cmd := tmp[i+1]
		switch cmd {
		case 255: // escaped 0xFF
			out = append(out, 255)
			i++
		case 251, 252, 253, 254: // WILL WONT DO DONT
			i += 2
		case 250: // SB ... SE
			for i < n && !(tmp[i] == 255 && i+1 < n && tmp[i+1] == 240) {
				i++
			}
			i++
		default:
			i++
		}
	}
	return len(out), err
}
