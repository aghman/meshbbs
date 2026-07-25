package tui

import (
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// sysopSession logs in an account flagged as sysop.
func sysopSession(t *testing.T, f *fixture, nick string) *session {
	t.Helper()
	if _, err := f.store.CreateUser(f.ctx, store.CreateUserOptions{
		Nick: nick, CanLogin: true, IsSysop: true,
		Capabilities: append(append([]string(nil), store.DefaultCapabilities...),
			store.CapPostFederated),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.EnsureDMKey(f.ctx, nick, "pw"); err != nil {
		t.Fatal(err)
	}
	u, err := f.store.GetUser(f.ctx, nick)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, nick)
	cfg.User = u
	return newSession(t, cfg)
}

func TestSysopPanelIsSysopOnly(t *testing.T) {
	f := newFixture(t)
	f.user(t, "regular", "pw")

	s := f.login(t, "regular")
	s.notContains("Sysop panel") // not offered in the menu
	s.typeRunes("s")
	s.contains("sysop function")
	s.notContains("Users  Areas") // and the panel did not open
}

func TestSysopPanelOpens(t *testing.T) {
	f := newFixture(t)
	s := sysopSession(t, f, "boss")

	s.contains("Sysop panel")
	s.typeRunes("s")
	s.contains("Users", "Areas", "Aliases", "Status")
	s.contains("boss")
}

// The [N7] lever: granting federated posting is the single most consequential
// thing a sysop does here, so it confirms and explains the cost.
func TestSysopGrantsFederatedPostingWithConfirmation(t *testing.T) {
	f := newFixture(t)
	f.user(t, "newbie", "pw")
	s := sysopSession(t, f, "boss")

	s.typeRunes("s")
	// Move to newbie (list is sorted: admin, newbie).
	for i := 0; i < 4; i++ {
		if strings.Contains(s.view(), "> newbie") {
			break
		}
		s.press(tea.KeyDown)
	}
	s.contains("> newbie")

	s.typeRunes("g")
	// The confirmation must state what it costs, not just ask.
	s.contains("shared airtime", "[y/N]")

	s.typeRunes("y")
	s.contains("can now post to federated areas")

	has, err := f.store.HasCapability(f.ctx, "newbie", store.CapPostFederated)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("the grant did not take effect")
	}
}

func TestSysopConfirmationCanBeDeclined(t *testing.T) {
	f := newFixture(t)
	f.user(t, "newbie", "pw")
	s := sysopSession(t, f, "boss")

	s.typeRunes("s")
	for i := 0; i < 4; i++ {
		if strings.Contains(s.view(), "> newbie") {
			break
		}
		s.press(tea.KeyDown)
	}
	s.typeRunes("g").typeRunes("n")
	s.contains("Cancelled")

	has, _ := f.store.HasCapability(f.ctx, "newbie", store.CapPostFederated)
	if has {
		t.Fatal("declining the confirmation still granted the capability")
	}
}

// Federating an area starts spending mesh airtime, so it confirms too.
func TestSysopTogglesAreaFederation(t *testing.T) {
	f := newFixture(t)
	s := sysopSession(t, f, "boss")

	s.typeRunes("s").press(tea.KeyTab) // Areas tab
	s.contains("general", "Local to this BBS")

	s.typeRunes("f")
	s.contains("consume airtime", "[y/N]")
	s.typeRunes("y")
	s.contains("replicates over the mesh")

	area, err := f.store.GetArea(f.ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	if !area.Federated {
		t.Fatal("the area was not federated")
	}
}

func TestSysopStatusShowsNodeIdentityAndSessions(t *testing.T) {
	f := newFixture(t)
	s := sysopSession(t, f, "boss")

	s.typeRunes("s")
	for i := 0; i < 3; i++ {
		s.press(tea.KeyTab)
	}
	s.contains("Node", "Sessions", "Counts")

	id := f.svc.NodeID()
	s.contains(id.String(), id.Words())
	// Be honest that federation is not built yet rather than showing a fake gauge.
	s.contains("not built yet")
}

func TestSysopAliasTabExplainsLocality(t *testing.T) {
	f := newFixture(t)
	s := sysopSession(t, f, "boss")

	s.typeRunes("s").press(tea.KeyTab).press(tea.KeyTab)
	// The alias tab must explain that names are local, since that is the
	// property that surprises people (§6.1.4).
	s.contains("never travel on the wire")
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

func TestChatDeliversBetweenSessions(t *testing.T) {
	f := newFixture(t)
	room := NewChatRoom(50)
	f.user(t, "alice", "pw")
	f.user(t, "bob", "pw")

	mk := func(nick string) *session {
		u, err := f.store.GetUser(f.ctx, nick)
		if err != nil {
			t.Fatal(err)
		}
		cfg := f.config(IntentAuthenticated, nick)
		cfg.User = u
		cfg.Chat = room
		cfg.SessionID = "sess-" + nick
		return newSession(t, cfg)
	}

	alice := mk("alice")
	bob := mk("bob")

	alice.typeRunes("c")
	bob.typeRunes("c")

	alice.typeRunes("anyone on 2m tonight?").enter()

	// Bob's view refreshes from the shared room.
	bob.dispatch(chatUpdatedMsg{lines: room.Lines()})
	bob.contains("alice: anyone on 2m tonight?")

	// Joins are announced.
	bob.contains("alice joined")
}

func TestChatIsBounded(t *testing.T) {
	room := NewChatRoom(5)
	for i := 0; i < 20; i++ {
		room.Say(ChatLine{Nick: "spam", Text: "line"})
	}
	if got := len(room.Lines()); got != 5 {
		t.Fatalf("room holds %d lines, want the 5 it was sized for", got)
	}
}

func TestGuestCanReadChatButNotSpeak(t *testing.T) {
	f := newFixture(t)
	room := NewChatRoom(50)
	room.Say(ChatLine{Nick: "alice", Text: "hello there"})

	cfg := f.config(IntentGuest, "guest")
	cfg.Chat = room
	s := newSession(t, cfg)

	s.typeRunes("c")
	s.contains("hello there", "read but not speak", "register to join in")
}

// Chat text comes from other users and goes straight to a terminal.
func TestChatSanitisesInput(t *testing.T) {
	f := newFixture(t)
	room := NewChatRoom(50)
	room.Say(ChatLine{Nick: "evil", Text: "before\x1b[2Jafter"})

	cfg := f.config(IntentGuest, "guest")
	cfg.Chat = room
	s := newSession(t, cfg)
	s.typeRunes("c")

	if strings.Contains(s.model.View(), "\x1b[2J") {
		t.Fatal("an escape sequence from chat reached the terminal")
	}
}

func TestLeavingChatAnnouncesDeparture(t *testing.T) {
	f := newFixture(t)
	room := NewChatRoom(50)
	f.user(t, "alice", "pw")

	u, _ := f.store.GetUser(f.ctx, "alice")
	cfg := f.config(IntentAuthenticated, "alice")
	cfg.User = u
	cfg.Chat = room
	s := newSession(t, cfg)

	s.typeRunes("c").escape()

	var sawLeave bool
	for _, l := range room.Lines() {
		if l.System && strings.Contains(l.Text, "left") {
			sawLeave = true
		}
	}
	if !sawLeave {
		t.Fatal("leaving chat did not announce a departure")
	}
}

func TestGoldenSysopFrames(t *testing.T) {
	t.Run("sysop_users", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "newbie", "pw")
		sysopSession(t, f, "boss").typeRunes("s").golden("sysop_users")
	})
	t.Run("sysop_areas", func(t *testing.T) {
		f := newFixture(t)
		sysopSession(t, f, "boss").typeRunes("s").press(tea.KeyTab).golden("sysop_areas")
	})
	t.Run("chat_empty", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "alice", "pw")
		u, _ := f.store.GetUser(f.ctx, "alice")
		cfg := f.config(IntentAuthenticated, "alice")
		cfg.User = u
		cfg.Chat = NewChatRoom(50)
		newSession(t, cfg).typeRunes("c").golden("chat")
	})
}
