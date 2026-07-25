package sshd

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

func testAuth(t *testing.T, guest, openSignup bool) (*Authenticator, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenMemory(ctx, clock.NewVirtual(time.Unix(1_700_000_000, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewAuthenticator(st, guest, openSignup), st, ctx
}

func testPubKey(t *testing.T, seed uint64) gossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rng.TestSecret(seed).Reader())
	if err != nil {
		t.Fatal(err)
	}
	key, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func enroll(t *testing.T, st *store.Store, ctx context.Context, nick string, key gossh.PublicKey) {
	t.Helper()
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{Nick: nick, CanLogin: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddUserKey(ctx, nick, AuthorizedKeyOf(key), FingerprintOf(key), "test"); err != nil {
		t.Fatal(err)
	}
}

// §5.1: `ssh new@host` is the documented front door and must accept any key,
// including one never seen before.
func TestNewAccountAcceptsAnyKey(t *testing.T) {
	a, _, ctx := testAuth(t, true, true)
	key := testPubKey(t, 1)

	d := a.PublicKey(ctx, NewUser, key)
	if d.Intent != IntentSignup {
		t.Fatalf("intent is %v, want signup", d.Intent)
	}
	// The offered key must be carried through so signup can enrol it — that is
	// what makes the next login passwordless with no key-pasting step.
	if d.PublicKey == "" || d.Fingerprint == "" {
		t.Fatal("the offered key was not captured for enrolment")
	}
}

func TestNewAccountWorksOverPassword(t *testing.T) {
	a, _, ctx := testAuth(t, true, true)
	if d := a.Password(ctx, NewUser, "anything"); d.Intent != IntentSignup {
		t.Fatalf("intent is %v, want signup", d.Intent)
	}
}

func TestEnrolledKeyAuthenticates(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	key := testPubKey(t, 2)
	enroll(t, st, ctx, "austin", key)

	d := a.PublicKey(ctx, "austin", key)
	if d.Intent != IntentAuthenticated {
		t.Fatalf("intent is %v, want authenticated", d.Intent)
	}
	if d.Nick != "austin" {
		t.Fatalf("nick is %q", d.Nick)
	}
}

// THE critical case from §5.1: an existing nick presenting an unknown key must
// NOT be offered registration. Doing so is how duplicate accounts and confused
// users happen when someone's key merely changed.
func TestExistingNickWithUnknownKeyIsNotOfferedSignup(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	enrolled := testPubKey(t, 3)
	enroll(t, st, ctx, "austin", enrolled)

	stranger := testPubKey(t, 4)
	d := a.PublicKey(ctx, "austin", stranger)

	if d.Intent == IntentSignup {
		t.Fatal("an existing account with an unrecognised key was offered registration; " +
			"this creates duplicate accounts (§5.1)")
	}
	if d.Intent != IntentKeyUnknown {
		t.Fatalf("intent is %v, want key-unknown", d.Intent)
	}
	// The message must tell the user how to recover.
	if !strings.Contains(d.Reason, "password") {
		t.Errorf("reason does not explain the recovery path: %q", d.Reason)
	}
}

// §5.1 convenience path: an unknown nick offers registration, pre-filled.
func TestUnknownNickOffersSignup(t *testing.T) {
	a, _, ctx := testAuth(t, true, true)
	d := a.PublicKey(ctx, "brandnew", testPubKey(t, 5))
	if d.Intent != IntentSignup {
		t.Fatalf("intent is %v, want signup", d.Intent)
	}
	if d.Nick != "brandnew" {
		t.Fatalf("suggested nick is %q, want brandnew", d.Nick)
	}
}

// A known key must not authenticate as a DIFFERENT account, or anyone could
// log in as anyone by guessing a nick and offering their own key.
func TestKnownKeyCannotImpersonateAnotherAccount(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	austinKey := testPubKey(t, 6)
	enroll(t, st, ctx, "austin", austinKey)
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{Nick: "bob", CanLogin: true}); err != nil {
		t.Fatal(err)
	}

	d := a.PublicKey(ctx, "bob", austinKey)
	if d.Intent == IntentAuthenticated {
		t.Fatalf("austin's key authenticated as %q", d.Nick)
	}
	if d.Intent == IntentSignup {
		t.Fatal("offered to register an account that already exists")
	}
}

func TestGuestAccess(t *testing.T) {
	a, _, ctx := testAuth(t, true, true)
	if d := a.PublicKey(ctx, GuestUser, testPubKey(t, 7)); d.Intent != IntentGuest {
		t.Fatalf("intent is %v, want guest", d.Intent)
	}
	if d := a.Password(ctx, GuestUser, ""); d.Intent != IntentGuest {
		t.Fatalf("intent is %v, want guest", d.Intent)
	}
}

func TestGuestDisabled(t *testing.T) {
	a, _, ctx := testAuth(t, false, true)
	d := a.PublicKey(ctx, GuestUser, testPubKey(t, 8))
	if d.Intent != IntentUnknown {
		t.Fatalf("intent is %v, want unknown", d.Intent)
	}
	if !strings.Contains(d.Reason, "guest") {
		t.Errorf("reason does not explain: %q", d.Reason)
	}
}

func TestClosedRegistration(t *testing.T) {
	a, _, ctx := testAuth(t, true, false)
	for _, d := range []Decision{
		a.PublicKey(ctx, NewUser, testPubKey(t, 9)),
		a.Password(ctx, NewUser, "x"),
		a.PublicKey(ctx, "someone-new", testPubKey(t, 10)),
	} {
		if d.Intent != IntentUnknown {
			t.Fatalf("intent is %v, want unknown when registration is closed", d.Intent)
		}
	}
}

func TestPasswordAuthentication(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	hash, err := auth.HashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: "austin", PasswordHash: hash, CanLogin: true,
	}); err != nil {
		t.Fatal(err)
	}

	if d := a.Password(ctx, "austin", "hunter2"); d.Intent != IntentAuthenticated {
		t.Fatalf("correct password gave intent %v", d.Intent)
	}
	if d := a.Password(ctx, "austin", "wrong"); d.Intent != IntentUnknown {
		t.Fatal("wrong password authenticated")
	}
}

