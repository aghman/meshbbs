// Package door runs a door game on a pseudo-terminal and bridges it to a
// session (design §9.1, §9.3, §9.4).
//
// A door is any executable, in any language, that talks over stdin and stdout
// (§9.1). Everything specific to a door being a DOOR rather than an arbitrary
// subprocess — the session descriptor, the API socket, the capability model of
// §9.1.1 — sits above this package. What lives here is the part that has to be
// right on five platforms: give the program a real terminal, move bytes both
// ways, keep it inside its limits, and make sure that when it is over, nothing
// it started is still running.
//
// # What this package does not claim to be
//
// It is not a sandbox. §9.4's minimum bar is a dedicated low-privilege account,
// which is documented for the sysop rather than enforced by us, and nothing here
// changes that: a door runs with the server's privileges and can read whatever
// the server can read. Setting a working directory is not confinement, an
// environment allowlist is not isolation, and neither is described as such
// below. What this package does enforce is that a door cannot run forever, and
// cannot leave anything behind when it stops.
package door

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
)

// Spec is everything about a door that does not change between runs. It comes
// from the doors table (§11.5), which is why nothing here is a config key.
type Spec struct {
	// Name identifies the door for logging and for concurrency accounting.
	Name string
	// Path is the executable, and must be ABSOLUTE. A bare name would be
	// resolved against PATH, which turns "which binary did the sysop mean" into
	// a question answered by the server's environment at launch time.
	Path string
	// Args are passed as argv, never through a shell (§9.4). There is no code
	// path here that builds a command line out of a string, so there is nothing
	// for a user-supplied value to escape from.
	Args []string
	// Dir is the door's working directory: absolute, and it must exist.
	Dir string
	// Env is the door's environment, on top of the small platform base in
	// baseEnv. It REPLACES the server's environment rather than adding to it —
	// see environ.
	Env []string
	// Dropfile is the format to write before launching, or DropfileNone (§9.2).
	Dropfile string
	// Grant is what the sysop allowed this door through the API (§9.1.1). It is
	// ignored when the Manager has no Host, which is the case for a BBS that
	// runs doors without offering them an API at all — §9.1 calls the socket
	// optional and means it.
	Grant Grant

	// MaxConcurrent caps simultaneous instances of this door. Zero means no cap.
	MaxConcurrent int
	// NodeLock allows at most one instance of this door per BBS node number.
	// Many doors keep per-node state on disk and assume they are alone in it
	// (§9.4).
	NodeLock bool
	// WallClock bounds one run, and is REQUIRED. A door is a third-party binary
	// holding a user's session open; without a bound, one that hangs takes the
	// session with it and the user cannot even quit. A generous limit is a
	// configuration decision, but having one is not.
	WallClock time.Duration
}

// Session is the terminal a door is being run for.
type Session struct {
	// RW is the user's terminal. In production it is a borrowed connection
	// port, so reads block and writes are discarded once the borrow ends.
	RW io.ReadWriter
	// Term is the terminal type, passed through as TERM.
	Term string
	// Width and Height are the initial window size.
	Width, Height int
	// Node is the BBS node number, which NodeLock is keyed on.
	Node int
	// Resize carries later window sizes, and may be nil.
	Resize <-chan Size

	// Nick is the account playing, empty for a guest. A door API call that
	// needs an account refuses when this is empty rather than inventing one.
	Nick string
	// RealName is shown to doors that ask; it is whatever the user chose to
	// give, and may be empty (§6.7's collect_real_name).
	RealName string
	// ANSI and Encoding are the level-1 terminal capability hints.
	ANSI     bool
	Encoding string
	// Sysop selects the security level a dropfile reports. It is NOT a gate:
	// what a caller may run was decided before the door started.
	Sysop bool
	// Location is where the caller says they are calling from, for the
	// dropfile formats that have a field for it. Free text, and sanitised.
	Location string
	// BBSName and SysopName identify the board to a door. They belong to the
	// instance rather than the session, and are carried here because the front
	// end launching a door is the thing that knows them.
	BBSName   string
	SysopName string
	// TimeRemaining reports how long this session has left. Nil means no
	// limit, which a door is told explicitly rather than by a zero.
	TimeRemaining func() time.Duration
}

