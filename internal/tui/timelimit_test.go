package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/store"
)

// advance moves the fixture's virtual clock. The fixture types it as the
// interface, and only the concrete Virtual can be driven.
func advance(t *testing.T, f *fixture, d time.Duration) {
	t.Helper()
	v, ok := f.clock.(*clock.Virtual)
	if !ok {
		t.Fatalf("the fixture clock is %T, not a virtual one", f.clock)
	}
	v.Advance(d)
}

// limitedSession opens an authenticated session with a time limit.
//
// The check message is sent by hand rather than waited for. watchTimeLimit
// parks on the injected clock, and the harness deliberately skips commands that
// do not return promptly — which is the right behaviour for the chat watcher
// and would make this a test of the harness's timeout rather than of the limit.
// Advancing the clock and sending the message tests what the command would
// have delivered, deterministically.
func limitedSession(t *testing.T, f *fixture, nick string, limit time.Duration) *session {
	t.Helper()
	u, err := f.store.GetUser(f.ctx, nick)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, nick)
	cfg.User = u
	cfg.SessionLimit = limit
	return newSession(t, cfg)
}

func TestSessionWithNoLimitIsNeverCut(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	s := limitedSession(t, f, "austin", 0)

	if _, limited := s.model.Remaining(); limited {
		t.Error("a session with no limit reports one")
	}

	advance(t, f, 365*24*time.Hour)
	s.send(timeCheckMsg{})

	if s.model.quitting {
		t.Error("a session with no limit was cut off")
	}
	if s.model.status != "" {
		t.Errorf("a session with no limit was warned: %q", s.model.status)
	}
}

func TestSessionIsWarnedThenCut(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	s := limitedSession(t, f, "austin", 10*time.Minute)

	if left, limited := s.model.Remaining(); !limited || left != 10*time.Minute {
		t.Fatalf("a fresh session has %v left (limited=%v)", left, limited)
	}

	// Five minutes in: the first mark.
	advance(t, f, 5*time.Minute)
	s.send(timeCheckMsg{})
	if s.model.quitting {
		t.Fatal("cut off at the first warning")
	}
	if !strings.Contains(s.model.status, "5 minutes") {
		t.Errorf("first warning is %q, want it to mention 5 minutes", s.model.status)
	}
	if s.model.statusErr {
		t.Error("the warning is styled as an error; it is not one yet")
	}

	// A second check inside the same window must not repeat the warning, or a
	// user near the end of a call sees nothing else.
	s.model.status = ""
	advance(t, f, 30*time.Second)
	s.send(timeCheckMsg{})
	if s.model.status != "" {
		t.Errorf("the same warning was given twice: %q", s.model.status)
	}

	// Under a minute: the second mark.
	advance(t, f, 4*time.Minute)
	s.send(timeCheckMsg{})
	if s.model.quitting {
		t.Fatal("cut off at the second warning")
	}
	if !strings.Contains(s.model.status, "30 seconds") {
		t.Errorf("second warning is %q, want it to mention 30 seconds", s.model.status)
	}

	// And time runs out.
	advance(t, f, time.Minute)
	s.send(timeCheckMsg{})
	if !s.model.quitting {
		t.Fatal("the session outlived its limit")
	}
	if left, _ := s.model.Remaining(); left != 0 {
		t.Errorf("remaining is %v after the limit; it should floor at zero", left)
	}
}

// A caller dropped without explanation assumes a fault and reconnects, which on
// a board with a time limit is the behaviour the limit exists to prevent.
func TestTheCutSaysWhy(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	s := limitedSession(t, f, "austin", 30*time.Minute)

	advance(t, f, 31*time.Minute)
	s.send(timeCheckMsg{})

	if !s.model.quitting {
		t.Fatal("the session was not cut")
	}
	view := s.model.View()
	if !strings.Contains(view, "30 minutes") {
		t.Errorf("the goodbye does not say what the limit was:\n%s", view)
	}
	// And an ordinary quit still gets the ordinary goodbye.
	plain := limitedSession(t, f, "austin", 0)
	next, _ := plain.model.leave()
	if got := next.(Model).View(); !strings.Contains(got, "Thanks for calling") {
		t.Errorf("the ordinary goodbye changed:\n%s", got)
	}
}

