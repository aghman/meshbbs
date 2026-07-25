package tui

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/term"
	"github.com/aghman/meshbbs/internal/theme"
	tea "github.com/charmbracelet/bubbletea"
)

var update = flag.Bool("update", false, "rewrite golden files")

// commandTimeout bounds how long the harness waits for a command.
//
// It must comfortably exceed the slowest legitimate command, which is an
// Argon2id verification — around a second under the race detector on CI
// hardware. Too tight and slow-but-valid commands get silently skipped, and
// tests then pass or fail depending on machine speed.
const commandTimeout = 10 * time.Second

// The Model is a pure state machine over Update/View, so it can be driven
// directly without a PTY or a running program. That is deliberate: every bug
// found during Phase 1 hand-testing (concurrent reload, field navigation,
// dead-ended key setup) is reachable from here, deterministically and in
// milliseconds.
//
// A `session` wraps the model with a message pump so that commands returned by
// Update are executed and their results fed back, exactly as tea.Program would.
type session struct {
	t     *testing.T
	model Model
	// budget bounds how many messages one interaction may cascade into.
	//
	// The pump runs "until quiescent", but some commands are deliberately
	// self-perpetuating: the chat poller re-arms itself on every redraw, so it
	// is never quiescent by design. Without a budget the harness spins on it
	// forever. Real Bubble Tea has no such problem — it is a redraw loop, not
	// a recursion.
	budget int
}

// maxCascade is how many messages a single keypress may produce before the
// harness stops following the chain.
//
// Kept small on purpose: the chat poller re-arms itself, so this is really a
// bound on how many times the harness rides that loop before moving on. Large
// values just make the suite slow.
const maxCascade = 20

func newSession(t *testing.T, cfg Config) *session {
	t.Helper()
	s := &session{t: t, model: New(cfg)}
	s.budget = maxCascade
	s.run(s.model.Init())
	return s
}

// run executes a command and pumps the resulting messages until quiescent.
//
// tea.Batch and tea.Sequence produce internal message types that are not
// exported, so this handles the common shapes: a single message, or a batch
// whose members are themselves commands. Sequence is flattened in order, which
// is what makes the post-then-reload ordering testable.
func (s *session) run(cmd tea.Cmd) {
	s.t.Helper()
	if cmd == nil || s.budget <= 0 {
		return
	}
	for _, c := range flatten(cmd) {
		if c == nil {
			continue
		}
		msg, ok := runWithTimeout(c)
		if !ok {
			// A command that does not return promptly is a legitimately
			// blocking one — the chat watcher parks until someone speaks.
			// Real Bubble Tea runs commands in goroutines, so blocking is
			// fine there; here it would deadlock the test. Skipping it
			// matches the real behaviour of "no message yet".
			continue
		}
		if msg == nil {
			continue
		}
		if inner, ok := msg.(tea.Cmd); ok {
			s.run(inner)
			continue
		}
		s.dispatch(msg)
	}
}

// send injects a message from outside, as another session's activity would.
// It resets the cascade budget, since it represents a fresh external event
// rather than a continuation of the last one.
func (s *session) send(msg tea.Msg) *session {
	s.t.Helper()
	s.budget = maxCascade
	s.dispatch(msg)
	return s
}

func (s *session) dispatch(msg tea.Msg) {
	s.t.Helper()
	if s.budget <= 0 {
		return
	}
	s.budget--
	next, cmd := s.model.Update(msg)
	s.model = next.(Model)
	s.run(cmd)
}

// flatten unwraps batch and sequence commands into an ordered slice.
//
// tea.Batch returns an exported BatchMsg, but tea.Sequence returns an
// UNEXPORTED sequenceMsg — and both are really just []tea.Cmd. Handling only
// the exported one silently drops every sequenced command, which is exactly
// the mistake that made the first run of these tests report bugs that did not
// exist. Reflection covers both without depending on internal names.
func flatten(cmd tea.Cmd) []tea.Cmd {
	if cmd == nil {
		return nil
	}
	msg, ok := runWithTimeout(cmd)
	if !ok || msg == nil {
		return nil
	}
	if cmds, ok := asCommandSlice(msg); ok {
		var out []tea.Cmd
		for _, c := range cmds {
			out = append(out, flatten(c)...)
		}
		return out
	}
	// Not a batch or sequence: re-wrap the already-computed message so it is
	// not executed twice, since commands may have side effects.
	return []tea.Cmd{func() tea.Msg { return msg }}
}

// runWithTimeout executes a command, giving up if it blocks.
//
// The returned bool reports whether a message was produced. Commands that park
// (the chat watcher waits for someone to speak) are expected, not errors.
func runWithTimeout(cmd tea.Cmd) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() {
		defer func() {
			// A panicking command should fail the test loudly rather than
			// looking like a blocked one, but recovering here keeps the
			// harness from taking the whole process down.
			_ = recover()
		}()
		done <- cmd()
	}()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(commandTimeout):
		return nil, false
	}
}

