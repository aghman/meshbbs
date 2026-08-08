package sshd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/theme"
	gossh "golang.org/x/crypto/ssh"
)

// syncBuffer is a destination whose contents can be read while writes are in
// flight, and which NOTICES two writers at once.
//
// The noticing is the point. A real destination — an ssh.Session, a net.Conn —
// makes no promise that two concurrent Writes will not interleave on the wire,
// so a test destination that quietly serialises them would report a clean screen
// no matter what the mux did. This one records the overlap instead, and yields
// inside the write to make sure a second writer has the chance to arrive.
type syncBuffer struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	writers  int
	overlaps int
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.writers++
	if b.writers > 1 {
		b.overlaps++
	}
	b.mu.Unlock()

	runtime.Gosched()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.writers--
	return b.buf.Write(p)
}

// Overlaps returns how many times a write began while another was in progress.
func (b *syncBuffer) Overlaps() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overlaps
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// newTestMux wires a mux to a pipe it can be fed through and a buffer whose
// writes can be inspected.
func newTestMux(t *testing.T) (*connMux, *io.PipeWriter, *syncBuffer) {
	t.Helper()
	pr, pw := io.Pipe()
	dst := &syncBuffer{}
	m := newConnMux(pr, dst)
	t.Cleanup(func() {
		m.Close()
		// Closing the source is what ends the pump: it cannot be interrupted
		// mid-read, which is the whole premise of the type.
		_ = pw.Close()
		_ = pr.Close()
	})
	return m, pw, dst
}

// With nothing borrowed the mux is a pass-through in both directions.
func TestMuxPassesThroughWhenNothingBorrowed(t *testing.T) {
	m, pw, dst := newTestMux(t)

	go func() { _, _ = pw.Write([]byte("hello")) }()

	got := make([]byte, 5)
	if _, err := io.ReadFull(m.TUI(), got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("read %q, want %q", got, "hello")
	}

	if _, err := m.TUI().Write([]byte("world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if dst.String() != "world" {
		t.Errorf("destination has %q, want %q", dst.String(), "world")
	}
}

// A handoff delivers bytes in order to whoever is active when they are read.
// This is the deterministic version of the property; the concurrent ones below
// cover a reader that is parked across the swap.
func TestMuxDeliversInOrderAcrossHandoff(t *testing.T) {
	m, pw, _ := newTestMux(t)

	go func() { _, _ = pw.Write([]byte("ABCDEFGHIJ")) }()

	read := func(r io.Reader, n int) string {
		t.Helper()
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			t.Fatalf("read %d: %v", n, err)
		}
		return string(b)
	}

	if got := read(m.TUI(), 3); got != "ABC" {
		t.Errorf("before borrow: got %q, want %q", got, "ABC")
	}

	var during string
	if err := m.Borrow(func(rw io.ReadWriter) error {
		during = read(rw, 4)
		return nil
	}); err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if during != "DEFG" {
		t.Errorf("during borrow: got %q, want %q", during, "DEFG")
	}

	if got := read(m.TUI(), 3); got != "HIJ" {
		t.Errorf("after borrow: got %q, want %q", got, "HIJ")
	}
}

// The bug this whole type exists to prevent: a reader parked across a handoff
// must not lose a byte to it.
//
// The TUI reader here is the realistic adversary — Bubble Tea's read loop, which
// goes straight back into Read and stays there. With tea.Exec over SSH that
// goroutine keeps reading sess through the handoff and eats a keystroke; here it
// is parked by the mux instead, and every byte written to the connection must
// still arrive, in order, exactly once.
func TestMuxParkedReaderLosesNothingAcrossManyHandoffs(t *testing.T) {
	m, pw, _ := newTestMux(t)

	const total = 8192
	want := make([]byte, total)
	for i := range want {
		want[i] = byte(i % 251) // a prime cycle, so a lost run cannot align
	}

	got := make([]byte, 0, total)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for len(got) < total {
			n, err := m.TUI().Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for off := 0; off < total; off += 97 {
			end := min(off+97, total)
			if _, err := pw.Write(want[off:end]); err != nil {
				return
			}
		}
	}()

	// Borrow repeatedly while the stream is flowing. Each borrow reads nothing,
	// so anything queued during it has to survive to the TUI — which is the
	// type-ahead case as well as the parked-reader one.
	for range 200 {
		if err := m.Borrow(func(io.ReadWriter) error { return nil }); err != nil {
			t.Fatalf("borrow: %v", err)
		}
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("reader stalled after %d of %d bytes", len(got), total)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("stream corrupted: read %d bytes of %d", len(got), total)
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("first difference at byte %d: got %d, want %d", i, got[i], want[i])
				break
			}
		}
	}
}

