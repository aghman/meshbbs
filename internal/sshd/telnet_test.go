package sshd

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/theme"
	"github.com/aghman/meshbbs/internal/tui"
)

// Telnet command bytes, named so the tests read as protocol rather than magic
// numbers.
const (
	iac  = 255
	se   = 240
	sb   = 250
	will = 251
	wont = 252
	do   = 253
	dont = 254
	echo = 1
	naws = 31
)

// pipeConn adapts a byte slice to net.Conn, handing it over in fixed-size
// chunks so tests can control exactly how a sequence is split across reads.
type pipeConn struct {
	data      []byte
	chunk     int
	pos       int
	writeSink bytes.Buffer
}

func (c *pipeConn) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.chunk
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

func (c *pipeConn) Write(p []byte) (int, error)      { return c.writeSink.Write(p) }
func (c *pipeConn) Close() error                     { return nil }
func (c *pipeConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *pipeConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *pipeConn) SetDeadline(time.Time) error      { return nil }
func (c *pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *pipeConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }

// drain reads everything the telnetReader yields for the given input, feeding
// the underlying connection in chunks of the given size.
func drain(t *testing.T, input []byte, chunk int) []byte {
	t.Helper()
	r := &telnetReader{conn: &pipeConn{data: input, chunk: chunk}}
	var out []byte
	buf := make([]byte, 64)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			break
		}
	}
	return out
}

func TestTelnetStripsNegotiation(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		want  string
	}{
		{
			"plain text passes through",
			[]byte("hello"),
			"hello",
		},
		{
			"WILL is stripped",
			[]byte{iac, will, echo, 'h', 'i'},
			"hi",
		},
		{
			"DO/DONT/WONT are stripped",
			[]byte{iac, do, naws, 'a', iac, dont, echo, 'b', iac, wont, echo, 'c'},
			"abc",
		},
		{
			"negotiation between characters",
			[]byte{'a', iac, will, echo, 'b', iac, do, naws, 'c'},
			"abc",
		},
		{
			"escaped 0xFF becomes a literal byte",
			[]byte{'a', iac, iac, 'b'},
			"a\xffb",
		},
		{
			"subnegotiation is skipped entirely",
			// IAC SB NAWS 0 80 0 24 IAC SE — a window size report.
			[]byte{'a', iac, sb, naws, 0, 80, 0, 24, iac, se, 'b'},
			"ab",
		},
		{
			"the opening handshake a real client sends",
			[]byte{
				iac, do, echo, iac, will, naws,
				iac, sb, naws, 0, 80, 0, 24, iac, se,
				'q',
			},
			"q",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := drain(t, tc.input, 0)
			if string(got) != tc.want {
				t.Fatalf("got %q (% x), want %q", got, got, tc.want)
			}
		})
	}
}

// The critical case: a TCP read can end anywhere, including the middle of an
// IAC sequence. If the reader loses that state, the remaining command bytes
// arrive as keystrokes — the classic symptom being a BBS menu reacting to keys
// nobody pressed.
func TestTelnetHandlesSequencesSplitAcrossReads(t *testing.T) {
	input := []byte{
		'a',
		iac, will, echo,
		'b',
		iac, sb, naws, 0, 80, 0, 24, iac, se,
		'c',
		iac, iac, // escaped literal 0xFF
		'd',
	}
	want := "ab" + "c" + "\xff" + "d"

	// Feeding one byte at a time is the worst case, and any chunk size must
	// produce identical output.
	for _, chunk := range []int{1, 2, 3, 5, 7, 64} {
		got := drain(t, input, chunk)
		if string(got) != want {
			t.Errorf("chunk=%d: got %q (% x), want %q", chunk, got, got, want)
		}
	}
}

func TestTelnetNeverEmitsCommandBytes(t *testing.T) {
	// A hostile or noisy client sending many sequences must never have a
	// command byte reach the TUI as input.
	var input []byte
	for i := 0; i < 50; i++ {
		input = append(input, iac, will, byte(i))
		input = append(input, byte('a'+i%26))
		input = append(input, iac, sb, byte(i), 1, 2, 3, iac, se)
	}

	for _, chunk := range []int{1, 4, 9, 128} {
		got := drain(t, input, chunk)
		if bytes.IndexByte(got, iac) >= 0 {
			t.Errorf("chunk=%d: an IAC byte reached the application: % x", chunk, got)
		}
		if len(got) != 50 {
			t.Errorf("chunk=%d: got %d payload bytes, want 50: %q", chunk, len(got), got)
		}
	}
}

// ---------------------------------------------------------------------------
// Server behaviour
// ---------------------------------------------------------------------------