// Size is a terminal window size.
type Size struct{ Width, Height int }

// Stop says why a door stopped.
type Stop int

const (
	// StopExited means the door returned on its own.
	StopExited Stop = iota
	// StopTimeLimit means WallClock ran out.
	StopTimeLimit
	// StopSessionEnded means the context was cancelled — the user hung up, or
	// the server is shutting down.
	StopSessionEnded
)

func (s Stop) String() string {
	switch s {
	case StopTimeLimit:
		return "time limit"
	case StopSessionEnded:
		return "session ended"
	default:
		return "exited"
	}
}

// Result describes one completed run.
type Result struct {
	// ExitCode is the door's status, or -1 when it was signalled.
	ExitCode int
	Runtime  time.Duration
	Stop     Stop
}

// Errors a caller is expected to handle rather than log.
var (
	// ErrAtCapacity means MaxConcurrent instances are already running.
	ErrAtCapacity = errors.New("that door is already running as many copies as it allows")
	// ErrNodeBusy means this node already has an instance of a node-locked door.
	ErrNodeBusy = errors.New("that door is already running on this node")
)

const (
	// graceperiod is how long a door gets to exit after being asked politely,
	// before it is killed. Long enough for a save-and-quit handler, short
	// enough that a user waiting to get back to the menu does not give up.
	//
	// On Windows there is nothing polite to ask with, so this elapses without
	// effect — see terminal_windows.go.
	gracePeriod = 5 * time.Second
	// drainLimit bounds how long we go on reading a door's output after it has
	// stopped. Anything still holding the terminal open at this point is a
	// grandchild the door left behind, and it is about to be killed anyway.
	drainLimit = 2 * time.Second
)

// Manager runs doors and enforces the limits that span runs.
//
// MaxConcurrent and NodeLock cannot live in Spec's own enforcement because they
// are statements about every session at once, and a session knows only itself.
type Manager struct {
	clock clock.Clock
	log   *slog.Logger

	host Host

	mu      sync.Mutex
	running map[string]int
	nodes   map[nodeKey]bool
	// announces holds recent announce times per door, for the level-3 rate
	// limit. Per door rather than per invocation: a limit that reset on every
	// relaunch would be no limit at all for a door that announces on startup.
	announces map[string][]time.Time
}

type nodeKey struct {
	door string
	node int
}

// New builds a Manager.
func New(clk clock.Clock, log *slog.Logger) *Manager {
	if clk == nil {
		clk = clock.NewReal()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		clock:     clk,
		log:       log,
		running:   map[string]int{},
		nodes:     map[nodeKey]bool{},
		announces: map[string][]time.Time{},
	}
}

// SetHost gives the Manager its way into the BBS, which is what turns the door
// API on. Call it once at startup, before any door runs.
//
// A Manager with no Host still runs doors; they simply get no socket and no
// descriptor. That is not a degraded mode — §9.1 calls the API optional, and a
// board with one legacy binary installed should not have to configure an API
// that binary will never call.
func (m *Manager) SetHost(h Host) { m.host = h }

// Running reports how many instances of a door are live.
func (m *Manager) Running(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[name]
}

