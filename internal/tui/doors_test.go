package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/store"
)

// fakeLauncher stands in for a front end.
type fakeLauncher struct {
	mu sync.Mutex

	canRun bool
	why    string

	launched []string
	sessions []DoorSession
	status   string
	err      error
	// block, when non-nil, holds Launch until it is closed — a door that is
	// still running.
	block chan struct{}
}

func (l *fakeLauncher) CanRun() (bool, string) { return l.canRun, l.why }

func (l *fakeLauncher) Launch(_ context.Context, d store.Door, sess DoorSession) (string, error) {
	if l.block != nil {
		<-l.block
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launched = append(l.launched, d.Name)
	l.sessions = append(l.sessions, sess)
	if l.err != nil {
		return "", l.err
	}
	return l.status, nil
}

func (l *fakeLauncher) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.launched...)
}

func installDoor(t *testing.T, f *fixture, d store.Door) {
	t.Helper()
	if d.Path == "" {
		d.Path = "/opt/doors/" + d.Name
	}
	if d.Cwd == "" {
		d.Cwd = "/opt/doors"
	}
	if d.WallClock == 0 {
		d.WallClock = time.Hour
	}
	if d.DropfileType == "" {
		d.DropfileType = store.DropfileNone
	}
	if d.APILevel == 0 {
		d.APILevel = store.APIAnnounce
	}
	if err := f.store.PutDoor(f.ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}
}

// doorSession opens a session with a launcher attached.
func doorSession(t *testing.T, f *fixture, nick string, l DoorLauncher) *session {
	t.Helper()
	u, err := f.store.GetUser(f.ctx, nick)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, nick)
	cfg.User = u
	cfg.Doors = l
	return newSession(t, cfg)
}

func TestDoorListShowsInstalledDoors(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	installDoor(t, f, store.Door{Name: "tradewars", Enabled: true, MaxConcurrent: 4})
	installDoor(t, f, store.Door{Name: "lord", Enabled: true, NodeLock: true})
	installDoor(t, f, store.Door{Name: "retired", Enabled: false})

	s := doorSession(t, f, "austin", &fakeLauncher{canRun: true})
	s.typeRunes("d")

	if s.model.screen != screenDoorList {
		t.Fatalf("[D] went to screen %d, not the door list", s.model.screen)
	}
	frame := s.model.View()
	for _, want := range []string{"tradewars", "lord"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the door list does not show %q:\n%s", want, frame)
		}
	}
	// A door the sysop turned off is not on offer.
	if strings.Contains(frame, "retired") {
		t.Errorf("a disabled door is on the list:\n%s", frame)
	}
	// The notes column earns its place: these are things a caller wants to
	// know before choosing.
	if !strings.Contains(frame, "one player per node") {
		t.Errorf("the list does not mention the node lock:\n%s", frame)
	}
}

// A browser cannot run doors, and says so on the list — before someone picks
// one, not only after.
func TestDoorListExplainsWhenItCannotRun(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	installDoor(t, f, store.Door{Name: "tradewars", Enabled: true})

	l := &fakeLauncher{canRun: false, why: "Door games need a terminal."}
	s := doorSession(t, f, "austin", l)
	s.typeRunes("d")

	if !strings.Contains(s.model.View(), "need a terminal") {
		t.Errorf("the list does not explain why it cannot run doors:\n%s", s.model.View())
	}

	// And picking one answers with the way to do it rather than silence.
	l.status = "tradewars needs a terminal. Connect with: ssh austin@bbs"
	s.enter()
	if !strings.Contains(s.model.status, "ssh austin@bbs") {
		t.Errorf("status after choosing a door is %q", s.model.status)
	}
}

// The list is still shown when there is nothing on it: a caller should be able
// to tell "no doors here" from "this BBS has no doors menu".
func TestDoorListSaysWhenThereAreNone(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	s := doorSession(t, f, "austin", &fakeLauncher{canRun: true})
	s.typeRunes("d")

	if !strings.Contains(s.model.View(), "No doors are installed") {
		t.Errorf("an empty door list says nothing:\n%s", s.model.View())
	}
	// Enter on an empty list does nothing rather than panicking on index zero.
	s.enter()
	if s.model.screen != screenDoorList {
		t.Error("enter on an empty list left the screen")
	}
}