// The password path must not leak whether an account exists.
func TestPasswordFailureIsUniform(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	hash, _ := auth.HashPassword("pw")
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: "austin", PasswordHash: hash, CanLogin: true,
	}); err != nil {
		t.Fatal(err)
	}

	wrong := a.Password(ctx, "austin", "nope")
	// A nick that does not exist takes the signup path on the `new` account
	// only; an ordinary unknown nick with a password is just a failure.
	missing := a.Password(ctx, "ghost", "nope")

	if wrong.Reason != missing.Reason {
		t.Fatalf("reasons differ and leak account existence: %q vs %q", wrong.Reason, missing.Reason)
	}
}

// A reserved nick must not be registrable through the unknown-nick path.
func TestReservedNicksCannotBeRegistered(t *testing.T) {
	a, _, ctx := testAuth(t, true, true)
	for _, nick := range []string{"sysop", "admin", "root", "postmaster"} {
		d := a.PublicKey(ctx, nick, testPubKey(t, 11))
		if d.Intent == IntentSignup {
			t.Errorf("offered to register reserved nick %q", nick)
		}
	}
}

func TestNoLoginAccountsAreRejected(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	key := testPubKey(t, 12)
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{Nick: "mailbox", CanLogin: false}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddUserKey(ctx, "mailbox", AuthorizedKeyOf(key), FingerprintOf(key), ""); err != nil {
		t.Fatal(err)
	}
	if d := a.PublicKey(ctx, "mailbox", key); d.Intent != IntentUnknown {
		t.Fatalf("a no-login account authenticated: %v", d.Intent)
	}
}

func TestUsernameMatchingIsCaseInsensitive(t *testing.T) {
	a, st, ctx := testAuth(t, true, true)
	key := testPubKey(t, 13)
	enroll(t, st, ctx, "Austin", key)

	if d := a.PublicKey(ctx, "austin", key); d.Intent != IntentAuthenticated {
		t.Fatalf("case-different username failed to authenticate: %v (%s)", d.Intent, d.Reason)
	}
}
