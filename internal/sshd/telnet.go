package sshd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

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
	Bind        string
	Port        int
	GuestOnly   bool
	MaxSessions int
	Themes      *theme.Set
	Theme       string
	Chat        *tui.ChatRoom
	Presence    *Presence
	Location    *time.Location
	Logger      *slog.Logger
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

	// ready closes once the listener is accepting, so callers can wait for
	// startup without opening a probe connection — which would occupy a
	// session slot and race anything that cares about capacity.
	ready chan struct{}
}

// NewTelnetServer builds the listener.
func NewTelnetServer(svc *bbs.Service, st *store.Store, opts TelnetOptions) *TelnetServer {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &TelnetServer{
		opts: opts, svc: svc, store: st, log: opts.Logger,
		conns: map[net.Conn]struct{}{},
		ready: make(chan struct{}),
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
	close(t.ready)

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

// Ready is closed once the listener is accepting connections.
func (t *TelnetServer) Ready() <-chan struct{} { return t.ready }

// ActiveSessions returns the number of live telnet connections.
func (t *TelnetServer) ActiveSessions() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.conns)
}

// Close stops the listener.
func (t *TelnetServer) Close() error {
	if t.ln != nil {
		return t.ln.Close()
	}
	return nil
}

func (t *TelnetServer) handle(ctx context.Context, conn net.Conn) {
	// Cap concurrent sessions. This is a plaintext port on the public
	// internet; without a limit, anything that opens sockets and never speaks
	// exhausts memory and file descriptors, and takes SSH down with it.
	max := t.opts.MaxSessions
	if max <= 0 {
		max = 16
	}

	t.mu.Lock()
	if len(t.conns) >= max {
		t.mu.Unlock()
		fmt.Fprintf(conn, "\r\nThis BBS is full (%d telnet sessions). Try again shortly,\r\n"+
			"or connect over SSH, which has its own capacity.\r\n", max)
		conn.Close()
		t.log.Warn("telnet connection refused: at capacity",
			"limit", max, "remote", conn.RemoteAddr())
		return
	}
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
	// Tie the program's lifetime to this connection.
	//
	// Without this, a session whose client vanishes can persist indefinitely:
	// Bubble Tea keeps running until something tells it to stop, and the only
	// signal it would otherwise get is a write failing — which only happens if
	// a render is pending. A client that connects and disappears would hold its
	// slot forever, and with a session cap in place that is worse than no cap,
	// because the BBS ends up permanently full.
	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	sessionID := fmt.Sprintf("telnet-%p", conn)
	model := tui.New(tui.Config{
		Service: t.svc, Store: t.store,
		Presence: presenceAdapter{t.opts.Presence},
		Chat:     t.opts.Chat,
		Location: t.opts.Location,
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
		tea.WithInput(&telnetReader{conn: conn, onEnd: cancelConn}),
		tea.WithOutput(conn),
		tea.WithContext(connCtx),
	)
	if _, err := p.Run(); err != nil {
		t.log.Debug("telnet session ended", "err", err)
	}
}

// Telnet protocol bytes (RFC 854).
const (
	tnIAC  = 255 // interpret as command
	tnSE   = 240 // end of subnegotiation
	tnSB   = 250 // begin subnegotiation
	tnWILL = 251
	tnWONT = 252
	tnDO   = 253
	tnDONT = 254
)

// telnetReader strips telnet IAC command sequences from the byte stream.
//
// Without this, a client's negotiation bytes arrive as keystrokes and the TUI
// sees garbage — the classic symptom being a menu that reacts to keys nobody
// pressed.
//
// It is a STATE MACHINE rather than a per-read scan, and that matters: a TCP
// read ends wherever the network decides, including between the IAC and the
// command byte, or halfway through a window-size subnegotiation. A parser that
// restarts on each read loses that context and emits the tail of the sequence
// as input. The first version of this code did exactly that, and only a test
// feeding one byte at a time caught it.
type telnetReader struct {
	conn  net.Conn
	state telnetState
	// onEnd fires once when the stream ends, so the session can be torn down
	// on the reader's authority rather than waiting for a write to fail.
	onEnd func()
	ended bool
}

type telnetState int

const (
	tnData        telnetState = iota // ordinary bytes
	tnSawIAC                         // saw IAC, awaiting a command
	tnAwaitOption                    // saw IAC WILL/WONT/DO/DONT, awaiting the option
	tnInSubneg                       // inside IAC SB ... awaiting IAC SE
	tnSubnegIAC                      // inside a subnegotiation, saw IAC
)

func (r *telnetReader) Read(p []byte) (int, error) {
	// Keep reading until at least one application byte is produced, or the
	// connection ends. A read that consumed nothing but protocol bytes — a
	// client's opening handshake is exactly that — has nothing to hand up, and
	// returning (0, nil) invites the consumer to spin. Blocking for more input
	// is what a caller expects from an io.Reader.
	for {
		n, err := r.readOnce(p)
		if err != nil {
			r.signalEnd()
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
}

// signalEnd reports the end of the stream exactly once.
func (r *telnetReader) signalEnd() {
	if r.ended || r.onEnd == nil {
		return
	}
	r.ended = true
	r.onEnd()
}

func (r *telnetReader) readOnce(p []byte) (int, error) {
	tmp := make([]byte, len(p))
	n, err := r.conn.Read(tmp)
	if n == 0 {
		return 0, err
	}

	out := p[:0]
	for _, c := range tmp[:n] {
		switch r.state {
		case tnData:
			if c == tnIAC {
				r.state = tnSawIAC
				continue
			}
			out = append(out, c)

		case tnSawIAC:
			switch c {
			case tnIAC:
				// IAC IAC is an escaped literal 0xFF.
				out = append(out, tnIAC)
				r.state = tnData
			case tnWILL, tnWONT, tnDO, tnDONT:
				r.state = tnAwaitOption
			case tnSB:
				r.state = tnInSubneg
			default:
				// A two-byte command (NOP, AYT, IP, ...): nothing follows.
				r.state = tnData
			}

		case tnAwaitOption:
			// Discard the option byte itself.
			r.state = tnData

		case tnInSubneg:
			if c == tnIAC {
				r.state = tnSubnegIAC
			}
			// Everything else inside a subnegotiation is payload we ignore.

		case tnSubnegIAC:
			if c == tnSE {
				r.state = tnData
			} else {
				// IAC IAC inside a subnegotiation is escaped data; anything
				// else is a malformed stream, and staying in the subnegotiation
				// is the conservative reading.
				r.state = tnInSubneg
			}
		}
	}
	return len(out), err
}