// Run launches a door and returns when it has stopped and nothing it started is
// still running.
//
// It blocks for the door's whole lifetime, which is the point: the caller is
// holding the connection on the door's behalf and must not get it back early.
func (m *Manager) Run(ctx context.Context, spec Spec, sess Session) (Result, error) {
	if err := spec.validate(); err != nil {
		return Result{}, err
	}
	release, err := m.reserve(spec, sess.Node)
	if err != nil {
		return Result{}, err
	}
	defer release()

	// The invocation's private directory: the API socket, the descriptor, the
	// token and the dropfile all live and die with this ONE launch (§9.1.1).
	// A door that restarts gets a new token, and there is no stale-credential
	// case to reason about because there is no credential left to go stale.
	// The dropfile goes with it, which is why the file naming a caller and
	// saying where they live does not outlast their game.
	api, err := m.startAPI(&spec, sess)
	if err != nil {
		return Result{}, err
	}
	if api != nil {
		defer api.close(m)
	}

	term, err := startTerminal(spec, sess)
	if err != nil {
		return Result{}, fmt.Errorf("start %s: %w", spec.Name, err)
	}
	// Close is idempotent; the ordinary path closes below, once the door's last
	// output has been read.
	defer term.Close()

	started := m.clock.Now()
	m.log.Info("door started", "door", spec.Name, "node", sess.Node, "pid", term.Pid())

	// The door's output goes to the user until the terminal reports end of
	// stream, which is what the last process holding it open closes.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(sess.RW, term)
	}()

	// The user's keystrokes go to the door. This goroutine is NOT waited for:
	// it is parked in a read on the session, and what releases it is the caller
	// taking the connection back, which cannot happen until Run returns. It
	// writes only to the terminal, so a straggler cannot reach the user's
	// screen.
	go func() { _, _ = io.Copy(term, sess.RW) }()

	// The resize follower is stopped explicitly rather than left to the
	// context, because it has to be finished BEFORE the terminal closes: a
	// resize in flight during a close is the one operation here that touches
	// the raw descriptor. The terminal refuses a late one anyway, but a
	// shutdown that cannot produce one is better than a guard that catches it.
	stopResizes := func() {}
	if sess.Resize != nil {
		rctx, cancel := context.WithCancel(ctx)
		followed := make(chan struct{})
		go func() {
			defer close(followed)
			followResizes(rctx, term, sess.Resize)
		}()
		stopResizes = func() {
			cancel()
			<-followed
		}
	}
	defer stopResizes()

	exited := make(chan int, 1)
	go func() {
		code, err := term.Wait()
		if err != nil {
			m.log.Debug("door wait", "door", spec.Name, "err", err)
		}
		exited <- code
	}()

	stop := StopExited
	var code int
	select {
	case code = <-exited:
	case <-m.clock.After(spec.WallClock):
		stop = StopTimeLimit
		code = m.halt(term, exited, spec)
	case <-ctx.Done():
		stop = StopSessionEnded
		code = m.halt(term, exited, spec)
	}

	// Let the door's last screen through before tearing the terminal down.
	select {
	case <-drained:
	case <-m.clock.After(drainLimit):
		m.log.Warn("door output did not end; something it started still holds "+
			"the terminal", "door", spec.Name)
	}

	stopResizes()

	// Unconditionally, even on a clean exit: a door that forks and returns
	// leaves a process holding the terminal and, on Unix, the whole process
	// group behind it. Nothing above notices that, because the door itself
	// exited perfectly well.
	if err := term.KillGroup(); err != nil {
		m.log.Debug("killing the door's process group", "door", spec.Name, "err", err)
	}
	term.Close()

	res := Result{ExitCode: code, Runtime: m.clock.Since(started), Stop: stop}
	m.log.Info("door stopped", "door", spec.Name, "node", sess.Node,
		"exit", res.ExitCode, "why", res.Stop.String(), "runtime", res.Runtime)
	return res, nil
}

// halt asks the door to stop, then insists.
func (m *Manager) halt(term terminal, exited <-chan int, spec Spec) int {
	if err := term.Terminate(); err != nil {
		m.log.Debug("asking the door to stop", "door", spec.Name, "err", err)
	}
	select {
	case code := <-exited:
		return code
	case <-m.clock.After(gracePeriod):
	}

	m.log.Warn("door ignored the request to stop; killing it", "door", spec.Name)
	if err := term.KillGroup(); err != nil {
		m.log.Debug("killing the door", "door", spec.Name, "err", err)
	}
	return <-exited
}

// followResizes passes window changes through to the door's terminal.
func followResizes(ctx context.Context, term terminal, sizes <-chan Size) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-sizes:
			if !ok {
				return
			}
			_ = term.Resize(s.Width, s.Height)
		}
	}
}

