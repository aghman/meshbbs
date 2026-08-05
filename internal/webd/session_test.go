package webd

import (
	"errors"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/go-webauthn/webauthn/webauthn"
)

func testSessions(clk clock.Clock) *sessionStore {
	return newSessionStore(clk, Options{
		MaxSessions:             3,
		MaxSessionsPerUser:      2,
		IdleTimeoutMins:         30,
		UnlockedIdleTimeoutMins: 10,
		SessionTTLHours:         12,
	})
}

func TestSessionRoundTrip(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := testSessions(clk)

	sess, err := s.Create("austin")
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" {
		t.Fatal("session has no ID")
	}

	got, err := s.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Nick != "austin" {
		t.Errorf("nick = %q", got.Nick)
	}

	s.Delete(sess.ID)
	if _, err := s.Get(sess.ID); !errors.Is(err, ErrNoSession) {
		t.Errorf("deleted session = %v, want ErrNoSession", err)
	}
}

// TestSessionIDsAreUnguessable is a smoke test for the failure that actually
// happens: a generator that forgets to read randomness and returns a constant.
func TestSessionIDsAreUnguessable(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := newSessionStore(clk, Options{IdleTimeoutMins: 30, SessionTTLHours: 12})

	seen := map[string]bool{}
	for range 100 {
		sess, err := s.Create("austin")
		if err != nil {
			t.Fatal(err)
		}
		if seen[sess.ID] {
			t.Fatalf("duplicate session ID %q", sess.ID)
		}
		if len(sess.ID) < 32 {
			t.Fatalf("session ID is only %d characters", len(sess.ID))
		}
		seen[sess.ID] = true
	}
}

func TestSessionCaps(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := testSessions(clk)

	// Two per user is the limit.
	if _, err := s.Create("austin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("austin"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("austin"); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("third session for one user = %v, want ErrTooManySessions", err)
	}

	// Three overall, so one more account fits and the next does not.
	if _, err := s.Create("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("carol"); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("session past the global cap = %v, want ErrTooManySessions", err)
	}
}

func TestSessionIdleExpiry(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := testSessions(clk)

	sess, err := s.Create("austin")
	if err != nil {
		t.Fatal(err)
	}

	// Activity keeps it alive.
	clk.Advance(25 * time.Minute)
	if _, err := s.Get(sess.ID); err != nil {
		t.Fatalf("session should still be live: %v", err)
	}
	clk.Advance(25 * time.Minute)
	if _, err := s.Get(sess.ID); err != nil {
		t.Fatalf("activity should have refreshed the idle timer: %v", err)
	}

	// Silence does not.
	clk.Advance(31 * time.Minute)
	if _, err := s.Get(sess.ID); !errors.Is(err, ErrNoSession) {
		t.Errorf("idle session = %v, want ErrNoSession", err)
	}
}

// TestUnlockedSessionsExpireSooner is the security-relevant one. A browser tab
// closing is a far less reliable goodbye than an SSH disconnect — a killed tab,
// a locked phone and a sleeping laptop may send nothing at all — so for a
// session holding a mail passphrase in memory this timer is the actual bound on
// exposure, not a convenience (webui.md §9).
func TestUnlockedSessionsExpireSooner(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := testSessions(clk)

	locked, err := s.Create("austin")
	if err != nil {
		t.Fatal(err)
	}
	unlocked, err := s.Create("bob")
	if err != nil {
		t.Fatal(err)
	}
	s.SetUnlocked(unlocked.ID, true)

	// Past the unlocked bound, short of the ordinary one.
	clk.Advance(11 * time.Minute)

	if _, err := s.Get(locked.ID); err != nil {
		t.Errorf("an ordinary session should survive 11 minutes idle: %v", err)
	}
	if _, err := s.Get(unlocked.ID); !errors.Is(err, ErrNoSession) {
		t.Error("a session holding a mail passphrase outlived its shorter bound")
	}
}

// TestSessionAbsoluteTTL — however active a session is, it does not live
// forever.
func TestSessionAbsoluteTTL(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := testSessions(clk)

	sess, err := s.Create("austin")
	if err != nil {
		t.Fatal(err)
	}
	// Stay busy the whole time, so only the absolute cap can end it.
	for range 30 {
		clk.Advance(25 * time.Minute)
		if _, err := s.Get(sess.ID); err != nil {
			break
		}
	}
	if _, err := s.Get(sess.ID); !errors.Is(err, ErrNoSession) {
		t.Error("an active session outlived its absolute TTL")
	}
}

// TestExpiredSessionsFreeTheirSlot — otherwise a busy day fills the cap
// permanently and the BBS reports itself full while nobody is connected.
func TestExpiredSessionsFreeTheirSlot(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s := testSessions(clk)

	for _, nick := range []string{"austin", "austin", "bob"} {
		if _, err := s.Create(nick); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create("carol"); !errors.Is(err, ErrTooManySessions) {
		t.Fatal("expected the store to be full")
	}

	clk.Advance(31 * time.Minute)
	if _, err := s.Create("carol"); err != nil {
		t.Errorf("slots should free up once sessions expire: %v", err)
	}
	if n := s.Count(); n != 1 {
		t.Errorf("live sessions = %d, want 1", n)
	}
}

func TestCeremonyIsSingleUse(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	c := newCeremonyStore(clk)

	id, err := c.Put("austin", webauthnSessionData())
	if err != nil {
		t.Fatal(err)
	}

	cer, ok := c.Take(id)
	if !ok {
		t.Fatal("ceremony should be retrievable")
	}
	if cer.Nick != "austin" {
		t.Errorf("nick = %q", cer.Nick)
	}
	// A challenge that could be answered twice is a replay waiting to happen.
	if _, ok := c.Take(id); ok {
		t.Error("ceremony was retrievable a second time")
	}
}

func TestCeremonyExpires(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	c := newCeremonyStore(clk)

	id, err := c.Put("austin", webauthnSessionData())
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(ceremonyTTL + time.Minute)
	if _, ok := c.Take(id); ok {
		t.Error("an expired ceremony was still accepted")
	}
}

// webauthnSessionData is a minimal challenge record; these tests care about the
// store's lifecycle, not the library's contents.
func webauthnSessionData() webauthn.SessionData {
	return webauthn.SessionData{Challenge: "test-challenge"}
}
