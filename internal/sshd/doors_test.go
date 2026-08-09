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

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/door"
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
	srv, err := NewServer(svc, st, Options{
		Bind: "127.0.0.1", Port: 0, KeysDir: t.TempDir(),
		Themes: themes, DefaultTheme: theme.DefaultName,
		Doors:    mgr,
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
	// session — which is a real behaviour, tested elsewhere, and not this one.
	authorized := string(gossh.MarshalAuthorizedKey(signer.PublicKey()))
	if err := st.AddUserKey(ctx, "austin", strings.TrimSpace(authorized),
		gossh.FingerprintSHA256(signer.PublicKey()), "test"); err != nil {
		t.Fatal(err)
	}
	client, err := gossh.Dial("tcp", ln.Addr().String(), &gossh.ClientConfig{
		User:            "austin",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Close()

	screen := &syncBuffer{}
	sess.Stdout = screen
	sess.Stderr = io.Discard
	if err := sess.RequestPty("xterm-256color", 24, 80, gossh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

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
