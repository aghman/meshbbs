package store

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// Node is a roster entry (§6.1.2).
type Node struct {
	ID           identity.NodeID
	PublicKey    ed25519.PublicKey
	DisplayName  string
	SysopContact string
	Incarnation  uint32
	IsSelf       bool
	FirstSeen    int64
	LastSeen     int64
}

// PutNode records or refreshes a roster entry.
//
// The public key is re-checked against the ID here even though the caller
// should already have verified the NODE record. This is cheap and it means the
// database cannot hold a row whose key and ID disagree — an invariant worth
// having unconditionally, since every signature check depends on it.
func (s *Store) PutNode(ctx context.Context, n Node) error {
	if !n.ID.Matches(n.PublicKey) {
		return fmt.Errorf("public key hashes to %s, not to %s",
			identity.NodeIDFromPublicKey(n.PublicKey), n.ID)
	}

	now := s.now()
	if n.FirstSeen == 0 {
		n.FirstSeen = now
	}
	if n.LastSeen == 0 {
		n.LastSeen = now
	}

	self := 0
	if n.IsSelf {
		self = 1
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO nodes (id, public_key, display_name, sysop_contact, incarnation, is_self, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			display_name  = excluded.display_name,
			sysop_contact = excluded.sysop_contact,
			incarnation   = MAX(nodes.incarnation, excluded.incarnation),
			last_seen     = excluded.last_seen`,
		n.ID[:], []byte(n.PublicKey), n.DisplayName, n.SysopContact,
		n.Incarnation, self, n.FirstSeen, n.LastSeen)
	if err != nil {
		return fmt.Errorf("put node: %w", err)
	}
	return nil
}

// PutNodeFromRecord verifies a NODE record and stores the roster entry.
//
// Verification is entirely self-contained (§6.1.2), so this is safe to call on
// a record received from any peer, trusted or not.
func (s *Store) PutNodeFromRecord(ctx context.Context, r *record.Record) error {
	body, err := record.VerifyNodeRecord(r)
	if err != nil {
		return err
	}
	return s.PutNode(ctx, Node{
		ID:           r.Origin,
		PublicKey:    body.PublicKey,
		DisplayName:  body.DisplayName,
		SysopContact: body.SysopContact,
		Incarnation:  body.Incarnation,
	})
}

// GetNode loads a roster entry.
func (s *Store) GetNode(ctx context.Context, id identity.NodeID) (Node, error) {
	var n Node
	var raw, key []byte
	var self int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, public_key, display_name, sysop_contact, incarnation, is_self, first_seen, last_seen
		FROM nodes WHERE id = ?`, id[:]).
		Scan(&raw, &key, &n.DisplayName, &n.SysopContact, &n.Incarnation, &self, &n.FirstSeen, &n.LastSeen)
	if isNoRows(err) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("get node: %w", err)
	}
	copy(n.ID[:], raw)
	n.PublicKey = ed25519.PublicKey(key)
	n.IsSelf = self == 1
	return n, nil
}

// SelfNode returns this instance's own roster entry.
func (s *Store) SelfNode(ctx context.Context) (Node, error) {
	var n Node
	var raw, key []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, public_key, display_name, sysop_contact, incarnation, first_seen, last_seen
		FROM nodes WHERE is_self = 1`).
		Scan(&raw, &key, &n.DisplayName, &n.SysopContact, &n.Incarnation, &n.FirstSeen, &n.LastSeen)
	if isNoRows(err) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("get self node: %w", err)
	}
	copy(n.ID[:], raw)
	n.PublicKey = ed25519.PublicKey(key)
	n.IsSelf = true
	return n, nil
}

// ListNodes returns the roster, ordered by ID so output is deterministic.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, public_key, display_name, sysop_contact, incarnation, is_self, first_seen, last_seen
		FROM nodes ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var n Node
		var raw, key []byte
		var self int
		if err := rows.Scan(&raw, &key, &n.DisplayName, &n.SysopContact,
			&n.Incarnation, &self, &n.FirstSeen, &n.LastSeen); err != nil {
			return nil, err
		}
		copy(n.ID[:], raw)
		n.PublicKey = ed25519.PublicKey(key)
		n.IsSelf = self == 1
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Aliases (§6.1.4, [N1])
// ---------------------------------------------------------------------------

// MaxAliasLen bounds an alias.
const MaxAliasLen = 32

// SetAlias binds a local petname to a node ID.
//
// Aliases are sysop-owned and local-only: they are resolved at compose time and
// never travel on the wire (§6.1.4.1). Two instances may disagree about what
// `pnw` means and neither is wrong, which is exactly why no registry is needed.
func (s *Store) SetAlias(ctx context.Context, alias string, id identity.NodeID) error {
	alias = strings.TrimSpace(alias)
	if err := ValidateAlias(alias); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO aliases (alias, node_id, created_at) VALUES (?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET node_id = excluded.node_id`,
		alias, id[:], s.now())
	if err != nil {
		return fmt.Errorf("set alias: %w", err)
	}
	return nil
}

