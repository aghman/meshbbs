package store

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/keyring"
)

func userWithPassword(t *testing.T, s *Store, ctx context.Context, nick, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, CreateUserOptions{
		Nick: nick, PasswordHash: hash, CanLogin: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func giveDMKey(t *testing.T, s *Store, ctx context.Context, nick, passphrase string) keyring.PublicKey {
	t.Helper()
	priv, pub, err := keyring.Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer priv.Zero()
	wrapped, err := keyring.Wrap(priv, passphrase)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDMKey(ctx, nick, pub, wrapped); err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestAuthenticatePassword(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "hunter2")

	if _, err := s.AuthenticatePassword(ctx, "austin", "hunter2"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if _, err := s.AuthenticatePassword(ctx, "austin", "wrong"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

// A login prompt must not become a nick oracle: a missing account and a wrong
// password must be indistinguishable to the caller.
func TestAuthenticateDoesNotRevealWhetherNickExists(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "hunter2")

	_, errWrongPassword := s.AuthenticatePassword(ctx, "austin", "nope")
	_, errNoSuchUser := s.AuthenticatePassword(ctx, "nobody", "nope")

	if errWrongPassword == nil || errNoSuchUser == nil {
		t.Fatal("expected both to fail")
	}
	if errWrongPassword.Error() != errNoSuchUser.Error() {
		t.Fatalf("errors differ and leak account existence:\n  wrong password: %v\n  no such user:   %v",
			errWrongPassword, errNoSuchUser)
	}
}

func TestAuthenticateRejectsNoLoginAccounts(t *testing.T) {
	s, ctx := testStore(t)
	hash, _ := auth.HashPassword("pw")
	if _, err := s.CreateUser(ctx, CreateUserOptions{
		Nick: "mailbox", PasswordHash: hash, CanLogin: false,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticatePassword(ctx, "mailbox", "pw"); err == nil {
		t.Fatal("a no-login account authenticated")
	}
}

func TestUserByFingerprint(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "austin", CanLogin: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUserKey(ctx, "austin", "ssh-ed25519 AAAA...", "SHA256:abc", "laptop"); err != nil {
		t.Fatal(err)
	}

	u, err := s.UserByFingerprint(ctx, "SHA256:abc")
	if err != nil {
		t.Fatal(err)
	}
	if u.Nick != "austin" {
		t.Fatalf("resolved to %q", u.Nick)
	}
	if _, err := s.UserByFingerprint(ctx, "SHA256:unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// §6.7: one key per laptop, phone and shell box is the normal case.
func TestMultiplePubkeysPerUser(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "austin", CanLogin: true}); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{"SHA256:laptop", "SHA256:phone", "SHA256:shell"} {
		if err := s.AddUserKey(ctx, "austin", "ssh-ed25519 X", fp, fp); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := s.UserKeys(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("enrolled %d keys, want 3", len(keys))
	}
	for _, fp := range []string{"SHA256:laptop", "SHA256:phone", "SHA256:shell"} {
		u, err := s.UserByFingerprint(ctx, fp)
		if err != nil || u.Nick != "austin" {
			t.Fatalf("key %s did not resolve to austin: %v", fp, err)
		}
	}
}

// §8.2: sending a DM needs only the public key, which the store hands out
// freely. Nothing in the send path touches wrapped private material.
func TestDMPublicKeyIsAvailableWithoutAnyPassphrase(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "pw")
	pub := giveDMKey(t, s, ctx, "austin", "dm-passphrase")

	got, err := s.DMPublicKey(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Fatal("stored public key differs")
	}

	// And it is genuinely usable for encryption with no secret involved.
	if _, err := keyring.Seal(got, []byte("hello")); err != nil {
		t.Fatalf("could not seal to the retrieved public key: %v", err)
	}
}

// The wrapped key is ciphertext: retrieving it is not enough to read anything.
func TestWrappedDMKeyIsUselessWithoutThePassphrase(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "pw")
	giveDMKey(t, s, ctx, "austin", "dm-passphrase")

	wrapped, err := s.WrappedDMKey(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Unwrap(wrapped, "guessing"); !errors.Is(err, keyring.ErrWrongPassphrase) {
		t.Fatalf("expected the wrapped key to resist a wrong passphrase, got %v", err)
	}
	if _, err := keyring.Unwrap(wrapped, "dm-passphrase"); err != nil {
		t.Fatalf("correct passphrase failed: %v", err)
	}
}

// The user-initiated path works because the user supplies the old passphrase.
func TestUserPasswordChangeRewrapsTheDMKey(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "old-pw")
	giveDMKey(t, s, ctx, "austin", "old-pw")

	// Grab the plaintext key before the change so we can prove it survived.
	before, err := s.WrappedDMKey(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	original, err := keyring.Unwrap(before, "old-pw")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.ChangePasswordAndRewrapDMKey(ctx, "austin", "old-pw", "new-pw", "new-pw"); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AuthenticatePassword(ctx, "austin", "new-pw"); err != nil {
		t.Fatalf("new password does not authenticate: %v", err)
	}
	after, err := s.WrappedDMKey(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := keyring.Unwrap(after, "new-pw")
	if err != nil {
		t.Fatalf("DM key not readable with the new passphrase: %v", err)
	}
	if recovered != original {
		t.Fatal("the DM key changed during a password change; existing mail would be unreadable")
	}
}

func TestUserPasswordChangeRequiresTheOldPassword(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "old-pw")
	giveDMKey(t, s, ctx, "austin", "old-pw")

	if err := s.ChangePasswordAndRewrapDMKey(ctx, "austin", "wrong", "new-pw", "new-pw"); err == nil {
		t.Fatal("password change succeeded without the current password")
	}
}

// §6.7, the trap: a sysop reset cannot re-wrap the DM key because the sysop
// does not have the passphrase. It must refuse rather than silently destroy
// the user's mail.
func TestSysopResetRefusesToSilentlyDestroyDMHistory(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "austin", "old-pw")
	giveDMKey(t, s, ctx, "austin", "old-pw")

	err := s.ResetPasswordAsSysop(ctx, "austin", "reset-pw", false)
	if !errors.Is(err, ErrDMHistoryWouldBeLost) {
		t.Fatalf("expected ErrDMHistoryWouldBeLost, got %v", err)
	}
	// The password must be unchanged after a refused reset.
	if _, err := s.AuthenticatePassword(ctx, "austin", "old-pw"); err != nil {
		t.Fatal("a refused reset changed the password anyway")
	}

	// Explicitly acknowledging the loss proceeds, and clears the now-unusable
	// key rather than leaving one nobody can open.
	if err := s.ResetPasswordAsSysop(ctx, "austin", "reset-pw", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticatePassword(ctx, "austin", "reset-pw"); err != nil {
		t.Fatalf("reset password does not authenticate: %v", err)
	}
	if _, err := s.WrappedDMKey(ctx, "austin"); err == nil {
		t.Fatal("an unusable wrapped key was left behind after a forced reset")
	}
}

// A user with no DM key can be reset freely — there is nothing to lose.
func TestSysopResetIsUnproblematicWithoutADMKey(t *testing.T) {
	s, ctx := testStore(t)
	userWithPassword(t, s, ctx, "bob", "old-pw")

	if err := s.ResetPasswordAsSysop(ctx, "bob", "new-pw", false); err != nil {
		t.Fatalf("reset refused for a user with no DM key: %v", err)
	}
	if _, err := s.AuthenticatePassword(ctx, "bob", "new-pw"); err != nil {
		t.Fatal(err)
	}
}

// The migration must not have introduced a plaintext key column.
func TestNoPlaintextDMKeyColumn(t *testing.T) {
	s, ctx := testStore(t)
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('users')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(name)
		if lower == "dm_private_key" || lower == "dm_passphrase" || lower == "passphrase" {
			t.Fatalf("users table has column %q; the server must never store plaintext key material (§8.2)", name)
		}
	}
}