// A borrower has the input to itself, with a TUI reader running against it.
//
// The adversary is the realistic one — a goroutine that reads in a loop and
// never stops, which is what Bubble Tea's read loop does. It is also why this
// sends MANY chunks rather than one: whether a single chunk goes to the right
// reader is decided by which goroutine a broadcast happens to wake first, so one
// chunk would let a mux that ignores ownership pass by coin flip. Every chunk
// has to land on the borrower.
func TestMuxBorrowerHasInputExclusively(t *testing.T) {
	m, pw, _ := newTestMux(t)

	const (
		chunks    = 200
		chunkSize = 8
	)
	payload := bytes.Repeat([]byte("door----"), chunks)

	var mu sync.Mutex
	var stolen []byte
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := m.TUI().Read(buf)
			mu.Lock()
			stolen = append(stolen, buf[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	// Let the TUI reader reach its blocking read before the handoff, so it is
	// parked inside Read rather than merely about to call it.
	time.Sleep(20 * time.Millisecond)

	var during []byte
	err := m.Borrow(func(rw io.ReadWriter) error {
		go func() {
			for i := range chunks {
				if _, err := pw.Write(payload[i*chunkSize : (i+1)*chunkSize]); err != nil {
					return
				}
			}
		}()

		// Bounded, because the failure this test looks for is the TUI taking
		// bytes — which leaves the borrower's read short, and an unbounded
		// ReadFull would hang rather than fail.
		got := make(chan []byte, 1)
		go func() {
			b := make([]byte, len(payload))
			if _, err := io.ReadFull(rw, b); err != nil {
				return
			}
			got <- b
		}()
		select {
		case during = <-got:
		case <-time.After(5 * time.Second):
			mu.Lock()
			n := len(stolen)
			mu.Unlock()
			return fmt.Errorf("the borrower is short; the TUI took %d bytes", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if !bytes.Equal(during, payload) {
		t.Errorf("borrower read %d bytes, want %d", len(during), len(payload))
	}

	mu.Lock()
	defer mu.Unlock()
	if len(stolen) != 0 {
		t.Errorf("the TUI read %d bytes while the connection was lent out", len(stolen))
	}
}

// While a borrower holds the connection the TUI's frames are discarded rather
// than spliced into the door's screen — and discarding is reported as success,
// because a write error is how Bubble Tea learns the terminal is gone.
func TestMuxSuppressesSuspendedWrites(t *testing.T) {
	m, _, dst := newTestMux(t)

	if _, err := m.TUI().Write([]byte("<before>")); err != nil {
		t.Fatalf("write before borrow: %v", err)
	}

	err := m.Borrow(func(rw io.ReadWriter) error {
		n, err := m.TUI().Write([]byte("<suspended frame>"))
		if err != nil {
			t.Errorf("suspended write reported an error: %v", err)
		}
		if n != len("<suspended frame>") {
			t.Errorf("suspended write reported %d bytes, want %d", n, len("<suspended frame>"))
		}
		_, err = rw.Write([]byte("<door>"))
		return err
	})
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}

	if _, err := m.TUI().Write([]byte("<after>")); err != nil {
		t.Fatalf("write after borrow: %v", err)
	}

	if want := "<before><door><after>"; dst.String() != want {
		t.Errorf("destination has %q, want %q", dst.String(), want)
	}
}

// A handoff may not begin or end in the middle of a frame, and nothing the TUI
// renders may reach the connection while a door holds it.
//
// Two distinct failures, both of which look like a corrupted door screen. A swap
// landing mid-write splices one owner's escape sequence into the other's; a swap
// landing between "am I active?" and the write itself lets a whole stale frame
// through after the door has taken over. Only the second one needs the write
// lock to cover the check as well as the write, which is why the ownership
// assertion is here rather than left to the suppression test.
func TestMuxWritesAreNotSplicedByAHandoff(t *testing.T) {
	m, _, dst := newTestMux(t)

	const (
		tuiFrame  = "\x1b[2J\x1b[HTTTTTTTTTTTTTTTT"
		doorFrame = "\x1b[2J\x1b[HDDDDDDDDDDDDDDDD"
	)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = m.TUI().Write([]byte(tuiFrame))
		}
	}()

	for range 300 {
		err := m.Borrow(func(rw io.ReadWriter) error {
			// The first door frame establishes the boundary. A TUI write that
			// was in flight when the handoff happened is entitled to finish, and
			// it lands ahead of this one because both serialise on the write
			// lock — so once this has landed, nothing the TUI renders can reach
			// the connection until the borrow ends. Snapshotting before this
			// write instead would be racing that legitimate straggler.
			if _, err := rw.Write([]byte(doorFrame)); err != nil {
				return err
			}
			before := dst.Len()
			if _, err := rw.Write([]byte(doorFrame)); err != nil {
				return err
			}
			landed := dst.String()[before:]
			if strings.ContainsRune(landed, 'T') {
				return fmt.Errorf("a TUI frame reached the connection during the "+
					"borrow: %q", landed)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()

	// No two writes were ever in progress at once, so nothing the connection
	// received could have been interleaved on the wire.
	if n := dst.Overlaps(); n != 0 {
		t.Errorf("%d writes began while another was still in progress", n)
	}

	// And every byte that reached the connection belongs to a whole frame:
	// suppressed writes vanish completely, and surviving ones are intact.
	out := dst.String()
	if n := len(out) % len(tuiFrame); n != 0 {
		t.Fatalf("destination holds %d bytes, not a whole number of %d-byte frames",
			len(out), len(tuiFrame))
	}
	for off := 0; off < len(out); off += len(tuiFrame) {
		if f := out[off : off+len(tuiFrame)]; f != tuiFrame && f != doorFrame {
			t.Fatalf("frame at offset %d is not intact: %q", off, f)
		}
	}
}

// The suspended renderer believes the screen still shows its last frame, so it
// has to be told to repaint — with the TUI already active, or the repaint would
// be suppressed too.
func TestMuxResumeHookRunsWithTUIActive(t *testing.T) {
	m, _, dst := newTestMux(t)

	var calls int
	m.onResume = func() {
		calls++
		if _, err := m.TUI().Write([]byte("<repaint>")); err != nil {
			t.Errorf("repaint write: %v", err)
		}
	}

	if err := m.Borrow(func(io.ReadWriter) error { return nil }); err != nil {
		t.Fatalf("borrow: %v", err)
	}
	if calls != 1 {
		t.Errorf("onResume ran %d times, want 1", calls)
	}
	if dst.String() != "<repaint>" {
		t.Errorf("destination has %q; the repaint was suppressed", dst.String())
	}
}

// The hook runs even when the borrower failed, because the screen is just as
// overwritten either way.
func TestMuxResumesAfterAFailedBorrow(t *testing.T) {
	m, _, _ := newTestMux(t)

	var resumed bool
	m.onResume = func() { resumed = true }

	boom := errors.New("the door crashed")
	if err := m.Borrow(func(io.ReadWriter) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("borrow returned %v, want %v", err, boom)
	}
	if !resumed {
		t.Error("onResume did not run after a failed borrow")
	}
	if _, err := m.TUI().Write([]byte("x")); err != nil {
		t.Errorf("the TUI is still suspended: %v", err)
	}
}

// Two doors at once is a bug or a double keypress, and neither wants serving
// eventually.
func TestMuxRefusesASecondBorrow(t *testing.T) {
	m, _, _ := newTestMux(t)

	err := m.Borrow(func(io.ReadWriter) error {
		if err := m.Borrow(func(io.ReadWriter) error {
			t.Error("the second borrow ran")
			return nil
		}); !errors.Is(err, errMuxBusy) {
			t.Errorf("second borrow returned %v, want %v", err, errMuxBusy)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first borrow: %v", err)
	}

	// And the refusal left the mux usable.
	if err := m.Borrow(func(io.ReadWriter) error { return nil }); err != nil {
		t.Errorf("borrow after a refusal: %v", err)
	}
}

// A dead connection has to reach a SUSPENDED reader too. If it did not, a TUI
// parked behind a door whose session has gone would wait for a turn that is
// never coming, and the session goroutine would never unwind.
func TestMuxConnectionDeathReachesASuspendedReader(t *testing.T) {
	m, pw, _ := newTestMux(t)

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := m.TUI().Read(buf)
		readErr <- err
	}()
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Borrow(func(io.ReadWriter) error {
			// The client hangs up while the door is running.
			_ = pw.CloseWithError(io.ErrUnexpectedEOF)
			select {
			case err := <-readErr:
				if err == nil {
					t.Error("the suspended read returned no error")
				}
			case <-time.After(5 * time.Second):
				t.Error("the suspended read never returned")
			}
			return nil
		})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the borrow never finished")
	}
}

// Close releases a reader that is waiting for its turn.
func TestMuxCloseReleasesReaders(t *testing.T) {
	m, _, _ := newTestMux(t)

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := m.TUI().Read(buf)
		readErr <- err
	}()
	time.Sleep(20 * time.Millisecond)

	m.Close()
	select {
	case err := <-readErr:
		if !errors.Is(err, io.EOF) {
			t.Errorf("read returned %v, want %v", err, io.EOF)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the reader")
	}

	if err := m.Borrow(func(io.ReadWriter) error {
		t.Error("borrowed a closed connection")
		return nil
	}); err == nil {
		t.Error("borrowing a closed mux succeeded")
	}
}

// A session's worth of handoffs must not leave goroutines behind. The pump ends
// with the connection, so the connection is what the test ends.
func TestMuxLeavesNoGoroutinesBehind(t *testing.T) {
	settle := func() int {
		var n int
		for range 50 {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}
	before := settle()

	for range 50 {
		pr, pw := io.Pipe()
		m := newConnMux(pr, io.Discard)
		for range 5 {
			if err := m.Borrow(func(io.ReadWriter) error { return nil }); err != nil {
				t.Fatalf("borrow: %v", err)
			}
		}
		m.Close()
		_ = pw.Close()
		_ = pr.Close()
	}

	if after := settle(); after > before+2 {
		t.Errorf("goroutines went from %d to %d", before, after)
	}
}

// The mux is only worth anything if a real session still works through it.
//
// Every other test here drives it directly. This one goes the whole way — a real
// SSH client, a real PTY, a keystroke in and a rendered screen out — because the
// mux sits between Bubble Tea and the connection in production, and the failure
// it would cause there is a session that connects and then ignores the keyboard.
func TestMuxCarriesARealSSHSession(t *testing.T) {
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

	_, priv, err := ed25519.GenerateKey(rng.TestSecret(11).Reader())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	client, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{
		User:            GuestUser,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()

	screen := &syncBuffer{}
	sess.Stdout = screen
	sess.Stderr = io.Discard

	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	// The menu proves output reaches the client through the mux.
	awaitScreen(t, screen, "MeshBBS", 10*time.Second)

	// And a keystroke that changes screens proves input reaches the model
	// through it. [W] is the cheapest such key: no database write, no prompt.
	if _, err := keys.Write([]byte("w")); err != nil {
		t.Fatal(err)
	}
	awaitScreen(t, screen, "Who's Online", 10*time.Second)
}

// awaitScreen polls the captured session output for want, ignoring the escape
// sequences the renderer wraps it in.
func awaitScreen(t *testing.T, out *syncBuffer, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		plain := ansiEscape.ReplaceAllString(out.String(), "")
		if strings.Contains(plain, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the session never showed %q; it showed:\n%s", want, plain)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]|\x1b[()][A-Za-z0-9]`)

// A door bridge's keystroke copier is parked in a read when the door exits, and
// nothing can interrupt it — the bytes it waits for have not arrived. Ending
// the borrow is what releases it, and it must do so promptly: otherwise every
// door launch leaks a goroutine that still believes it is owed the user's input.
func TestMuxReleasesABorrowersReaderWhenTheBorrowEnds(t *testing.T) {
	m, _, _ := newTestMux(t)

	parked := make(chan error, 1)
	release := make(chan struct{})

	err := m.Borrow(func(rw io.ReadWriter) error {
		go func() {
			buf := make([]byte, 16)
			_, err := rw.Read(buf)
			parked <- err
		}()
		// Let the copier reach its blocking read before the borrow ends.
		time.Sleep(20 * time.Millisecond)
		select {
		case err := <-parked:
			t.Errorf("the read returned %v while the borrow was still live", err)
		default:
		}
		close(release)
		return nil
	})
	if err != nil {
		t.Fatalf("borrow: %v", err)
	}
	<-release

	select {
	case err := <-parked:
		if !errors.Is(err, errBorrowEnded) {
			t.Errorf("the parked read returned %v, want %v", err, errBorrowEnded)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the borrower's reader is still parked after the borrow ended")
	}

	// And the TUI has its input back.
	if err := m.Borrow(func(io.ReadWriter) error { return nil }); err != nil {
		t.Errorf("borrow after release: %v", err)
	}
}