// ValidateAlias enforces the alias namespace rules.
//
// The critical rule is the last one: an alias must never be parseable as a
// literal node ID. Resolution tries the literal form first (§6.1.4.1), so an
// alias that looks like an ID would be permanently shadowed — a confusing
// failure that is much better prevented than diagnosed.
func ValidateAlias(alias string) error {
	if alias == "" {
		return fmt.Errorf("alias must not be empty")
	}
	if len(alias) > MaxAliasLen {
		return fmt.Errorf("alias is %d bytes, limit is %d", len(alias), MaxAliasLen)
	}
	for _, r := range alias {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("alias contains invalid character %q; use letters, digits, - _ or .", r)
		}
	}
	if _, err := identity.ParseNodeID(alias); err == nil {
		return fmt.Errorf("alias %q is also a valid node ID, which would make it unreachable "+
			"(literal IDs are resolved first, §6.1.4.1)", alias)
	}
	return nil
}

// ResolveAlias returns the node ID an alias points at.
func (s *Store) ResolveAlias(ctx context.Context, alias string) (identity.NodeID, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT node_id FROM aliases WHERE alias = ?`, strings.TrimSpace(alias)).Scan(&raw)
	if isNoRows(err) {
		return identity.NodeID{}, ErrNotFound
	}
	if err != nil {
		return identity.NodeID{}, fmt.Errorf("resolve alias: %w", err)
	}
	var id identity.NodeID
	copy(id[:], raw)
	return id, nil
}

// RemoveAlias deletes an alias.
func (s *Store) RemoveAlias(ctx context.Context, alias string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM aliases WHERE alias = ?`, strings.TrimSpace(alias))
	if err != nil {
		return fmt.Errorf("remove alias: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Alias pairs a petname with its target.
type Alias struct {
	Alias  string
	NodeID identity.NodeID
}

// ListAliases returns all aliases, ordered for deterministic output.
func (s *Store) ListAliases(ctx context.Context) ([]Alias, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT alias, node_id FROM aliases ORDER BY alias`)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()

	var out []Alias
	for rows.Next() {
		var a Alias
		var raw []byte
		if err := rows.Scan(&a.Alias, &raw); err != nil {
			return nil, err
		}
		copy(a.NodeID[:], raw)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveNodeRef resolves either a literal node ID or a local alias.
//
// This implements the §6.1.4.1 resolution rule. The literal form is tried
// first, so a valid 13-character Crockford string is never treated as an alias
// — and ValidateAlias refuses to create aliases that look like IDs, so the two
// namespaces cannot collide.
func (s *Store) ResolveNodeRef(ctx context.Context, ref string) (identity.NodeID, error) {
	ref = strings.TrimSpace(ref)
	if id, err := identity.ParseNodeID(ref); err == nil {
		return id, nil
	}
	id, err := s.ResolveAlias(ctx, ref)
	if err == ErrNotFound {
		return identity.NodeID{}, fmt.Errorf(
			"no node known as %q — it is neither a node ID nor an alias on this BBS. "+
				"Ask your sysop to add it (`meshbbs peer alias %s <node-id>`), or use the full ID",
			ref, ref)
	}
	return id, err
}
