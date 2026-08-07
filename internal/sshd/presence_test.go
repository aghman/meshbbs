package sshd

import (
	"context"
	"crypto/ed25519"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/theme"
	gossh "golang.org/x/crypto/ssh"
)

// A session that ends without a clean quit — a dropped network, a killed
// terminal, a client that simply goes away — must still leave [W] Who's Online.
// The model's exit path never runs for those, so anything that leaves only on
// Ctrl+C leaves a ghost in the tracker for the lifetime of the process.

// waitPresence polls until the tracker holds want sessions.
//
// Polling rather than a single check because both joining and leaving cross a
// goroutine boundary: the join runs as a Bubble Tea command, and the departure
// runs as the connection handler unwinds.
func waitPresence(t *testing.T, p *Presence, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := p.Count()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("who's-online holds %d sessions, want %d: %+v", got, want, p.List())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTelnetSessionLeavesPresenceWhenConnectionDrops(t *testing.T) {
	addr, srv := startTelnetLimited(t, true, 0)
	presence := srv.opts.Presence

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Answer the handshake as a real client would, so the session reaches the
	// menu and registers itself.
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte{
		iac, do, echo,
		iac, sb, naws, 0, 80, 0, 24, iac, se,
	}); err != nil {
		t.Fatal(err)
	}
	waitPresence(t, presence, 1, 5*time.Second)

	// Vanish. No quit key, no goodbye — just a socket that stops existing.
	conn.Close()

	waitPresence(t, presence, 0, 5*time.Second)
}

func TestSSHSessionLeavesPresenceWhenConnectionDrops(t *testing.T) {
	svc, st, themes := telnetFixture(t)

	srv, err := NewServer(svc, st, Options{
		Bind: "127.0.0.1", Port: 0, KeysDir: t.TempDir(),
		GuestEnabled: true,
		Themes:       themes, DefaultTheme: theme.DefaultName,
		Clock:    clock.NewVirtual(time.Unix(1_700_000_000, 0)),
		Location: time.UTC,
	})
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	tcp, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	_, priv, err := ed25519.GenerateKey(rng.TestSecret(9).Reader())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	c, chans, reqs, err := gossh.NewClientConn(tcp, ln.Addr().String(), &gossh.ClientConfig{
		User:            GuestUser,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := gossh.NewClient(c, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	// Hold stdin open with a pipe nobody writes to. Handing the session an
	// immediate EOF would end the program on its own, which is not the case
	// under test.
	stdin, stdinW := io.Pipe()
	defer stdinW.Close()
	sess.Stdin = stdin
	sess.Stdout = io.Discard
	sess.Stderr = io.Discard

	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	waitPresence(t, srv.Presence(), 1, 10*time.Second)

	// Yank the transport out from under the session: no exit key, no channel
	// close, nothing the model would ever see.
	tcp.Close()

	waitPresence(t, srv.Presence(), 0, 10*time.Second)
}

// A join that arrives after the connection has gone must not re-register the
// session. Joining runs in a Bubble Tea command goroutine, and Bubble Tea does
// not wait for those at shutdown, so the two can cross.
func TestSessionPresenceIgnoresJoinAfterLeave(t *testing.T) {
	p := NewPresence()
	sp := newSessionPresence(p)

	sp.Leave("session-1")

	if node := sp.Join("session-1", "nick", "127.0.0.1:1234", true); node != 0 {
		t.Errorf("join after the connection ended returned node %d, want 0", node)
	}
	if n := p.Count(); n != 0 {
		t.Errorf("who's-online holds %d sessions after a late join, want 0", n)
	}
}
