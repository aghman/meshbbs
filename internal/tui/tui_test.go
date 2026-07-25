package tui

import (
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// ---------------------------------------------------------------------------
// Regressions for the bugs found by hand-driving Phase 1
// ---------------------------------------------------------------------------

// A post must be visible to its author immediately.
//
// The original code used tea.Batch for the write and the reload; Batch runs
// commands concurrently, so the reload could read the area before the write
// committed and the author saw "No posts yet" over their own post.
func TestPostAppearsImmediatelyAfterSending(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	s := f.login(t, "austin")

	s.typeRunes("m").enter()           // areas -> open "general"
	s.typeRunes("p")                   // compose
	s.line("Solar setup")              // subject
	s.typeRunes("40W into a 12V AGM.") // body
	s.ctrlD()

	s.contains("Solar setup", "40W into a 12V AGM.", "message 1 of 1")
	s.notContains("No posts yet")
}

// Enter must advance out of the single-line compose fields.
//
// Originally only the subject field handled Enter, so a recipient, subject and
// body all concatenated into "To:" and the send was refused as empty.
func TestEnterAdvancesThroughComposeFields(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	f.user(t, "bob", "bobpw")

	s := f.login(t, "austin")
	s.typeRunes("e") // mail -> unlock
	s.line("pw")     // passphrase
	s.typeRunes("c") // compose

	s.line("bob")     // To, then enter
	s.line("Antenna") // Subject, then enter
	s.typeRunes("J-pole time.")

	// Each value must have landed in its own field, not run together.
	v := s.view()
	if !strings.Contains(v, "To: bob") {
		t.Fatalf("recipient did not stay in the To field:\n%s", v)
	}
	if !strings.Contains(v, "Subject: Antenna") {
		t.Fatalf("subject did not land in the Subject field:\n%s", v)
	}
	if strings.Contains(v, "To: bobAntenna") {
		t.Fatalf("fields concatenated into To:\n%s", v)
	}

	s.ctrlD()
	s.contains("Message sent to bob")
}

// An account with no DM key must be offered one, not dead-ended.
//
// Accounts created by the CLI have no key, because the CLI cannot know a
// passphrase. The original code sent them to the unlock screen, where every
// attempt failed with "no DM key yet" and no way forward.
func TestAccountWithoutDMKeyIsOfferedKeySetup(t *testing.T) {
	f := newFixture(t)
	f.user(t, "cliuser", "") // deliberately no DM key

	s := f.login(t, "cliuser")
	s.typeRunes("e")

	s.contains("Create Your Message Key", "do not have a message key")
	s.notContains("Unlock Mail")

	s.line("newphrase123")
	s.line("newphrase123")
	s.contains("Mail")

	// The key must actually exist now, and be usable.
	if _, err := f.store.DMPublicKey(f.ctx, "cliuser"); err != nil {
		t.Fatalf("key setup did not create a DM key: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Registration (§6.7)
// ---------------------------------------------------------------------------

func TestSignupCreatesAccountAndEnrolsOfferedKey(t *testing.T) {
	f := newFixture(t)
	cfg := f.config(IntentSignup, "")
	cfg.PublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI testkey"
	cfg.KeyFP = "SHA256:testfingerprint"
	s := newSession(t, cfg)

	s.contains("New User Registration")
	s.line("newbie")
	// With a key offered, the password step says so and may be skipped.
	s.contains("SSH key will be enrolled")
	s.enter()
	s.line("messagephrase")
	s.line("messagephrase")

	// §6.7 requires the unrecoverable-passphrase warning at signup, in plain
	// language, before the account exists.
	s.contains("cannot be undone", "permanently unreadable")
	s.typeRunes("y")

	s.contains("Welcome to the BBS, newbie")

	u, err := f.store.GetUser(f.ctx, "newbie")
	if err != nil {
		t.Fatalf("account was not created: %v", err)
	}
	if !u.CanLogin {
		t.Error("account cannot log in")
	}
	// The key SSH already handed us must be enrolled, so the next login needs
	// no password (§5.1).
	if _, err := f.store.UserByFingerprint(f.ctx, "SHA256:testfingerprint"); err != nil {
		t.Errorf("offered key was not enrolled: %v", err)
	}
}

// [N7]: registration is open, but the shared airtime is gated. A brand new
// account must not be able to post to a federated area.
func TestNewAccountCannotPostToFederatedArea(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateArea(f.ctx, "meshwide", "Federated", true); err != nil {
		t.Fatal(err)
	}
	f.user(t, "newbie", "pw")

	caps, err := f.store.Capabilities(f.ctx, "newbie")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c == store.CapPostFederated {
			t.Fatal("a new account has post_federated by default; [N7] withholds it")
		}
	}

	s := f.login(t, "newbie")
	s.typeRunes("m")
	// Navigate to the federated area.
	for i := 0; i < 5; i++ {
		if strings.Contains(s.view(), "> meshwide") {
			break
		}
		s.press(tea.KeyDown)
	}
	s.enter()
	s.contains("Scope: Federated")

	s.typeRunes("p")
	s.line("subject")
	s.typeRunes("body text")
	s.ctrlD()

	// The refusal must name the remedy, since the user cannot grant it.
	s.contains(store.CapPostFederated)
	s.contains("sysop")
}

func TestSignupRejectsTakenNick(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")

	s := newSession(t, f.config(IntentSignup, ""))
	s.line("austin")
	s.contains("already taken")
	// Still on the nick step.
	s.contains("Nick:")
}

func TestSignupRejectsReservedNick(t *testing.T) {
	f := newFixture(t)
	s := newSession(t, f.config(IntentSignup, ""))
	s.line("sysop")
	s.contains("reserved")
}

// ---------------------------------------------------------------------------
// Mail and key custody (§8.2)
// ---------------------------------------------------------------------------

// The subject travels inside the sealed payload, so the inbox cannot show it
// before unlocking — and must say so rather than showing a blank column.
func TestInboxDoesNotRevealSubjects(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	f.user(t, "bob", "bobpw")

	if _, err := f.svc.SendDM(f.ctx, "bob", "austin", "Repeater codes", "the tone is 100.0"); err != nil {
		t.Fatal(err)
	}

	s := f.login(t, "austin")
	s.typeRunes("e").line("pw")

	s.contains("bob", "(encrypted")
	s.notContains("Repeater codes")

	// Opening it reveals both subject and body.
	s.enter()
	s.contains("Repeater codes", "the tone is 100.0")
}

func TestWrongPassphraseIsRejected(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "correct-phrase")

	s := f.login(t, "austin")
	s.typeRunes("e")
	s.contains("Unlock Mail")
	s.line("wrong-phrase")

	s.contains("Wrong passphrase")
	// Still on the unlock screen, not in the mailbox.
	s.contains("Unlock Mail", "Passphrase:")
}

// The passphrase must not survive the session ending (§8.2).
func TestLeavingClearsThePassphrase(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")

	s := f.login(t, "austin")
	s.typeRunes("e").line("pw")
	if s.model.passphrase == "" {
		t.Fatal("test setup wrong: session should be unlocked")
	}

	s.key(tea.KeyMsg{Type: tea.KeyCtrlC})
	if s.model.passphrase != "" {
		t.Fatal("the passphrase survived the session ending")
	}
	if s.model.unlocked {
		t.Fatal("the session is still marked unlocked after leaving")
	}
	if !f.presence.left {
		t.Error("presence was not notified of the departure")
	}
}

// ---------------------------------------------------------------------------
// Guests (§5.1)
// ---------------------------------------------------------------------------

func TestGuestIsReadOnly(t *testing.T) {
	f := newFixture(t)
	s := newSession(t, f.config(IntentGuest, "guest"))

	s.contains("guest, read-only", "ssh new@")

	s.typeRunes("e")
	s.contains("Guests cannot read mail")

	s.typeRunes("m").enter().typeRunes("p")
	s.contains("Guests cannot post")
}

// ---------------------------------------------------------------------------
// Auth recovery (§5.1)
// ---------------------------------------------------------------------------

// An existing account presenting an unknown key must get an explanation and a
// route back in, never the registration flow.
func TestKeyUnknownScreenExplainsRecovery(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")

	cfg := f.config(IntentKeyUnknown, "austin")
	cfg.AuthNote = "an account with that name exists, but this key is not enrolled on it."
	s := newSession(t, cfg)

	s.contains("Key Not Recognised", "not enrolled")
	// It must offer the password path and the different-name path.
	s.contains("PreferredAuthentications=password")
	s.contains("ssh new@")
	// And must NOT look like a registration screen.
	s.notContains("New User Registration")
}

// ---------------------------------------------------------------------------
// Terminal safety (§5.4)
// ---------------------------------------------------------------------------

// Post content comes from other users, and on a federated BBS from other
// people's BBSes. An escape sequence in a subject must not reach the terminal.
func TestUntrustedTextIsSanitisedBeforeDisplay(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")

	evil := "clear\x1b[2Jscreen"
	if _, err := f.svc.Post(f.ctx, "austin", "general", evil, "body\x1b]0;retitled\x07here"); err != nil {
		t.Fatal(err)
	}

	s := f.login(t, "austin")
	s.typeRunes("m").enter()

	// View() output still contains styling escapes, so check the raw frame for
	// the specific injected sequences rather than for any ESC at all.
	raw := s.model.View()
	if strings.Contains(raw, "\x1b[2J") {
		t.Fatal("a screen-clear sequence from a post reached the terminal")
	}
	if strings.Contains(raw, "\x1b]0;") {
		t.Fatal("a window-title sequence from a post reached the terminal")
	}
	// The ESC byte is removed, which is what makes the sequence inert. The
	// remaining literal characters are left visible on purpose: silently
	// deleting parts of someone's message would be a worse default than
	// showing that something was stripped.
	s.contains("clear", "screen")
}

// ---------------------------------------------------------------------------
// Node identity (§6.1.4.2)
// ---------------------------------------------------------------------------

func TestNodeInfoShowsBothRenderings(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	s := f.login(t, "austin")

	s.typeRunes("n")
	v := s.view()

	id := f.svc.NodeID()
	if !strings.Contains(v, id.String()) {
		t.Errorf("base32 rendering missing:\n%s", v)
	}
	if !strings.Contains(v, id.Words()) {
		t.Errorf("word rendering missing:\n%s", v)
	}
	// The screen should explain which form is for which channel.
	s.contains("aloud", "config")
}

// ---------------------------------------------------------------------------
// Golden frames (§12.8)
// ---------------------------------------------------------------------------

// Layout regressions are invisible to ordinary assertions: a mangled border or
// a column that stopped aligning still passes a Contains check. These compare
// the whole frame.
func TestGoldenFrames(t *testing.T) {
	t.Run("menu", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "austin", "pw")
		f.login(t, "austin").golden("menu")
	})

	t.Run("area_list", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "austin", "pw")
		f.login(t, "austin").typeRunes("m").golden("area_list")
	})

	t.Run("signup_nick", func(t *testing.T) {
		f := newFixture(t)
		newSession(t, f.config(IntentSignup, "")).golden("signup_nick")
	})

	t.Run("signup_warning", func(t *testing.T) {
		f := newFixture(t)
		cfg := f.config(IntentSignup, "")
		cfg.PublicKey = "ssh-ed25519 AAAA test"
		cfg.KeyFP = "SHA256:x"
		s := newSession(t, cfg)
		s.line("newbie").enter().line("phrase123").line("phrase123")
		s.golden("signup_warning")
	})

	t.Run("guest_menu", func(t *testing.T) {
		f := newFixture(t)
		newSession(t, f.config(IntentGuest, "guest")).golden("guest_menu")
	})

	t.Run("key_unknown", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "austin", "pw")
		cfg := f.config(IntentKeyUnknown, "austin")
		cfg.AuthNote = "an account with that name exists, but this key is not enrolled on it."
		newSession(t, cfg).golden("key_unknown")
	})

	t.Run("unlock", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "austin", "pw")
		f.login(t, "austin").typeRunes("e").golden("unlock")
	})
}

