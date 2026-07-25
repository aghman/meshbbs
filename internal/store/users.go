package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Capabilities are per-user grants (§6.7). A capability list beats a role
// ladder here because it maps directly onto the abuse vectors that exist.
const (
	// CapPostFederated permits posting to federated areas — that is, spending
	// the network's shared airtime. It is deliberately NOT granted by default
	// ([N7]): open front door, gated commons.
	CapPostFederated = "post_federated"

	CapSendDMOffnode = "send_dm_offnode"
	CapUploadFiles   = "upload_files"
	CapRunDoors      = "run_doors"
)

// DefaultCapabilities are granted to every new account. Note the absence of
// post_federated.
var DefaultCapabilities = []string{CapRunDoors, CapUploadFiles}

// KnownCapabilities is the full set, for validation and help text.
var KnownCapabilities = []string{
	CapPostFederated, CapSendDMOffnode, CapUploadFiles, CapRunDoors,
}

// ReservedNicks can never be registered (§5.1). `new` and `guest` are routing
// targets at the SSH auth layer; the rest would be confusing or impersonating.
var ReservedNicks = []string{
	"new", "guest", "sysop", "admin", "root", "bbs", "all", "postmaster", "daemon",
}

// ErrNickTaken is returned when a nick is already registered.
var ErrNickTaken = errors.New("nick is already taken")

// ErrNickReserved is returned for reserved nicks.
var ErrNickReserved = errors.New("nick is reserved")

// User is an account (§6.7).
type User struct {
	ID              int64
	Nick            string
	DisplayName     string
	PasswordHash    string
	DirectoryListed bool
	IsSysop         bool
	CanLogin        bool
	State           string
	CreatedAt       int64
	LastLoginAt     int64
}

// ValidateNick enforces the §6.7 nick rules.
//
// Uniqueness is per-instance only — nicks are never globally unique (§6.1.5) —
// which is what lets registration complete with no network round trip, on a
// node with no radio attached.
func ValidateNick(nick string) error {
	if len(nick) < 2 || len(nick) > 16 {
		return fmt.Errorf("nick must be 2-16 characters, got %d", len(nick))
	}
	first := rune(nick[0])
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')) {
		return errors.New("nick must start with a letter")
	}
	for _, r := range nick {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			return fmt.Errorf("nick contains invalid character %q; use letters, digits, _ or -", r)
		}
	}
	lower := strings.ToLower(nick)
	for _, r := range ReservedNicks {
		if lower == r {
			return fmt.Errorf("%w: %q", ErrNickReserved, nick)
		}
	}
	return nil
}

// CreateUserOptions configures account creation.
type CreateUserOptions struct {
	Nick         string
	DisplayName  string
	PasswordHash string
	IsSysop      bool
	CanLogin     bool
	Capabilities []string
}

// CreateUser creates an account and grants its capabilities atomically.
func (s *Store) CreateUser(ctx context.Context, opts CreateUserOptions) (User, error) {
	if err := ValidateNick(opts.Nick); err != nil {
		return User{}, err
	}
	caps := opts.Capabilities
	if caps == nil {
		caps = DefaultCapabilities
	}
	for _, c := range caps {
		if !isKnownCapability(c) {
			return User{}, fmt.Errorf("unknown capability %q (known: %s)",
				c, strings.Join(KnownCapabilities, ", "))
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin create user: %w", err)
	}
	defer tx.Rollback()

	var taken int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE nick = ?`, opts.Nick).Scan(&taken); err != nil {
		return User{}, fmt.Errorf("check nick: %w", err)
	}
	if taken > 0 {
		return User{}, fmt.Errorf("%w: %q", ErrNickTaken, opts.Nick)
	}

	now := s.now()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO users (nick, display_name, password_hash, directory_listed, is_sysop, can_login, state, created_at)
		VALUES (?, ?, ?, 1, ?, ?, 'active', ?)`,
		opts.Nick, opts.DisplayName, opts.PasswordHash,
		boolToInt(opts.IsSysop), boolToInt(opts.CanLogin), now)
	if err != nil {
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}

	// Sort so the insert order — and therefore the audit trail — is
	// deterministic rather than dependent on the caller's slice order.
	sorted := append([]string(nil), caps...)
	sort.Strings(sorted)
	for _, c := range sorted {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO user_capabilities (user_id, capability, granted_at, granted_by)
			 VALUES (?, ?, ?, 'system')`, id, c, now); err != nil {
			return User{}, fmt.Errorf("grant %s: %w", c, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit create user: %w", err)
	}

	return User{
		ID: id, Nick: opts.Nick, DisplayName: opts.DisplayName,
		PasswordHash: opts.PasswordHash, DirectoryListed: true,
		IsSysop: opts.IsSysop, CanLogin: opts.CanLogin,
		State: "active", CreatedAt: now,
	}, nil
}

func isKnownCapability(c string) bool {
	for _, k := range KnownCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// GetUser loads an account by nick (case-insensitive).
func (s *Store) GetUser(ctx context.Context, nick string) (User, error) {
	var u User
	var listed, sysop, login int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, nick, display_name, password_hash, directory_listed, is_sysop, can_login, state, created_at, last_login_at
		FROM users WHERE nick = ?`, nick).
		Scan(&u.ID, &u.Nick, &u.DisplayName, &u.PasswordHash, &listed,
			&sysop, &login, &u.State, &u.CreatedAt, &u.LastLoginAt)
	if isNoRows(err) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	u.DirectoryListed, u.IsSysop, u.CanLogin = listed == 1, sysop == 1, login == 1
	return u, nil
}

