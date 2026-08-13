package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/keyring"
)

// ErrNoPassword is returned when a user has no password set, so password
// authentication cannot succeed for them.
var ErrNoPassword = errors.New("account has no password set")

// AuthenticatePassword verifies a password and returns the user.
//
// It deliberately returns the same error for "no such user" and "wrong
// password": distinguishing them turns the login prompt into a nick oracle.
func (s *Store) AuthenticatePassword(ctx context.Context, nick, password string) (User, error) {
	fail := errors.New("invalid credentials")

	u, err := s.GetUser(ctx, nick)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Spend comparable time on a missing account so response timing
			// does not reveal whether the nick exists.
			_ = auth.VerifyPassword(password, dummyHash)
			return User{}, fail
		}
		return User{}, err
	}
	if u.PasswordHash == "" {
		_ = auth.VerifyPassword(password, dummyHash)
		return User{}, fail
	}
	if err := auth.VerifyPassword(password, u.PasswordHash); err != nil {
		return User{}, fail
	}
	if !u.CanLogin {
		return User{}, errors.New("this account cannot log in")
	}
	return u, nil
}

// dummyHash is a real Argon2id hash of an unguessable value, used to equalise
// timing when an account does not exist.
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=4$" +
	"YWJjZGVmZ2hpamtsbW5vcA$Zm9vYmFyYmF6cXV1eGNvcmdlZ3JhdWx0Z2FycGx5" // nolint

// UserByFingerprint finds the account owning an SSH public key fingerprint.
func (s *Store) UserByFingerprint(ctx context.Context, fingerprint string) (User, error) {
	var nick string
	err := s.db.QueryRowContext(ctx,
		`SELECT u.nick FROM users u
		 JOIN user_keys k ON k.user_id = u.id
		 WHERE k.fingerprint = ?`, fingerprint).Scan(&nick)
	if isNoRows(err) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("look up key: %w", err)
	}
	return s.GetUser(ctx, nick)
}

// UserKey is an enrolled SSH public key.
type UserKey struct {
	Fingerprint string
	PublicKey   string
	Comment     string
	AddedAt     int64
}

// UserKeys lists a user's enrolled keys. Multiple keys per user is a v1
// requirement (§6.7) — one per laptop, phone, and shell box.
func (s *Store) UserKeys(ctx context.Context, nick string) ([]UserKey, error) {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT fingerprint, public_key, comment, added_at
		 FROM user_keys WHERE user_id = ? ORDER BY added_at, fingerprint`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("list user keys: %w", err)
	}
	defer rows.Close()

	var out []UserKey
	for rows.Next() {
		var k UserKey
		if err := rows.Scan(&k.Fingerprint, &k.PublicKey, &k.Comment, &k.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecordLogin stamps a successful login.
func (s *Store) RecordLogin(ctx context.Context, nick string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE nick = ?`, s.now(), nick)
	if err != nil {
		return fmt.Errorf("record login: %w", err)
	}
	return nil
}