// reserve takes a concurrency slot, returning the function that gives it back.
func (m *Manager) reserve(spec Spec, node int) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if spec.MaxConcurrent > 0 && m.running[spec.Name] >= spec.MaxConcurrent {
		return nil, fmt.Errorf("%w (%s allows %d at once)",
			ErrAtCapacity, spec.Name, spec.MaxConcurrent)
	}
	key := nodeKey{door: spec.Name, node: node}
	if spec.NodeLock && m.nodes[key] {
		return nil, fmt.Errorf("%w (%s on node %d)", ErrNodeBusy, spec.Name, node)
	}

	m.running[spec.Name]++
	if spec.NodeLock {
		m.nodes[key] = true
	}
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.running[spec.Name]--
		if m.running[spec.Name] <= 0 {
			delete(m.running, spec.Name)
		}
		if spec.NodeLock {
			delete(m.nodes, key)
		}
	}, nil
}

// validate rejects a Spec before anything is launched.
//
// Every rule here is one whose violation would otherwise show up as a confusing
// runtime failure, or as the server's environment quietly reaching a
// third-party binary.
func (s Spec) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("a door needs a name")
	}
	if !filepath.IsAbs(s.Path) {
		return fmt.Errorf("door %s: path %q must be absolute, so that which binary "+
			"runs does not depend on the server's PATH", s.Name, s.Path)
	}
	if info, err := os.Stat(s.Path); err != nil {
		return fmt.Errorf("door %s: %w", s.Name, err)
	} else if info.IsDir() {
		return fmt.Errorf("door %s: path %q is a directory", s.Name, s.Path)
	}
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("door %s: working directory %q must be absolute", s.Name, s.Dir)
	}
	if info, err := os.Stat(s.Dir); err != nil {
		return fmt.Errorf("door %s: working directory: %w", s.Name, err)
	} else if !info.IsDir() {
		return fmt.Errorf("door %s: working directory %q is not a directory", s.Name, s.Dir)
	}
	for _, kv := range s.Env {
		if !strings.Contains(kv, "=") {
			return fmt.Errorf("door %s: environment entry %q is not KEY=VALUE", s.Name, kv)
		}
	}
	if s.WallClock <= 0 {
		return fmt.Errorf("door %s: needs a wall-clock limit; a door with no bound "+
			"can hold a session open forever", s.Name)
	}
	return nil
}

// environ builds the environment the door will see.
//
// It REPLACES the server's environment rather than extending it. A door is a
// third-party binary running on the sysop's machine, and the server's own
// environment is where deployment secrets live — an API token in the unit file
// reaches every door ever installed if the default is inheritance. os/exec's
// default IS inheritance when Env is nil, so this must never return nil.
//
// The platform base is the exception, and it is small and specific: a process
// with no SYSTEMROOT frequently cannot start at all on Windows, and a door that
// cannot find any program on PATH is not a useful door. Anything beyond that is
// the sysop's explicit choice, which is what env_passthrough (§11.5) is for.
func (s Spec) environ(sess Session) []string {
	env := baseEnv()
	if sess.Term != "" {
		env = append(env, "TERM="+sess.Term)
	}
	env = append(env, s.Env...)
	return dedupEnv(env)
}

// dedupEnv keeps the last value for each key, which is how a shell and os/exec
// both resolve a repeated variable.
func dedupEnv(env []string) []string {
	seen := make(map[string]int, len(env))
	out := make([]string, 0, len(env))
	for _, kv := range env {
		eq := strings.Index(kv, "=")
		if eq < 0 {
			out = append(out, kv)
			continue
		}
		key := kv[:eq]
		if envKeysAreCaseInsensitive {
			key = strings.ToUpper(key)
		}
		if at, ok := seen[key]; ok {
			out[at] = kv
			continue
		}
		seen[key] = len(out)
		out = append(out, kv)
	}
	return out
}

// terminal is a pseudo-terminal with a door attached to it. The two
// implementations are a Unix pty and a Windows ConPTY (§9.3).
type terminal interface {
	io.ReadWriter

	// Pid is the door's process id, for logging.
	Pid() int
	// Resize changes the window size the door sees.
	Resize(width, height int) error
	// Wait blocks until the door exits and returns its status.
	Wait() (int, error)
	// Terminate asks the door to stop, where the platform has a way to ask.
	Terminate() error
	// KillGroup ends the door and everything it started, without asking.
	KillGroup() error
	// Close releases the terminal. It is safe to call more than once.
	Close() error
}