// ListUsers returns all accounts, ordered by nick.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, nick, display_name, password_hash, directory_listed, is_sysop, can_login, state, created_at, last_login_at
		FROM users ORDER BY nick`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		var listed, sysop, login int
		if err := rows.Scan(&u.ID, &u.Nick, &u.DisplayName, &u.PasswordHash, &listed,
			&sysop, &login, &u.State, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, err
		}
		u.DirectoryListed, u.IsSysop, u.CanLogin = listed == 1, sysop == 1, login == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

// GrantCapability grants a capability to a user.
func (s *Store) GrantCapability(ctx context.Context, nick, capability, grantedBy string) error {
	if !isKnownCapability(capability) {
		return fmt.Errorf("unknown capability %q (known: %s)",
			capability, strings.Join(KnownCapabilities, ", "))
	}
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_capabilities (user_id, capability, granted_at, granted_by)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, capability) DO NOTHING`,
		u.ID, capability, s.now(), grantedBy)
	if err != nil {
		return fmt.Errorf("grant capability: %w", err)
	}
	return s.audit(ctx, grantedBy, "capability.grant", nick, capability)
}

// RevokeCapability removes a capability from a user.
func (s *Store) RevokeCapability(ctx context.Context, nick, capability, actor string) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM user_capabilities WHERE user_id = ? AND capability = ?`,
		u.ID, capability); err != nil {
		return fmt.Errorf("revoke capability: %w", err)
	}
	return s.audit(ctx, actor, "capability.revoke", nick, capability)
}

// Capabilities returns a user's capabilities, sorted.
func (s *Store) Capabilities(ctx context.Context, nick string) ([]string, error) {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT capability FROM user_capabilities WHERE user_id = ? ORDER BY capability`, u.ID)
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// HasCapability reports whether a user holds a capability.
func (s *Store) HasCapability(ctx context.Context, nick, capability string) (bool, error) {
	caps, err := s.Capabilities(ctx, nick)
	if err != nil {
		return false, err
	}
	for _, c := range caps {
		if c == capability {
			return true, nil
		}
	}
	return false, nil
}

// AddUserKey enrolls an SSH public key for a user. Multiple keys per user is a
// v1 requirement (§6.7): one per laptop, phone, and shell box.
func (s *Store) AddUserKey(ctx context.Context, nick, publicKey, fingerprint, comment string) error {
	u, err := s.GetUser(ctx, nick)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO user_keys (user_id, public_key, fingerprint, comment, added_at)
		VALUES (?, ?, ?, ?, ?)`, u.ID, publicKey, fingerprint, comment, s.now())
	if err != nil {
		return fmt.Errorf("add user key: %w", err)
	}
	return nil
}

// audit appends to the audit log (§11.6).
func (s *Store) audit(ctx context.Context, actor, action, target, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		s.now(), actor, action, target, detail)
	if err != nil {
		return fmt.Errorf("write audit log: %w", err)
	}
	return nil
}

// Audit records an event from outside this package.
func (s *Store) Audit(ctx context.Context, actor, action, target, detail string) error {
	return s.audit(ctx, actor, action, target, detail)
}