func TestLaunchingADoorPassesTheSession(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	installDoor(t, f, store.Door{Name: "tradewars", Enabled: true})

	l := &fakeLauncher{canRun: true, status: "tradewars ended."}
	s := doorSession(t, f, "austin", l)
	s.typeRunes("d")
	s.enter()

	if got := l.seen(); len(got) != 1 || got[0] != "tradewars" {
		t.Fatalf("launched %v", got)
	}
	sess := l.sessions[0]
	if sess.Nick != "austin" {
		t.Errorf("the launcher was told the nick is %q", sess.Nick)
	}
	if sess.Width != 80 || sess.Height != 24 {
		t.Errorf("the launcher got a %dx%d terminal", sess.Width, sess.Height)
	}
	if sess.TimeRemaining == nil {
		t.Error("the launcher was given no way to read the time remaining")
	}
	if s.model.status != "tradewars ended." {
		t.Errorf("status after the door is %q", s.model.status)
	}
	// Back on the list, not left on a screen the door drew over.
	if s.model.screen != screenDoorList {
		t.Errorf("after the door the session is on screen %d", s.model.screen)
	}
}

// §6.7's gates are enforced at the point of use with the capability named, so
// somebody can go and ask for it. Guests get told how to get an account.
func TestRunningADoorIsGated(t *testing.T) {
	f := newFixture(t)

	// A user with the default capabilities has run_doors; one that needs more
	// is refused by name.
	f.user(t, "austin", "")
	installDoor(t, f, store.Door{
		Name: "tradewars", Enabled: true,
		RequiredCapability: store.CapPostFederated,
	})

	l := &fakeLauncher{canRun: true, status: "ok"}
	s := doorSession(t, f, "austin", l)
	s.typeRunes("d")
	s.enter()

	if got := l.seen(); len(got) != 0 {
		t.Fatalf("a door ran without the capability it requires: %v", got)
	}
	if !strings.Contains(s.model.status, store.CapPostFederated) {
		t.Errorf("the refusal does not name the capability: %q", s.model.status)
	}
	if !s.model.statusErr {
		t.Error("the refusal is not marked as one")
	}

	// Granted, it runs.
	if err := f.store.GrantCapability(f.ctx, "austin", store.CapPostFederated, "sysop"); err != nil {
		t.Fatal(err)
	}
	s.enter()
	if got := l.seen(); len(got) != 1 {
		t.Errorf("the door did not run after the grant: %v", got)
	}
}

func TestGuestsCannotRunDoors(t *testing.T) {
	f := newFixture(t)
	installDoor(t, f, store.Door{Name: "tradewars", Enabled: true})

	cfg := f.config(IntentGuest, "guest")
	cfg.Doors = &fakeLauncher{canRun: true, status: "ok"}
	s := newSession(t, cfg)
	s.typeRunes("d")
	s.enter()

	if !strings.Contains(strings.ToLower(s.model.status), "guest") {
		t.Errorf("a guest was refused with %q", s.model.status)
	}
	// And is told how to stop being one, rather than just being told no.
	if !strings.Contains(s.model.status, "ssh new@") {
		t.Errorf("the refusal does not say how to register: %q", s.model.status)
	}
}

// A connection with no launcher at all — a front end that has not been taught
// about doors — refuses rather than silently doing nothing.
func TestADoorlessConnectionSaysSo(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")
	installDoor(t, f, store.Door{Name: "tradewars", Enabled: true})

	s := doorSession(t, f, "austin", nil)
	s.typeRunes("d")
	s.enter()

	if !s.model.statusErr || s.model.status == "" {
		t.Errorf("a doorless connection answered %q (err=%v)", s.model.status, s.model.statusErr)
	}
}