// The limit shares lines between callers. The sysop is not competing for one,
// and a board that disconnects its own operator mid-repair has turned a
// courtesy into a hazard.
func TestSysopIsNotTimedOut(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateUser(f.ctx, store.CreateUserOptions{
		Nick: "ciadmin", CanLogin: true, IsSysop: true,
		Capabilities: store.DefaultCapabilities,
	}); err != nil {
		t.Fatal(err)
	}
	s := limitedSession(t, f, "ciadmin", 10*time.Minute)

	if !s.model.sysop {
		t.Fatal("the fixture did not produce a sysop session")
	}
	if _, limited := s.model.Remaining(); limited {
		t.Error("a sysop session is on the clock")
	}

	advance(t, f, time.Hour)
	s.send(timeCheckMsg{})
	if s.model.quitting {
		t.Error("the sysop was timed out of their own board")
	}
}

// A door handed a bare zero cannot tell "no limit" from "no time left", and
// those call for opposite behaviour (§9.1).
func TestRemainingDistinguishesNoLimitFromNoTime(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")

	unlimited := limitedSession(t, f, "austin", 0)
	left, limited := unlimited.model.Remaining()
	if limited || left != 0 {
		t.Errorf("unlimited session reports %v limited=%v", left, limited)
	}

	limitedSess := limitedSession(t, f, "austin", time.Minute)
	advance(t, f, 2*time.Minute)
	left, limited = limitedSess.model.Remaining()
	if !limited {
		t.Error("an expired session reports itself unlimited")
	}
	if left != 0 {
		t.Errorf("an expired session reports %v left, want 0", left)
	}
}

// The watcher sleeps until the next thing worth saying rather than polling, so
// a board full of sessions is not a board full of timers.
func TestTheWatcherSleepsUntilTheNextMark(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	s := limitedSession(t, f, "austin", time.Hour)

	cases := []struct {
		left time.Duration
		want time.Duration
	}{
		// An hour in hand: nothing to say for another 55 minutes.
		{time.Hour, 55 * time.Minute},
		// Past the five-minute mark, the next event is the one-minute mark.
		{4 * time.Minute, 3 * time.Minute},
		// Inside the last minute, the next event is the cut.
		{30 * time.Second, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := s.model.untilNextTimeEvent(tc.left); got != tc.want {
			t.Errorf("with %v left the watcher sleeps %v, want %v", tc.left, got, tc.want)
		}
	}

	// After the five-minute mark has been given, it is not waited for again.
	s.model.timeWarned = 5 * time.Minute
	if got := s.model.untilNextTimeEvent(10 * time.Minute); got != 9*time.Minute {
		t.Errorf("after warning at 5m, the watcher sleeps %v with 10m left, want 9m", got)
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "1 second"},
		{30 * time.Second, "30 seconds"},
		{time.Minute, "1 minute"},
		{5 * time.Minute, "5 minutes"},
		{59 * time.Minute, "59 minutes"},
		{time.Hour, "1 hour"},
		{90 * time.Minute, "1 hour 30 minutes"},
		{2 * time.Hour, "2 hours"},
		{150 * time.Minute, "2 hours 30 minutes"},
		// The case this function exists for: a user told "4m59.83s remaining"
		// has been given a number, not a warning.
		{4*time.Minute + 59*time.Second + 830*time.Millisecond, "5 minutes"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The watcher re-arms itself, or a session gets one warning and then runs past
// its limit unnoticed.
//
// This one enables the watchers the rest of the file turns off, so it is
// testing the command the harness normally skips. It only inspects whether a
// command was produced — running it would park until the clock moves, which is
// exactly why the other tests drive the message directly.
func TestTheWatcherReArmsAfterEachMark(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")

	u, err := f.store.GetUser(f.ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, "austin")
	cfg.User = u
	cfg.SessionLimit = 10 * time.Minute
	cfg.DisableWatchers = false

	m := New(cfg)
	if m.watchTimeLimit() == nil {
		t.Fatal("a limited session started no watcher")
	}

	// After a warning, another watcher: the next mark is still ahead.
	advance(t, f, 5*time.Minute)
	next, cmd := m.enforceTimeLimit()
	m = next.(Model)
	if m.quitting {
		t.Fatal("cut off at the first warning")
	}
	if cmd == nil {
		t.Error("the watcher did not re-arm after a warning")
	}

	// After the cut, no more: the session is over.
	advance(t, f, 6*time.Minute)
	next, _ = m.enforceTimeLimit()
	if !next.(Model).quitting {
		t.Fatal("the session outlived its limit")
	}
}

// A session with no limit starts no watcher at all, so an unlimited board pays
// nothing for the feature.
func TestNoWatcherWithoutALimit(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")

	cfg := f.config(IntentAuthenticated, "austin")
	cfg.DisableWatchers = false
	if New(cfg).watchTimeLimit() != nil {
		t.Error("an unlimited session started a time watcher")
	}
}
