package door

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
)

// The door under test is this test binary, re-executed.
//
// A door is "any executable, any language" (§9.1), so the runner has no opinion
// about what it launches — which means a test door can be anything that starts.
// Using the test binary keeps the doors in the same file as the assertions
// about them, needs no build step, and behaves identically on all five targets;
// a shell script would not run on Windows, and a fixture binary would have to be
// compiled per platform by the test.
const (
	helperEnv     = "MESHBBS_DOOR_TEST_MODE"
	helperArgEnv  = "MESHBBS_DOOR_TEST_ARG"
	inheritedEnv  = "MESHBBS_DOOR_TEST_MUST_NOT_LEAK"
	inheritedWord = "a-deployment-secret"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		helperMain(mode, os.Getenv(helperArgEnv))
		return
	}
	os.Exit(m.Run())
}

// helperMain is the door. It never returns.
func helperMain(mode, arg string) {
	switch mode {
	case "echo":
		// A pty is in canonical mode, so input arrives a line at a time —
		// which is what an interactive door sees too.
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if sc.Text() == "quit" {
				fmt.Print("BYE\r\n")
				os.Exit(0)
			}
			fmt.Printf("[%s]\r\n", strings.ToUpper(sc.Text()))
		}
		os.Exit(0)

	case "exit":
		code, _ := strconv.Atoi(arg)
		fmt.Print("going\r\n")
		os.Exit(code)

	case "spin":
		fmt.Print("spinning\r\n")
		for {
			time.Sleep(10 * time.Millisecond)
		}

	case "stubborn":
		// A door that will not take the hint. Ignoring SIGTERM is what a badly
		// written save-on-exit handler looks like from outside, and it is the
		// case the escalation to SIGKILL exists for.
		signal.Ignore(syscall.SIGTERM)
		fmt.Print("stubborn\r\n")
		for {
			time.Sleep(10 * time.Millisecond)
		}

	case "orphan":
		// Fork something that outlives us and keeps the terminal open, then
		// exit cleanly. This is the door that looks perfectly well behaved and
		// leaves a process behind (§9.4).
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			helperEnv+"=hold",
			helperArgEnv+"="+arg,
		)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		// Do not exit until the child is up and has disowned SIGHUP.
		//
		// Exiting immediately makes the kernel do the test's work: the door is
		// the session leader, so its death HUPs the foreground process group,
		// and a child still inside Go's runtime init dies of that before it
		// runs a line. What is under test is the runner cleaning up after a
		// door, not the tty driver cleaning up after a session leader.
		for range 500 {
			if _, err := os.Stat(arg); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		fmt.Print("forked\r\n")
		os.Exit(0)

	case "hold":
		// A worker a door left running: it survives its parent, stays in the
		// process group, and holds the terminal. Ignoring SIGHUP is what makes
		// it outlast the door rather than being swept up with it, and it is
		// ordinary behaviour for anything meant to keep working.
		signal.Ignore(syscall.SIGHUP)
		if arg != "" {
			_ = os.WriteFile(arg, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(10 * time.Minute)
		os.Exit(0)

	case "env":
		for _, kv := range os.Environ() {
			fmt.Printf("%s\r\n", kv)
		}
		fmt.Print("ENVDONE\r\n")
		os.Exit(0)

	default:
		// Modes that need platform-specific machinery — reading the window
		// size needs an ioctl, and there is no portable spelling of one.
		if platformHelper(mode, arg) {
			return
		}
		os.Exit(97)
	}
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// screen collects what the door drew.
type screen struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *screen) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// harness is one door run's worth of plumbing.
type harness struct {
	mgr    *Manager
	spec   Spec
	sess   Session
	keysR  io.Reader
	keys   *io.PipeWriter
	screen *screen
}

func newHarness(t *testing.T, clk clock.Clock, mode, arg string) *harness {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	keysR, keysW := io.Pipe()
	t.Cleanup(func() { _ = keysW.Close() })

	scr := &screen{}
	return &harness{
		mgr: New(clk, slog.New(slog.NewTextHandler(io.Discard, nil))),
		spec: Spec{
			Name: "testdoor",
			Path: self,
			Dir:  t.TempDir(),
			Env: []string{
				helperEnv + "=" + mode,
				helperArgEnv + "=" + arg,
			},
			WallClock: 30 * time.Second,
		},
		sess: Session{
			Term:   "xterm-testing",
			Width:  80,
			Height: 24,
			Node:   1,
		},
		keysR:  keysR,
		keys:   keysW,
		screen: scr,
	}
}

// session completes the Session with the screen, which has to happen after any
// test-specific edits to the harness.
func (h *harness) session() Session {
	s := h.sess
	s.RW = readWriter{r: h.keysR, w: h.screen}
	return s
}

type readWriter struct {
	r io.Reader
	w io.Writer
}

func (rw readWriter) Read(p []byte) (int, error)  { return rw.r.Read(p) }
func (rw readWriter) Write(p []byte) (int, error) { return rw.w.Write(p) }

// await polls the screen for want.
func (h *harness) await(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(h.screen.String(), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the door never wrote %q; it wrote:\n%s", want, h.screen.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// runInBackground starts a door that is expected to still be running when the
// test makes its assertions, and stops it when the test ends. A door left
// running would hold its concurrency slot and its process into the next test.
func (h *harness) runInBackground(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := h.mgr.Run(ctx, h.spec, h.session()); err != nil {
			t.Errorf("background door: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("a background door never stopped")
		}
	})
}

// sameDoorAs makes this harness a different session of the SAME door, which is
// what the cross-session limits are keyed on. Copying the whole spec would also
// copy the other door's environment, and with it which helper mode it runs.
func (h *harness) sameDoorAs(other *harness) {
	h.spec.Name = other.spec.Name
	h.spec.MaxConcurrent = other.spec.MaxConcurrent
	h.spec.NodeLock = other.spec.NodeLock
}

func realClock() clock.Clock { return clock.NewReal() }

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// The whole point of the package: a door gets a real terminal, and bytes move
// both ways.
func TestRunBridgesTheDoorToTheSession(t *testing.T) {
	h := newHarness(t, realClock(), "echo", "")

	done := make(chan Result, 1)
	go func() {
		res, err := h.mgr.Run(context.Background(), h.spec, h.session())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()

	if _, err := h.keys.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	h.await(t, "[HELLO]", 15*time.Second)

	if _, err := h.keys.Write([]byte("quit\n")); err != nil {
		t.Fatal(err)
	}
	h.await(t, "BYE", 15*time.Second)

	select {
	case res := <-done:
		if res.Stop != StopExited {
			t.Errorf("stopped because %v, want %v", res.Stop, StopExited)
		}
		if res.ExitCode != 0 {
			t.Errorf("exit code %d, want 0", res.ExitCode)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the door exited")
	}
}

// A door's exit status is the door's, and the caller gets it verbatim.
func TestRunReportsTheExitCode(t *testing.T) {
	h := newHarness(t, realClock(), "exit", "7")

	res, err := h.mgr.Run(context.Background(), h.spec, h.session())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code %d, want 7", res.ExitCode)
	}
	if res.Stop != StopExited {
		t.Errorf("stopped because %v, want %v", res.Stop, StopExited)
	}
}

// A door that will not stop gets stopped. Without this a hung third-party
// binary holds the user's session open with no way out.
func TestRunEndsADoorThatOverrunsItsLimit(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	h := newHarness(t, clk, "spin", "")
	h.spec.WallClock = time.Minute

	done := make(chan Result, 1)
	go func() {
		res, err := h.mgr.Run(context.Background(), h.spec, h.session())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()

	stop := advanceUntil(clk)
	defer close(stop)

	select {
	case res := <-done:
		if res.Stop != StopTimeLimit {
			t.Errorf("stopped because %v, want %v", res.Stop, StopTimeLimit)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the door outlived its wall-clock limit")
	}
}

// Asking politely is not enough on its own. A door that ignores SIGTERM has to
// be killed, or the user's session never comes back.
func TestRunKillsADoorThatIgnoresTheRequestToStop(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	h := newHarness(t, clk, "stubborn", "")
	h.spec.WallClock = time.Minute

	done := make(chan Result, 1)
	go func() {
		res, err := h.mgr.Run(context.Background(), h.spec, h.session())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()
	h.await(t, "stubborn", 15*time.Second)

	stop := advanceUntil(clk)
	defer close(stop)

	select {
	case res := <-done:
		if res.Stop != StopTimeLimit {
			t.Errorf("stopped because %v, want %v", res.Stop, StopTimeLimit)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a door that ignored SIGTERM was never killed")
	}
}

// The user hanging up ends the door too.
func TestRunEndsWhenTheSessionDoes(t *testing.T) {
	h := newHarness(t, realClock(), "spin", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		res, err := h.mgr.Run(ctx, h.spec, h.session())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()
	h.await(t, "spinning", 15*time.Second)
	cancel()

	select {
	case res := <-done:
		if res.Stop != StopSessionEnded {
			t.Errorf("stopped because %v, want %v", res.Stop, StopSessionEnded)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the door survived the session")
	}
}

// A door that forks and exits looks perfectly well behaved, and has left a
// process holding the user's terminal. Both halves matter: Run has to return,
// and the process has to be gone.
//
// How Run gets there differs by platform, which is why the assertion is about
// the orphan and not about the route. Linux leaves the pty open as long as the
// orphan holds it, so the drain elapses and the group is killed after it. The
// BSDs revoke the terminal when the session leader exits, which invalidates the
// orphan's descriptors immediately — so the drain ends at once and the orphan
// is still sitting there, alive, with a dead terminal. Only the kill reaches it
// either way.
func TestRunKillsWhatTheDoorLeftBehind(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "orphan.pid")
	h := newHarness(t, realClock(), "orphan", pidFile)

	done := make(chan Result, 1)
	go func() {
		res, err := h.mgr.Run(context.Background(), h.spec, h.session())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Run never returned; the orphan held the terminal open forever")
	}

	pid := readPID(t, pidFile, 10*time.Second)
	waitGone(t, pid, 20*time.Second)
}

// Concurrency caps are the sysop's, and they hold across sessions.
func TestRunRefusesBeyondMaxConcurrent(t *testing.T) {
	first := newHarness(t, realClock(), "spin", "")
	first.spec.MaxConcurrent = 1
	first.runInBackground(t)
	first.await(t, "spinning", 15*time.Second)

	second := newHarness(t, realClock(), "exit", "0")
	second.sameDoorAs(first)
	second.sess.Node = 2
	// Same Manager: the cap is a statement about the instance, not the session.
	_, err := first.mgr.Run(context.Background(), second.spec, second.session())
	if !errors.Is(err, ErrAtCapacity) {
		t.Errorf("second run returned %v, want %v", err, ErrAtCapacity)
	}
}

// A node-locked door allows one instance per node, and refuses a second on the
// same node — which is what a door keeping per-node state on disk assumes.
func TestRunLocksADoorToOneInstancePerNode(t *testing.T) {
	first := newHarness(t, realClock(), "spin", "")
	first.spec.NodeLock = true
	first.runInBackground(t)
	first.await(t, "spinning", 15*time.Second)

	sameNode := newHarness(t, realClock(), "exit", "0")
	sameNode.sameDoorAs(first)
	sameNode.sess.Node = first.sess.Node
	if _, err := first.mgr.Run(context.Background(), sameNode.spec, sameNode.session()); !errors.Is(err, ErrNodeBusy) {
		t.Errorf("same node returned %v, want %v", err, ErrNodeBusy)
	}

	otherNode := newHarness(t, realClock(), "exit", "0")
	otherNode.sameDoorAs(first)
	otherNode.sess.Node = first.sess.Node + 1
	if _, err := first.mgr.Run(context.Background(), otherNode.spec, otherNode.session()); err != nil {
		t.Errorf("a different node was refused: %v", err)
	}
}

// A slot taken is a slot given back, however the run ended.
func TestRunReleasesItsSlot(t *testing.T) {
	h := newHarness(t, realClock(), "exit", "3")
	h.spec.MaxConcurrent = 1
	h.spec.NodeLock = true

	for i := range 3 {
		if _, err := h.mgr.Run(context.Background(), h.spec, h.session()); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if n := h.mgr.Running(h.spec.Name); n != 0 {
		t.Errorf("%d instances still counted as running", n)
	}
}

// A door is a third-party binary on the sysop's machine, and the server's
// environment is where deployment secrets live. os/exec inherits by default,
// so this is a property that has to be asserted rather than assumed.
func TestDoorDoesNotInheritTheServerEnvironment(t *testing.T) {
	t.Setenv(inheritedEnv, inheritedWord)

	h := newHarness(t, realClock(), "env", "")
	h.spec.Env = append(h.spec.Env, "DOOR_OWN_SETTING=yes")

	if _, err := h.mgr.Run(context.Background(), h.spec, h.session()); err != nil {
		t.Fatalf("run: %v", err)
	}
	h.await(t, "ENVDONE", 15*time.Second)
	env := h.screen.String()

	if strings.Contains(env, inheritedWord) {
		t.Errorf("the door inherited %s from the server:\n%s", inheritedEnv, env)
	}
	for _, want := range []string{"DOOR_OWN_SETTING=yes", "TERM=xterm-testing"} {
		if !strings.Contains(env, want) {
			t.Errorf("the door's environment is missing %q:\n%s", want, env)
		}
	}
	// PATH comes from the platform base, because a door that can find no
	// program at all is not a useful door.
	if !strings.Contains(env, "PATH=") {
		t.Errorf("the door's environment has no PATH:\n%s", env)
	}
}

func TestSpecValidation(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ok := Spec{Name: "d", Path: self, Dir: dir, WallClock: time.Minute}

	if err := ok.validate(); err != nil {
		t.Fatalf("a valid spec was rejected: %v", err)
	}

	cases := []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"no name", func(s *Spec) { s.Name = "  " }, "name"},
		{"relative path", func(s *Spec) { s.Path = "doorgame" }, "absolute"},
		{"path is a directory", func(s *Spec) { s.Path = dir }, "directory"},
		{"path does not exist", func(s *Spec) { s.Path = filepath.Join(dir, "nope") }, "nope"},
		{"relative working directory", func(s *Spec) { s.Dir = "doors/lord" }, "absolute"},
		{"working directory missing", func(s *Spec) { s.Dir = filepath.Join(dir, "gone") }, "working directory"},
		{"working directory is a file", func(s *Spec) { s.Dir = self }, "not a directory"},
		{"malformed environment", func(s *Spec) { s.Env = []string{"JUSTAKEY"} }, "KEY=VALUE"},
		{"no wall-clock limit", func(s *Spec) { s.WallClock = 0 }, "wall-clock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := ok
			tc.edit(&spec)
			err := spec.validate()
			if err == nil {
				t.Fatalf("accepted: %+v", spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A door that has run and stopped leaves nothing running.
func TestRunLeavesNoGoroutines(t *testing.T) {
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

	for range 5 {
		h := newHarness(t, realClock(), "exit", "0")
		if _, err := h.mgr.Run(context.Background(), h.spec, h.session()); err != nil {
			t.Fatalf("run: %v", err)
		}
		// The keystroke copier is parked reading the session, which in
		// production is released by the caller taking the connection back.
		_ = h.keys.Close()
	}

	if after := settle(); after > before+2 {
		t.Errorf("goroutines went from %d to %d", before, after)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// advanceUntil keeps a virtual clock moving until the returned channel closes.
//
// Run registers its timers from inside the select they sit in, so a single
// Advance would race them: too early and no waiter exists yet, too late is
// indistinguishable from never. Advancing repeatedly is the virtual-clock
// equivalent of waiting for a real deadline.
func advanceUntil(clk *clock.Virtual) chan struct{} {
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			clk.Advance(10 * time.Second)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return stop
}

// waitGone blocks until a process is no longer running.
func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if processGone(pid) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running; the door's leftovers survived it", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func readPID(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the orphan never recorded its pid at %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
