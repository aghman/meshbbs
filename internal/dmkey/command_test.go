package dmkey

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/keyring"
	"github.com/aghman/meshbbs/internal/store"
)

func testEnv(t *testing.T, stdin string, passphrases ...string) (*Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errBuf bytes.Buffer
	i := 0
	return &Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errBuf,
		KeyPath: filepath.Join(t.TempDir(), "dm.key"),
		Passphrase: func(string) (string, error) {
			if i >= len(passphrases) {
				return "", errors.New("asked for more passphrases than the test supplied")
			}
			p := passphrases[i]
			i++
			return p, nil
		},
	}, &out, &errBuf
}

// The whole point of tier 3, end to end: the SERVER seals with only the public
// key, and the helper opens it with a private key the server never held.
func TestTheHelperOpensWhatTheServerSealed(t *testing.T) {
	env, out, _ := testEnv(t, "", "correct horse", "correct horse")
	if err := Init(env); err != nil {
		t.Fatalf("init: %v", err)
	}

	// The public key as the user would copy it off their screen.
	var pub keyring.PublicKey
	for _, f := range strings.Fields(out.String()) {
		if p, err := keyring.ParsePublicKey(f); err == nil {
			pub = p
			break
		}
	}
	if pub == (keyring.PublicKey{}) {
		t.Fatalf("init printed no usable public key:\n%s", out.String())
	}

	// The server's side. It has the public key and nothing else — no wrapped
	// key, no passphrase — which is exactly a tier-3 user's row.
	payload, err := store.MarshalSealedPayload(store.SealedPayload{
		Subject: "league night", Text: "you were robbed",
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keyring.Seal(pub, payload)
	if err != nil {
		t.Fatal(err)
	}

	// What the user pastes, with a terminal's worth of noise around it.
	pasted := "meshbbs:mail> read 1\n" + Armour(sealed) + "\n[q] back\n"

	read, readOut, _ := testEnv(t, pasted, "correct horse")
	read.KeyPath = env.KeyPath
	if err := Open(read); err != nil {
		t.Fatalf("open: %v", err)
	}
	got := readOut.String()
	if !strings.Contains(got, "you were robbed") {
		t.Errorf("the message body did not come back:\n%s", got)
	}
	if !strings.Contains(got, "league night") {
		t.Errorf("the subject did not come back — it is sealed WITH the body (§8.2):\n%s", got)
	}
}

// Overwriting a DM key destroys every message ever sent to it, with no error at
// the time: the mail is still there and is now ciphertext nobody can open.
func TestInitRefusesToClobberAnExistingKey(t *testing.T) {
	env, _, _ := testEnv(t, "", "first", "first")
	if err := Init(env); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(env.KeyPath)
	if err != nil {
		t.Fatal(err)
	}

	// A second init must not even ASK for a passphrase — prompting and then
	// refusing invites the user to type it somewhere it will not be needed.
	again := &Env{
		Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		KeyPath: env.KeyPath,
		Passphrase: func(string) (string, error) {
			t.Error("init asked for a passphrase before noticing the key file existed")
			return "second", nil
		},
	}
	if err := Init(again); !errors.Is(err, ErrKeyFileExists) {
		t.Fatalf("got %v, want ErrKeyFileExists", err)
	}

	after, err := os.ReadFile(env.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the key file changed; every message ever sent to it is now unreadable")
	}
}

func TestInitRefusesMismatchedOrEmptyPassphrases(t *testing.T) {
	t.Run("mismatched", func(t *testing.T) {
		env, _, _ := testEnv(t, "", "one", "two")
		if err := Init(env); err == nil {
			t.Fatal("accepted two different passphrases")
		}
		if _, err := os.Stat(env.KeyPath); !errors.Is(err, os.ErrNotExist) {
			t.Error("a key file was written despite the refusal")
		}
	})
	t.Run("empty", func(t *testing.T) {
		env, _, _ := testEnv(t, "", "   ", "   ")
		if err := Init(env); err == nil {
			t.Fatal("accepted an empty passphrase")
		}
	})
}

// A wrong passphrase must be indistinguishable from a corrupt file, so that
// nobody can probe one to learn about the other.
func TestOpenWithTheWrongPassphrase(t *testing.T) {
	env, _, _ := testEnv(t, "", "right", "right")
	if err := Init(env); err != nil {
		t.Fatal(err)
	}
	read, _, _ := testEnv(t, Armour(bytes.Repeat([]byte{1}, 64)), "wrong")
	read.KeyPath = env.KeyPath
	err := Open(read)
	if !errors.Is(err, keyring.ErrWrongPassphrase) {
		t.Errorf("got %v, want ErrWrongPassphrase", err)
	}
}

// The ciphertext is read before the passphrase is asked for. Otherwise a user
// pasting into a pipe that is silently waiting on a hidden prompt sees nothing
// happen — and the most likely next thing they do is type the passphrase into
// the paste.
func TestOpenReadsTheMessageBeforeAskingForAPassphrase(t *testing.T) {
	env, _, _ := testEnv(t, "", "pass", "pass")
	if err := Init(env); err != nil {
		t.Fatal(err)
	}

	asked := false
	read := &Env{
		Stdin:  strings.NewReader("this is not a DM block at all"),
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		KeyPath: env.KeyPath,
		Passphrase: func(string) (string, error) {
			asked = true
			return "pass", nil
		},
	}
	if err := Open(read); !errors.Is(err, ErrNoArmour) {
		t.Fatalf("got %v, want ErrNoArmour", err)
	}
	if asked {
		t.Error("the passphrase was requested before the message was found to be unreadable")
	}
}

// A message sealed to somebody else must fail with an explanation, not a
// cryptic authentication error.
func TestOpenAMessageForAnotherKey(t *testing.T) {
	env, _, _ := testEnv(t, "", "mine", "mine")
	if err := Init(env); err != nil {
		t.Fatal(err)
	}
	_, otherPub, err := keyring.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := keyring.Seal(otherPub, []byte("not for you"))
	if err != nil {
		t.Fatal(err)
	}

	read, _, _ := testEnv(t, Armour(sealed), "mine")
	read.KeyPath = env.KeyPath
	err = Open(read)
	if err == nil {
		t.Fatal("opened a message sealed to another key")
	}
	if !strings.Contains(err.Error(), "not readable with this key") {
		t.Errorf("the error does not explain what happened: %v", err)
	}
}

// Pubkey derives from the private key rather than storing it beside it, so it
// needs the passphrase — and must print the same key init did.
func TestPubkeyMatchesWhatInitPrinted(t *testing.T) {
	env, out, _ := testEnv(t, "", "pw", "pw")
	if err := Init(env); err != nil {
		t.Fatal(err)
	}
	var printed string
	for _, f := range strings.Fields(out.String()) {
		if _, err := keyring.ParsePublicKey(f); err == nil {
			printed = f
			break
		}
	}

	again, againOut, _ := testEnv(t, "", "pw")
	again.KeyPath = env.KeyPath
	if err := Pubkey(again); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(againOut.String()); got != printed {
		t.Errorf("pubkey printed %q, init printed %q", got, printed)
	}
}

func TestLoadWithoutAKeyFile(t *testing.T) {
	env, _, _ := testEnv(t, Armour([]byte{1, 2, 3}), "pw")
	if err := Open(env); !errors.Is(err, ErrNoKeyFile) {
		t.Errorf("got %v, want ErrNoKeyFile", err)
	}
}

// The key file is the user's only copy. It must not be group- or
// world-readable, and a permissive one is warned about rather than refused —
// the file is ciphertext, and refusing to run would punish a permission bit
// while their mail sits unread.
func TestKeyFilePermissions(t *testing.T) {
	env, _, _ := testEnv(t, "", "pw", "pw")
	if err := Init(env); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(env.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeIsWindows() {
		return
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("key file is mode %04o, want no access for group or other", mode)
	}
	if w := PermissionWarning(env.KeyPath); w != "" {
		t.Errorf("a freshly written key file warned: %s", w)
	}

	if err := os.Chmod(env.KeyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	w := PermissionWarning(env.KeyPath)
	if w == "" {
		t.Fatal("a world-readable key file did not warn")
	}
	if !strings.Contains(w, "chmod 600") {
		t.Errorf("the warning does not say how to fix it: %s", w)
	}

	// Still usable, because it is ciphertext.
	read, _, readErr := testEnv(t, "", "pw")
	read.KeyPath = env.KeyPath
	if err := Pubkey(read); err != nil {
		t.Errorf("a permissive key file was refused rather than warned about: %v", err)
	}
	if !strings.Contains(readErr.String(), "readable by others") {
		t.Errorf("the warning did not reach stderr: %s", readErr.String())
	}
}
