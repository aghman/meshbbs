package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Passkey storage for the web front end (webui.md §7).

// WebAuthnCredential is one enrolled passkey.
type WebAuthnCredential struct {
	CredentialID []byte
	PublicKey    []byte
	SignCount    uint32
	AAGUID       []byte
	Transports   []string
	Label        string
	AddedAt      int64
	LastUsedAt   int64
}

// ErrCredentialExists is returned when a passkey is enrolled twice.
var ErrCredentialExists = errors.New("credential already enrolled")

// ErrCloned is returned when an authenticator's signature counter goes
// backwards, which means the credential has been copied.
var ErrCloned = errors.New("authenticator signature counter went backwards")

// AddWebAuthnCredential enrols a passkey on an account.
func (s *Store) AddWebAuthnCredential(ctx context.Context, nick string, c WebAuthnCredential) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin add passkey: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Check before inserting, matching CreateUser: the UNIQUE index is the
	// backstop against a race, and this is what turns the common case into a
	// named error rather than a driver string.
	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM webauthn_credentials WHERE credential_id = ?`,
		c.CredentialID).Scan(&taken); err != nil {
		return fmt.Errorf("check passkey: %w", err)
	}
	if taken > 0 {
		return ErrCredentialExists
	}

	// A nil slice binds as NULL, and a column DEFAULT only applies when the
	// column is omitted entirely — so an authenticator that reports no AAGUID
	// would otherwise fail the NOT NULL constraint rather than storing empty.
	aaguid := c.AAGUID
	if aaguid == nil {
		aaguid = []byte{}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO webauthn_credentials
		   (user_id, credential_id, public_key, sign_count, aaguid, transports, label, added_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, c.CredentialID, c.PublicKey, c.SignCount, aaguid,
		strings.Join(c.Transports, ","), c.Label, s.now()); err != nil {
		return fmt.Errorf("add passkey: %w", err)
	}
	return tx.Commit()
}

// WebAuthnCredentials lists a user's passkeys.
func (s *Store) WebAuthnCredentials(ctx context.Context, nick string) ([]WebAuthnCredential, error) {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT credential_id, public_key, sign_count, aaguid, transports, label,
		        added_at, COALESCE(last_used_at, 0)
		 FROM webauthn_credentials WHERE user_id = ? ORDER BY added_at, id`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()

	var out []WebAuthnCredential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WebAuthnCredentialByID finds a passkey and the account it belongs to.
//
// Sign-in arrives with only the credential ID — discoverable credentials mean
// the user never types a nick ([D17]) — so this is the lookup that resolves an
// assertion to an account.
func (s *Store) WebAuthnCredentialByID(ctx context.Context, credentialID []byte) (WebAuthnCredential, User, error) {
	var nick string
	row := s.db.QueryRowContext(ctx,
		`SELECT c.credential_id, c.public_key, c.sign_count, c.aaguid, c.transports,
		        c.label, c.added_at, COALESCE(c.last_used_at, 0), u.nick
		 FROM webauthn_credentials c
		 JOIN users u ON u.id = c.user_id
		 WHERE c.credential_id = ?`, credentialID)

	var c WebAuthnCredential
	var transports string
	err := row.Scan(&c.CredentialID, &c.PublicKey, &c.SignCount, &c.AAGUID,
		&transports, &c.Label, &c.AddedAt, &c.LastUsedAt, &nick)
	if isNoRows(err) {
		return WebAuthnCredential{}, User{}, ErrNotFound
	}
	if err != nil {
		return WebAuthnCredential{}, User{}, fmt.Errorf("look up passkey: %w", err)
	}
	c.Transports = splitTransports(transports)

	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return WebAuthnCredential{}, User{}, err
	}
	return c, u, nil
}

