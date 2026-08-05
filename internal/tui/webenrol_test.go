package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/store"
)

// The SSH half of [D18]: an authenticated session mints a code that a browser
// can redeem for a passkey, and for nothing else.

func webCfg(f *fixture, nick string) Config {
	cfg := f.config(IntentAuthenticated, nick)
	cfg.WebEnabled = true
	cfg.WebURL = "https://bbs.example.com"
	return cfg
}

func TestWebEnrolCodeIsIssuedAndRedeemable(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	u, err := f.store.GetUser(f.ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	cfg := webCfg(f, "austin")
	cfg.User = u

	s := newSession(t, cfg).contains("[P] Passkey for the web").typeRunes("p")

	code := s.model.webCode
	if code == "" {
		t.Fatal("pressing P produced no code")
	}
	s.contains(code, "https://bbs.example.com")

	// The store keeps the hash, never the code itself.
	redeemed, err := f.store.RedeemEnrolmentCode(f.ctx, auth.HashEnrolmentCode(code))
	if err != nil {
		t.Fatalf("the displayed code should redeem: %v", err)
	}
	if redeemed.Nick != "austin" {
		t.Errorf("code redeemed to %q, want austin", redeemed.Nick)
	}
}

// TestWebEnrolCodeSaysWhatItCannotDo is a wording test, and it earns its place:
// the code looks exactly like a password, and a user who believes it is one
// will treat it like one. If this text is ever dropped the screen still works
// and the user is quietly misinformed, which no other test would catch.
func TestWebEnrolCodeSaysWhatItCannotDo(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	u, _ := f.store.GetUser(f.ctx, "austin")
	cfg := webCfg(f, "austin")
	cfg.User = u

	view := newSession(t, cfg).typeRunes("p").view()
	for _, want := range []string{"cannot log anyone in", "works once"} {
		if !strings.Contains(view, want) {
			t.Errorf("enrolment screen no longer says %q:\n%s", want, view)
		}
	}
}

// TestWebEnrolReissueCancelsThePrevious pins the property that makes a
// shoulder-surfed code self-healing.
func TestWebEnrolReissueCancelsThePrevious(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	u, _ := f.store.GetUser(f.ctx, "austin")
	cfg := webCfg(f, "austin")
	cfg.User = u

	s := newSession(t, cfg).typeRunes("p")
	first := s.model.webCode

	s.escape().typeRunes("p") // back to the menu, then ask again
	second := s.model.webCode

	if first == second {
		t.Fatal("re-issuing produced the same code")
	}
	if _, err := f.store.RedeemEnrolmentCode(f.ctx, auth.HashEnrolmentCode(first)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the superseded code still redeems: %v", err)
	}
	if _, err := f.store.RedeemEnrolmentCode(f.ctx, auth.HashEnrolmentCode(second)); err != nil {
		t.Errorf("the current code should redeem: %v", err)
	}
}

// TestWebEnrolIsHiddenWhenWebIsOff — a code that leads nowhere is worse than
// no offer, so the menu item appears only when the sysop has turned the web on.
func TestWebEnrolIsHiddenWhenWebIsOff(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")

	s := f.login(t, "austin").notContains("[P] Passkey for the web")
	s.typeRunes("p")
	if s.model.screen != screenMenu {
		t.Error("P navigated somewhere with the web UI disabled")
	}
	if s.model.webCode != "" {
		t.Error("a code was minted with the web UI disabled")
	}
}

// TestWebEnrolRefusesGuests — a guest has no account to enrol onto.
func TestWebEnrolRefusesGuests(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(IntentGuest, "guest")
	cfg.WebEnabled = true

	// Hidden from the menu, and refused if pressed anyway — the second is what
	// matters, since the key works whether or not a menu line advertises it.
	s := newSession(t, cfg).notContains("[P] Passkey for the web").typeRunes("p")
	if s.model.webCode != "" {
		t.Error("a guest session minted an enrolment code")
	}
	if !s.model.statusErr {
		t.Error("a guest pressing P got no explanation")
	}
	s.contains("Guests have no account")
}

// TestWebEnrolCodeIsClearedOnLeaving keeps the plaintext from outliving the
// screen that shows it. The store never had it; this session should not keep it
// either.
func TestWebEnrolCodeIsClearedOnLeaving(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	u, _ := f.store.GetUser(f.ctx, "austin")
	cfg := webCfg(f, "austin")
	cfg.User = u

	s := newSession(t, cfg).typeRunes("p")
	if s.model.webCode == "" {
		t.Fatal("no code issued")
	}
	s.escape()
	if s.model.webCode != "" {
		t.Errorf("code survived leaving the screen: %q", s.model.webCode)
	}
}