// SetPassword sets or replaces a user's password hash.
//
// This does NOT touch the DM key. See RewrapDMKey and the warning in
// ResetPasswordAsSysop for why those are separate operations.
func (s *Store) SetPassword(ctx context.Context, nick, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = ? WHERE nick = ?`, hash, nick)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// DM key custody (§8.2)
// ---------------------------------------------------------------------------

// SetDMKey stores a user's DM keypair: the public half in the clear, the
// private half wrapped under their passphrase.
func (s *Store) SetDMKey(ctx context.Context, nick string, pub keyring.PublicKey, wrapped *keyring.WrappedKey) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET dm_public_key = ?, dm_wrapped_key = ? WHERE nick = ?`,
		pub.String(), wrapped.Encode(), nick)
	if err != nil {
		return fmt.Errorf("store DM key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ErrWouldStrandExistingMail is returned when adopting a client-held key would
// leave mail behind that nobody can read.
//
// The same loss ErrDMHistoryWouldBeLost describes, arriving from the other
// direction: messages already in the inbox are sealed to the OLD public key, and
// a new key cannot open them. The server cannot re-seal them either — it never
// had the private half, which is the whole point of §8.2 — so they are gone the
// moment the key changes.
var ErrWouldStrandExistingMail = errors.New(
	"this account already has a DM key, and messages already delivered are sealed to it; " +
		"adopting a different key makes them permanently unreadable")

// SetClientHeldDMKey adopts a public key whose private half lives on the user's
// own machine (§8.2 tier 3).
//
// The wrapped column is set to NULL rather than left alone, and that is the
// whole mechanism: a row with a public key and no wrapped key IS a tier-3 user.
// Everything the server does — discovery, addressing, verification, delivery —
// already works from the public half, so nothing else changes. Leaving a stale
// wrapped key behind would be worse than untidy: OpenDM would find it, unwrap it
// with the session passphrase, and hand back plaintext for a user who had been
// told their key was theirs alone.
//
// replace guards the loss above. Without it an account that already has a key
// keeps it, because the alternative is silently stranding mail.
func (s *Store) SetClientHeldDMKey(ctx context.Context, nick string, pub keyring.PublicKey, replace bool) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	var existing string
	if err := s.db.QueryRowContext(ctx,
		`SELECT dm_public_key FROM users WHERE id = ?`, u.ID).Scan(&existing); err != nil {
		return fmt.Errorf("check the existing DM key: %w", err)
	}
	if existing != "" && existing != pub.String() && !replace {
		return ErrWouldStrandExistingMail
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE users SET dm_public_key = ?, dm_wrapped_key = NULL WHERE id = ?`,
		pub.String(), u.ID); err != nil {
		return fmt.Errorf("store the client-held DM key: %w", err)
	}
	return s.audit(ctx, "cli", "user.dm_key_client_held", nick, pub.String())
}

// DMKeyIsClientHeld reports whether a user holds their own private key.
//
// A public key with no wrapped private half. Distinguished from "no key at all"
// because the two call for opposite behaviour: one gets an armoured block to
// take away, the other gets a key generated for them at next login.
func (s *Store) DMKeyIsClientHeld(ctx context.Context, nick string) (bool, error) {
	var pub string
	var wrapped []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT dm_public_key, dm_wrapped_key FROM users WHERE nick = ?`, nick).Scan(&pub, &wrapped)
	if isNoRows(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read DM key custody: %w", err)
	}
	return pub != "" && len(wrapped) == 0, nil
}

// DMPublicKey returns a user's DM public key.
//
// Encrypting to a user needs only this. Nothing in the send path ever needs
// the private half — that is the §8.2 boundary that keeps tier 3 possible.
func (s *Store) DMPublicKey(ctx context.Context, nick string) (keyring.PublicKey, error) {
	var encoded string
	err := s.db.QueryRowContext(ctx,
		`SELECT dm_public_key FROM users WHERE nick = ?`, nick).Scan(&encoded)
	if isNoRows(err) {
		return keyring.PublicKey{}, ErrNotFound
	}
	if err != nil {
		return keyring.PublicKey{}, fmt.Errorf("read DM public key: %w", err)
	}
	if encoded == "" {
		return keyring.PublicKey{}, fmt.Errorf("%s has no DM key yet", nick)
	}
	return keyring.ParsePublicKey(encoded)
}

// WrappedDMKey returns a user's wrapped private key.
//
// The caller still needs the passphrase to do anything with it — this returns
// ciphertext. It is separated from DMPublicKey so that code reaching for
// private material is visibly doing so.
func (s *Store) WrappedDMKey(ctx context.Context, nick string) (*keyring.WrappedKey, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT dm_wrapped_key FROM users WHERE nick = ?`, nick).Scan(&blob)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read wrapped DM key: %w", err)
	}
	if len(blob) == 0 {
		return nil, fmt.Errorf("%s has no DM key yet", nick)
	}
	return keyring.DecodeWrapped(blob)
}

// ChangePasswordAndRewrapDMKey changes a password and re-wraps the DM key
// under the new passphrase, atomically.
//
// This is the path a USER takes, and it works because the user supplies the
// old passphrase. Compare ResetPasswordAsSysop, which cannot.
func (s *Store) ChangePasswordAndRewrapDMKey(ctx context.Context, nick, oldPassphrase, newPassword, newPassphrase string) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	if err := auth.VerifyPassword(oldPassphrase, u.PasswordHash); err != nil {
		return errors.New("current password is incorrect")
	}

	wrapped, err := s.WrappedDMKey(ctx, nick)
	if err != nil {
		return err
	}
	rewrapped, err := keyring.Rewrap(wrapped, oldPassphrase, newPassphrase)
	if err != nil {
		return fmt.Errorf("re-wrap DM key: %w", err)
	}
	newHash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = ?, dm_wrapped_key = ? WHERE nick = ?`,
		newHash, rewrapped.Encode(), nick); err != nil {
		return fmt.Errorf("update credentials: %w", err)
	}
	return tx.Commit()
}

// ErrDMHistoryWouldBeLost is returned by ResetPasswordAsSysop when the user
// has a DM key that the reset cannot re-wrap.
//
// §6.7 requires this to be surfaced rather than silently accepted: a sysop
// reset does not have the user's passphrase, so it cannot re-wrap the key, and
// proceeding would leave the user with mail they can never read again.
var ErrDMHistoryWouldBeLost = errors.New(
	"this account has a DM key wrapped under its old passphrase; a sysop reset " +
		"cannot re-wrap it, so the user's existing DM history will become permanently unreadable")

// ResetPasswordAsSysop performs an administrative password reset.
//
// It refuses by default when the user holds a DM key, because the reset would
// silently destroy their mail. Pass discardDMKey to proceed, which clears the
// key rather than leaving an unusable one behind — the user gets a fresh
// keypair at next login and their old messages are gone. That is a real loss,
// and it should be a deliberate act by a sysop who has been told.
func (s *Store) ResetPasswordAsSysop(ctx context.Context, nick, newPassword string, discardDMKey bool) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}

	var blob []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT dm_wrapped_key FROM users WHERE nick = ?`, nick).Scan(&blob); err != nil {
		return fmt.Errorf("check DM key: %w", err)
	}
	hasKey := len(blob) > 0
	if hasKey && !discardDMKey {
		return ErrDMHistoryWouldBeLost
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if hasKey {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ?, dm_public_key = '', dm_wrapped_key = NULL WHERE id = ?`,
			hash, u.ID); err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET password_hash = ? WHERE id = ?`, hash, u.ID); err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, detail) VALUES (?, 'sysop', 'user.password_reset', ?, ?)`,
		s.now(), nick, map[bool]string{true: "DM key discarded", false: ""}[hasKey]); err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return tx.Commit()
}