func telnetFixture(t *testing.T) (*bbs.Service, *store.Store, *theme.Set) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))

	st, err := store.OpenMemory(ctx, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := identity.GenerateNodeKey(rng.TestSecret(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, store.Node{
		ID: key.ID(), PublicKey: key.Public, DisplayName: "telnet-bbs", IsSelf: true,
	}); err != nil {
		t.Fatal(err)
	}

	svc := bbs.New(st, key, clk)
	if err := svc.SeedDefaultAreas(ctx); err != nil {
		t.Fatal(err)
	}
	themes, err := theme.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return svc, st, themes
}

// startTelnet runs a server on an ephemeral port and returns its address.
func startTelnet(t *testing.T, guestOnly bool) string {
	addr, _ := startTelnetLimited(t, guestOnly, 0)
	return addr
}

// startTelnetLimited is startTelnet with an explicit session cap, and returns
// the server so a test can observe how many sessions are live.
func startTelnetLimited(t *testing.T, guestOnly bool, maxSessions int) (string, *TelnetServer) {
	t.Helper()
	svc, st, themes := telnetFixture(t)

	// Bind first so the test knows the port without racing the goroutine.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	_, portStr, _ := net.SplitHostPort(addr)
	var port int
	if _, err := fmtSscan(portStr, &port); err != nil {
		t.Fatal(err)
	}

	srv := NewTelnetServer(svc, st, TelnetOptions{
		Bind: "127.0.0.1", Port: port, GuestOnly: guestOnly,
		MaxSessions: maxSessions,
		Themes:      themes, Theme: theme.DefaultName,
		Chat: tui.NewChatRoom(20), Presence: NewPresence(),
		Location: time.UTC,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.ListenAndServe(ctx) }()

	// Wait on the server's own readiness signal. A dial probe would occupy a
	// session slot until its handler unwound, which races any test that cares
	// about capacity.
	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatalf("telnet server did not start on %s", addr)
	}
	return addr, srv
}

// readUntil accumulates from conn until want appears or the deadline passes.
func readUntil(t *testing.T, conn net.Conn, want string, timeout time.Duration) string {
	t.Helper()
	var acc strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		n, err := conn.Read(buf)
		acc.Write(buf[:n])
		if strings.Contains(acc.String(), want) {
			break
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			break
		}
	}
	return acc.String()
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, io.ErrUnexpectedEOF
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return 1, nil
}

// [D12]: telnet is plaintext, and the warning must reach the user, not just
// the sysop's log.
func TestTelnetWarnsAboutPlaintextOnConnect(t *testing.T) {
	addr := startTelnet(t, true)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The banner may span several TCP segments, so accumulate rather than
	// assuming one read gets it all.
	got := readUntil(t, conn, "SSH", 5*time.Second)

	if !strings.Contains(got, "PLAINTEXT") {
		t.Fatalf("connect banner does not warn about plaintext:\n%q", got)
	}
	if !strings.Contains(got, "SSH") {
		t.Errorf("banner does not point at the private alternative:\n%q", got)
	}
}

// The server must negotiate character-at-a-time with no echo, or a full-screen
// TUI cannot work.
func TestTelnetNegotiatesOnConnect(t *testing.T) {
	addr := startTelnet(t, true)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	got := buf[:n]

	for _, want := range [][]byte{
		{iac, will, echo},
		{iac, will, 3}, // SUPPRESS_GO_AHEAD
		{iac, do, naws},
	} {
		if !bytes.Contains(got, want) {
			t.Errorf("handshake missing % x, got % x", want, got)
		}
	}
}

// A telnet session must land in a guest, read-only BBS — never a login prompt.
// Accepting a password over plaintext is exactly what [D12] warns about.
func TestTelnetSessionIsGuestReadOnly(t *testing.T) {
	addr := startTelnet(t, true)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Reply to the handshake as a real client would, then read the screen.
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte{
		iac, do, echo,
		iac, sb, naws, 0, 80, 0, 24, iac, se,
	}); err != nil {
		t.Fatal(err)
	}

	got := readUntil(t, conn, "guest", 6*time.Second)
	if !strings.Contains(got, "guest") {
		t.Fatalf("telnet session is not a guest session:\n%q", got)
	}
	// It must not offer a password prompt over a plaintext link.
	for _, forbidden := range []string{"Password:", "password:"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("telnet offered a password prompt over plaintext: %q", forbidden)
		}
	}
}

func TestTelnetShutsDownCleanly(t *testing.T) {
	svc, st, themes := telnetFixture(t)
	srv := NewTelnetServer(svc, st, TelnetOptions{
		Bind: "127.0.0.1", Port: 0, GuestOnly: true,
		Themes: themes, Theme: theme.DefaultName,
		Chat: tui.NewChatRoom(20), Presence: NewPresence(),
		Location: time.UTC,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("telnet server did not stop when its context was cancelled")
	}
}

// Telnet is a plaintext port on the public internet. Without a cap, anything
// that opens sockets and never speaks exhausts memory and file descriptors,
// and takes the SSH front end down with it.
func TestTelnetRefusesConnectionsOverTheLimit(t *testing.T) {
	addr, srv := startTelnetLimited(t, true, 2)

	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})

	// Fill the two available slots and keep them open.
	for i := 0; i < 2; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("connection %d refused while under the limit: %v", i, err)
		}
		held = append(held, c)
		// Wait until the server has registered it, so the third attempt races
		// nothing.
		if got := readUntil(t, c, "PLAINTEXT", 3*time.Second); !strings.Contains(got, "PLAINTEXT") {
			t.Fatalf("connection %d never got the banner (active=%d)", i, srv.ActiveSessions())
		}
	}

	// The third must be told the BBS is full and closed, not left hanging.
	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()

	got := readUntil(t, third, "full", 3*time.Second)
	if !strings.Contains(got, "full") {
		t.Fatalf("over-limit connection was not told the BBS is full:\n%q", got)
	}
	// And it should point at the alternative rather than just refusing.
	if !strings.Contains(got, "SSH") {
		t.Errorf("refusal does not mention SSH as an alternative:\n%q", got)
	}

	// Freeing a slot must let a new session in.
	held[0].Close()
	held = held[1:]
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && srv.ActiveSessions() >= 2 {
		time.Sleep(10 * time.Millisecond)
	}
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		out := readUntil(t, c, "PLAINTEXT", 1*time.Second)
		c.Close()
		if strings.Contains(out, "PLAINTEXT") && !strings.Contains(out, "full") {
			return // a slot was reclaimed
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("closing a session did not free its slot")
}