// A narrow terminal must still produce a sane frame — 80x24 is the floor a BBS
// promises, but people connect from phones.
func TestNarrowTerminalDoesNotOverflow(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")

	cfg := f.config(IntentAuthenticated, "austin")
	cfg.Width, cfg.Height = 40, 10
	u, _ := f.store.GetUser(f.ctx, "austin")
	cfg.User = u
	s := newSession(t, cfg)

	for _, line := range strings.Split(s.view(), "\n") {
		if len([]rune(line)) > 40 {
			t.Errorf("line exceeds a 40-column terminal (%d runes): %q", len([]rune(line)), line)
		}
	}
}

// A client that cannot report its geometry sends 0x0. Adopting that collapses
// every screen; the size negotiated at connection time is the better guess.
func TestBogusWindowSizeIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	s := f.login(t, "austin")

	before := s.model.width
	if before != 80 {
		t.Fatalf("fixture width is %d, expected 80", before)
	}

	s.dispatch(tea.WindowSizeMsg{Width: 0, Height: 0})
	if s.model.width != before {
		t.Fatalf("a 0x0 size was adopted: width is now %d", s.model.width)
	}

	// A real resize still applies.
	s.dispatch(tea.WindowSizeMsg{Width: 132, Height: 50})
	if s.model.width != 132 {
		t.Fatalf("a legitimate resize was ignored: width is %d", s.model.width)
	}
}