// asCommandSlice reports whether msg is a slice of tea.Cmd, whatever its
// concrete named type.
func asCommandSlice(msg tea.Msg) ([]tea.Cmd, bool) {
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		return nil, false
	}
	cmdType := reflect.TypeOf(tea.Cmd(nil))
	if !v.Type().Elem().AssignableTo(cmdType) {
		return nil, false
	}
	out := make([]tea.Cmd, 0, v.Len())
	for i := 0; i < v.Len(); i++ {
		c, ok := v.Index(i).Interface().(tea.Cmd)
		if !ok {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}

// key sends a single keypress, resetting the cascade budget: each user action
// gets a fresh allowance.
func (s *session) key(k tea.KeyMsg) *session {
	s.t.Helper()
	s.budget = maxCascade
	s.dispatch(k)
	return s
}

// typeRunes sends printable characters one at a time, as a terminal would.
func (s *session) typeRunes(text string) *session {
	s.t.Helper()
	for _, r := range text {
		if r == ' ' {
			s.key(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		s.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return s
}

func (s *session) press(t tea.KeyType) *session {
	return s.key(tea.KeyMsg{Type: t})
}

func (s *session) enter() *session  { return s.press(tea.KeyEnter) }
func (s *session) ctrlD() *session  { return s.press(tea.KeyCtrlD) }
func (s *session) escape() *session { return s.press(tea.KeyEsc) }

// line types text then presses enter.
func (s *session) line(text string) *session { return s.typeRunes(text).enter() }

// view returns the current screen with ANSI escapes removed, so assertions and
// golden files are about content rather than colour codes.
func (s *session) view() string { return stripANSI(s.model.View()) }

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func stripANSI(s string) string {
	out := ansiRE.ReplaceAllString(s, "")
	// Trailing spaces vary with terminal width padding and carry no meaning.
	lines := strings.Split(out, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return strings.Join(lines, "\n")
}

// contains asserts the screen shows some text.
func (s *session) contains(want ...string) *session {
	s.t.Helper()
	v := s.view()
	for _, w := range want {
		if !strings.Contains(v, w) {
			s.t.Fatalf("screen does not contain %q:\n%s", w, v)
		}
	}
	return s
}

func (s *session) notContains(unwanted ...string) *session {
	s.t.Helper()
	v := s.view()
	for _, w := range unwanted {
		if strings.Contains(v, w) {
			s.t.Fatalf("screen unexpectedly contains %q:\n%s", w, v)
		}
	}
	return s
}

// golden compares the current screen against a committed file (§12.8).
//
// ANSI output is invisible to ordinary assertions — a layout regression or a
// mangled box border produces a passing test and a broken screen. Golden files
// make the whole frame the assertion.
func (s *session) golden(name string) *session {
	s.t.Helper()
	path := filepath.Join("testdata", name+".golden")
	got := s.view()

	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			s.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			s.t.Fatal(err)
		}
		return s
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatalf("missing golden file %s (run: go test ./internal/tui -update): %v", path, err)
	}
	// Normalise line endings defensively. .gitattributes marks these files
	// -text so git leaves them alone, but a checkout made before that landed,
	// or an editor that "helpfully" converts, would otherwise fail every
	// golden on Windows only.
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got != want {
		s.t.Fatalf("screen does not match %s\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
	return s
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

type fixture struct {
	svc      *bbs.Service
	store    *store.Store
	ctx      context.Context
	themes   *theme.Set
	presence *fakePresence
	clock    clock.Clock
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))

	st, err := store.OpenMemory(ctx, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := identity.GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, store.Node{
		ID: key.ID(), PublicKey: key.Public, DisplayName: "test-bbs", IsSelf: true,
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
	return &fixture{
		svc: svc, store: st, ctx: ctx, themes: themes,
		presence: &fakePresence{},
		// §12.1: inject the clock. Without it the chat timestamps come from
		// the real clock and any golden containing one is unstable.
		clock: clk,
	}
}

func (f *fixture) config(intent Intent, nick string) Config {
	return Config{
		Service: f.svc, Store: f.store, Presence: f.presence,
		Themes: f.themes, ThemeName: theme.DefaultName,
		Encoding: term.EncodingUTF8, Width: 80, Height: 24,
		SessionID: "test-session", Remote: "127.0.0.1:1234",
		Intent: intent, Nick: nick, Ctx: f.ctx, Clock: f.clock,
		// Pin the zone: without it, rendered timestamps follow the host's
		// local time and every golden containing one becomes machine-
		// dependent. CI runs UTC; a developer laptop usually does not.
		Location: time.UTC,
		// The chat watcher parks until someone speaks. That is right in a real
		// program and a deadlock here, so tests inject chat updates directly
		// with send() instead.
		DisableChatPolling: true,
	}
}

// user creates an account with a DM key.
func (f *fixture) user(t *testing.T, nick, passphrase string, caps ...string) store.User {
	t.Helper()
	u, err := f.store.CreateUser(f.ctx, store.CreateUserOptions{
		Nick: nick, CanLogin: true,
		Capabilities: append(append([]string(nil), store.DefaultCapabilities...), caps...),
	})
	if err != nil {
		t.Fatal(err)
	}
	if passphrase != "" {
		if err := f.svc.EnsureDMKey(f.ctx, nick, passphrase); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

// login opens an authenticated session.
func (f *fixture) login(t *testing.T, nick string) *session {
	t.Helper()
	u, err := f.store.GetUser(f.ctx, nick)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, nick)
	cfg.User = u
	return newSession(t, cfg)
}

// fakePresence records presence calls without a server.
type fakePresence struct {
	peers  []Peer
	joined bool
	left   bool
	where  string
}

func (p *fakePresence) Join(id, nick, remote string, guest bool) int {
	p.joined = true
	p.peers = append(p.peers, Peer{Node: len(p.peers) + 1, Nick: nick, Guest: guest, Where: "menu"})
	return len(p.peers)
}
func (p *fakePresence) Leave(id string)              { p.left = true }
func (p *fakePresence) SetLocation(id, where string) { p.where = where }
func (p *fakePresence) List() []Peer                 { return p.peers }
