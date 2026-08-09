package sshd

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/door/example"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/theme"
	gossh "golang.org/x/crypto/ssh"
)

// A door, over a real SSH connection, all the way through.
//
// Every layer below this has its own tests, and none of them can catch the
// thing that actually breaks: a handoff that works in isolation and not when
// Bubble Tea is holding the other end. So this drives the whole stack — client,
// PTY, mux, runner, pseudo-terminal, and back — and the door is this test
// binary re-executed, for the same reason the runner's own tests use it.

const doorHelperEnv = "MESHBBS_SSHD_DOOR_HELPER"

func TestMain(m *testing.M) {
	// The reference doors, so that this binary can be pointed at as a door in
	// exactly the way `meshbbs door examples` points at the real one. Same
	// argv shape, same entry point, same package.
	if len(os.Args) > 2 && os.Args[1] == "door-example" {
		if err := example.Run(os.Args[2], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if os.Getenv(doorHelperEnv) != "" {
		// The door: greet, echo one line back in capitals, leave.
		fmt.Print("DOOR-READY\r\n")
		buf := make([]byte, 256)
		n, _ := os.Stdin.Read(buf)
		fmt.Printf("DOOR-SAW[%s]\r\n", strings.ToUpper(strings.TrimSpace(string(buf[:n]))))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestADoorRunsOverARealSSHSession(t *testing.T) {
	svc, st, themes := telnetFixture(t)
	ctx := context.Background()

	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: "austin", CanLogin: true, Capabilities: store.DefaultCapabilities,
	}); err != nil {
		t.Fatal(err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutDoor(ctx, store.Door{
		Name: "echodoor", Path: self, Cwd: t.TempDir(),
		EnvPassthrough: []string{doorHelperEnv},
		DropfileType:   store.DropfileNone,
		WallClock:      30 * time.Second,
		APILevel:       store.APISession,
		Enabled:        true,
	}, "sysop"); err != nil {
		t.Fatal(err)
	}
	// env_passthrough only forwards what the SERVER has, which is how a door
	// gets anything from the environment at all (§11.5).
	t.Setenv(doorHelperEnv, "1")

	mgr := door.New(clock.NewReal(), discardLog())
	screen, keys := openDoorSession(t, svc, st, themes, mgr, "austin")

	awaitScreen(t, screen, "MeshBBS", 15*time.Second)

	// Into the door list, and play.
	if _, err := keys.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	awaitScreen(t, screen, "echodoor", 15*time.Second)
	if _, err := keys.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}

	// The door's own output reaches the client: the mux has lent it the
	// connection and the runner is bridging a pseudo-terminal to it.
	awaitScreen(t, screen, "DOOR-READY", 20*time.Second)

	// And the user's keystrokes reach the door rather than the menu behind it.
	if _, err := keys.Write([]byte("hello\r\n")); err != nil {
		t.Fatal(err)
	}
	awaitScreen(t, screen, "DOOR-SAW[HELLO]", 20*time.Second)

	// The door exits and the BBS repaints, which is the mux's onResume doing
	// its job. Asserted on the RAW bytes after the door's last output, and not
	// by looking for the menu again: the door list was on screen before the
	// door started, so anything searching the whole transcript for it would
	// pass on history alone. What has to be true is that the terminal is
	// cleared AFTER the door drew — without that the user gets the menu back as
	// a few changed lines over a game's final board.
	awaitRawAfter(t, screen, "DOOR-SAW[HELLO]", "\x1b[2J", 20*time.Second)
	awaitScreen(t, screen, "echodoor ended", 20*time.Second)

	// Exclusivity — that the TUI does not also receive what the user typed at
	// the door — is not asserted here. Which of two concurrent readers wins a
	// chunk is a race, so an end-to-end test could only catch it sometimes.
	// TestMuxBorrowerHasInputExclusively pins it deterministically instead, by
	// sending two hundred chunks that all have to land on the borrower.
}

// discardLog keeps test output about the test rather than about the door.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// awaitRawAfter waits for want to appear in the raw session output at some
// point after marker. Raw, because what it is usually looking for is an escape
// sequence, and awaitScreen strips those out by design.
func awaitRawAfter(t *testing.T, out *syncBuffer, marker, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		raw := out.String()
		if at := strings.Index(raw, marker); at >= 0 {
			if strings.Contains(raw[at+len(marker):], want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never appeared after %q in the session output", want, marker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The reference doors, played over a real SSH session.
//
// §9.1 ships them "so the API has proof-of-life", and proof-of-life means this:
// the guess door reads its session (level 1), saves a score and reads back a
// shared record (level 2), and announces a new one (level 3) — through the real
// socket, from a real pseudo-terminal, launched the way any third-party door
// would be. The unit tests around the API prove the server; this proves the
// contract a door author is being asked to write against.
func TestTheReferenceDoorPlaysOverSSH(t *testing.T) {
	svc, st, themes := telnetFixture(t)
	ctx := context.Background()

	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: "austin", CanLogin: true, Capabilities: store.DefaultCapabilities,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArea(ctx, "games", "game chatter", false); err != nil {
		t.Fatal(err)
	}

	// The door is this test binary, invoked exactly as `door examples` invokes
	// the real one: same argv shape, same entry point.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutDoor(ctx, store.Door{
		Name: "guess", Path: self, Args: []string{"door-example", "guess"},
		Cwd: t.TempDir(), DropfileType: store.DropfileNone,
		WallClock: 2 * time.Minute, APILevel: store.APIAnnounce,
		AnnounceArea: "games", AnnouncePerHour: 4, StateQuota: 4096,
		Enabled: true,
	}, "sysop"); err != nil {
		t.Fatal(err)
	}

	mgr := door.New(clock.NewReal(), discardLog())
	mgr.SetHost(svc.Doors())

	screen, keys := openDoorSession(t, svc, st, themes, mgr, "austin")

	awaitScreen(t, screen, "MeshBBS", 15*time.Second)
	if _, err := keys.Write([]byte("d")); err != nil {
		t.Fatal(err)
	}
	awaitScreen(t, screen, "guess", 15*time.Second)
	if _, err := keys.Write([]byte("\r")); err != nil {
		t.Fatal(err)
	}

	// Level 1: it greeted the player by name, which it could only learn from
	// the session it asked for over the socket.
	awaitScreen(t, screen, "Good luck, austin", 20*time.Second)

	// Play it out. Binary search over 1..100 finds any number in at most seven.
	lo, hi := 1, 100
	for range 8 {
		guess := (lo + hi) / 2
		// The offset is taken BEFORE the guess is sent. Taking it after would
		// race a reply that arrived in between, and the loop would then read
		// the previous hint as the answer to this guess and search the wrong
		// half.
		since := plainLen(screen)
		if _, err := keys.Write([]byte(fmt.Sprintf("%d\r\n", guess))); err != nil {
			t.Fatal(err)
		}
		hint := awaitOneOf(t, screen, since,
			[]string{"Higher.", "Lower.", "Got it in"}, 20*time.Second)
		switch hint {
		case "Higher.":
			lo = guess + 1
		case "Lower.":
			hi = guess - 1
		default:
			goto won
		}
	}
	t.Fatalf("the door never accepted a winning guess:\n%s", screen.String())

won:
	// Level 2: the score was saved, and level 3: the board was told. Both are
	// asserted against the DATABASE rather than the screen, because what the
	// door printed is what the door believed.
	awaitScreen(t, screen, "new board record", 20*time.Second)

	v, ok, err := st.DoorStateGet(ctx, "guess", store.ScopeUser, "austin", "best")
	if err != nil || !ok || v == "" {
		t.Errorf("the door saved no personal best: %q ok=%v err=%v", v, ok, err)
	}
	shared, ok, err := st.DoorStateGet(ctx, "guess", store.ScopeGlobal, "", "record")
	if err != nil || !ok || !strings.Contains(shared, "austin") {
		t.Errorf("the shared record is %q ok=%v err=%v", shared, ok, err)
	}

	posts, err := st.ListPosts(ctx, "games", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("the door made %d announcements, want 1", len(posts))
	}
	// As the DOOR, never as the user (§9.1.1).
	if posts[0].Author != store.DoorAuthor("guess") {
		t.Errorf("the announcement is by %q, want %q",
			posts[0].Author, store.DoorAuthor("guess"))
	}
}

// plainLen is how much the session has shown, with escape sequences removed.
func plainLen(out *syncBuffer) int {
	return len(ansiEscape.ReplaceAllString(out.String(), ""))
}

// awaitOneOf waits for one of several strings to appear after an offset, and
// says which. The offset is what keeps one guess's hint from answering the next.
func awaitOneOf(t *testing.T, out *syncBuffer, start int, want []string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		plain := ansiEscape.ReplaceAllString(out.String(), "")
		if len(plain) > start {
			fresh := plain[start:]
			for _, w := range want {
				if strings.Contains(fresh, w) {
					return w
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("none of %v appeared; the door showed:\n%s", want, plain)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openDoorSession brings up a server with doors and connects a real SSH client
// to it, returning the client's screen and its keyboard.
func openDoorSession(t *testing.T, svc *bbs.Service, st *store.Store,
	themes *theme.Set, mgr *door.Manager, nick string) (*syncBuffer, io.WriteCloser) {
	t.Helper()
	ctx := context.Background()

	srv, err := NewServer(svc, st, Options{
		Bind: "127.0.0.1", Port: 0, KeysDir: t.TempDir(),
		Themes: themes, DefaultTheme: theme.DefaultName,
		Doors:    mgr,
		BBSName:  "Fog City",
		Clock:    clock.NewVirtual(time.Unix(1_700_000_000, 0)),
		Location: time.UTC,
		Logger:   discardLog(),
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
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	})

	_, priv, err := ed25519.GenerateKey(rng.TestSecret(21).Reader())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	// Enrol the key, or auth lands on the key-unknown screen instead of a
	// session — a real behaviour, tested elsewhere, and not the one under test.
	authorized := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(signer.PublicKey())))
	if err := st.AddUserKey(ctx, nick, authorized,
		gossh.FingerprintSHA256(signer.PublicKey()), "test"); err != nil {
		t.Fatal(err)
	}

	client, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{
		User:            nick,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close() })

	screen := &syncBuffer{}
	sess.Stdout = screen
	sess.Stderr = io.Discard
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}
	return screen, keys
}