// UseWebAuthnCredential records a successful assertion and advances the
// signature counter.
//
// # Why this rejects rather than repairs
//
// A real authenticator's counter only ever increases. A value at or below what
// is already stored means two devices are presenting the same credential — the
// credential has been cloned — and the honest response is to refuse the
// assertion, not to accept it and quietly write the lower value.
//
// Authenticators that do not implement a counter report zero forever. That is
// legal and common (most platform authenticators do it), so a stored zero and
// an offered zero is the normal case, not a clone.
func (s *Store) UseWebAuthnCredential(ctx context.Context, credentialID []byte, signCount uint32) error {
	var stored uint32
	err := s.db.QueryRowContext(ctx,
		`SELECT sign_count FROM webauthn_credentials WHERE credential_id = ?`,
		credentialID).Scan(&stored)
	if isNoRows(err) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read signature counter: %w", err)
	}
	if signCount != 0 && signCount <= stored {
		return ErrCloned
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE webauthn_credentials SET sign_count = ?, last_used_at = ? WHERE credential_id = ?`,
		signCount, s.now(), credentialID)
	if err != nil {
		return fmt.Errorf("record passkey use: %w", err)
	}
	return nil
}

// RemoveWebAuthnCredential revokes one passkey.
func (s *Store) RemoveWebAuthnCredential(ctx context.Context, nick string, credentialID []byte) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webauthn_credentials WHERE user_id = ? AND credential_id = ?`,
		u.ID, credentialID)
	if err != nil {
		return fmt.Errorf("remove passkey: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Enrolment codes ([D18])
// ---------------------------------------------------------------------------

// ErrCodeExpired is returned when a code is past its expiry.
var ErrCodeExpired = errors.New("enrolment code has expired")

// PutEnrolmentCode stores a code hash for an account, replacing any live one.
//
// The replacement is the point: one live code per account means a code glimpsed
// over somebody's shoulder is dead as soon as the owner issues another, and
// that codes cannot be stockpiled.
func (s *Store) PutEnrolmentCode(ctx context.Context, nick, hash string, expiresAt int64) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO webauthn_enrolment_codes (user_id, code_hash, issued_at, expires_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (user_id) DO UPDATE SET
		   code_hash = excluded.code_hash,
		   issued_at = excluded.issued_at,
		   expires_at = excluded.expires_at`,
		u.ID, hash, s.now(), expiresAt)
	if err != nil {
		return fmt.Errorf("store enrolment code: %w", err)
	}
	return nil
}

// RedeemEnrolmentCode consumes a code and returns the account it belongs to.
//
// Redemption DELETES the row whether or not the code had expired, so a code is
// single-use in every outcome. Returning the expiry as a distinct error lets
// the UI say "that code has expired, ask for another" instead of "wrong code",
// which is the difference between a user retrying successfully and giving up.
//
// This grants NO session by itself. The caller may register a passkey for the
// returned account and nothing else ([D18]).
func (s *Store) RedeemEnrolmentCode(ctx context.Context, hash string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin redeem: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var userID int64
	var expiresAt int64
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, expires_at FROM webauthn_enrolment_codes WHERE code_hash = ?`,
		hash).Scan(&userID, &expiresAt)
	if isNoRows(err) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("look up enrolment code: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM webauthn_enrolment_codes WHERE user_id = ?`, userID); err != nil {
		return User{}, fmt.Errorf("consume enrolment code: %w", err)
	}

	var nick string
	if err := tx.QueryRowContext(ctx, `SELECT nick FROM users WHERE id = ?`, userID).Scan(&nick); err != nil {
		return User{}, fmt.Errorf("resolve enrolment code account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit redeem: %w", err)
	}

	if s.now() >= expiresAt {
		return User{}, ErrCodeExpired
	}
	return s.GetUser(ctx, nick)
}

// ---------------------------------------------------------------------------

func scanCredential(rows *sql.Rows) (WebAuthnCredential, error) {
	var c WebAuthnCredential
	var transports string
	if err := rows.Scan(&c.CredentialID, &c.PublicKey, &c.SignCount, &c.AAGUID,
		&transports, &c.Label, &c.AddedAt, &c.LastUsedAt); err != nil {
		return WebAuthnCredential{}, err
	}
	c.Transports = splitTransports(transports)
	return c, nil
}

func splitTransports(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
