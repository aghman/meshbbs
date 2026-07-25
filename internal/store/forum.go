package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/record"
)

// Area is a forum message base (§6.3).
type Area struct {
	ID            int64
	Name          string
	Tag           record.AreaTag
	Description   string
	Federated     bool
	ReadOnly      bool
	RetentionDays int
	CreatedAt     int64
}

// Scope returns a human label for the area's reach.
//
// This is the affordance [N7] requires: a user who posts and sees nothing
// federate must never be left guessing, so every area states its scope.
func (a Area) Scope() string {
	if a.Federated {
		return "Federated"
	}
	return "Local to this BBS"
}

// ErrAreaExists is returned when creating a duplicate area.
var ErrAreaExists = errors.New("an area with that name already exists")

// CreateArea creates a forum area.
//
// federated defaults to false at every call site by design (§6.3): sysops opt
// IN to spending the network's airtime. At roughly ten originated packets per
// day per node (§1.1) that default is load-bearing, not a nicety.
func (s *Store) CreateArea(ctx context.Context, name, description string, federated bool) (Area, error) {
	name = strings.TrimSpace(name)
	if err := ValidateAreaName(name); err != nil {
		return Area{}, err
	}
	tag := record.AreaTagFor(name)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO areas (name, tag, description, federated, created_at) VALUES (?, ?, ?, ?, ?)`,
		name, tag[:], description, boolToInt(federated), s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Area{}, fmt.Errorf("%w: %q", ErrAreaExists, name)
		}
		return Area{}, fmt.Errorf("create area: %w", err)
	}
	id, _ := res.LastInsertId()
	return Area{
		ID: id, Name: name, Tag: tag, Description: description,
		Federated: federated, CreatedAt: s.now(),
	}, nil
}

// ValidateAreaName enforces area naming rules.
func ValidateAreaName(name string) error {
	if len(name) < 1 || len(name) > 32 {
		return fmt.Errorf("area name must be 1-32 characters, got %d", len(name))
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("area name contains invalid character %q; use letters, digits, - _ or .", r)
		}
	}
	return nil
}

// GetArea loads an area by name.
func (s *Store) GetArea(ctx context.Context, name string) (Area, error) {
	var a Area
	var tag []byte
	var fed, ro int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, tag, description, federated, read_only, retention_days, created_at
		 FROM areas WHERE name = ?`, strings.TrimSpace(name)).
		Scan(&a.ID, &a.Name, &tag, &a.Description, &fed, &ro, &a.RetentionDays, &a.CreatedAt)
	if isNoRows(err) {
		return Area{}, ErrNotFound
	}
	if err != nil {
		return Area{}, fmt.Errorf("get area: %w", err)
	}
	copy(a.Tag[:], tag)
	a.Federated, a.ReadOnly = fed == 1, ro == 1
	return a, nil
}

// ListAreas returns all areas, ordered by name.
func (s *Store) ListAreas(ctx context.Context) ([]Area, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, tag, description, federated, read_only, retention_days, created_at
		 FROM areas ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list areas: %w", err)
	}
	defer rows.Close()

	var out []Area
	for rows.Next() {
		var a Area
		var tag []byte
		var fed, ro int
		if err := rows.Scan(&a.ID, &a.Name, &tag, &a.Description, &fed, &ro,
			&a.RetentionDays, &a.CreatedAt); err != nil {
			return nil, err
		}
		copy(a.Tag[:], tag)
		a.Federated, a.ReadOnly = fed == 1, ro == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAreaFederated changes whether an area replicates over the mesh.
func (s *Store) SetAreaFederated(ctx context.Context, name string, federated bool, actor string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE areas SET federated = ? WHERE name = ?`, boolToInt(federated), name)
	if err != nil {
		return fmt.Errorf("set area federation: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	detail := "local-only"
	if federated {
		detail = "federated"
	}
	return s.audit(ctx, actor, "area.federate", name, detail)
}

// Post is a forum post as displayed.
type Post struct {
	ID      record.ID
	Area    string
	Author  string
	Subject string
	Body    string
	TS      uint32
	Parent  record.ID
	Origin  string // short node ID of the authoring instance
	Local   bool
}

// PostBody is the wire body of a POST record.
//
// The author's nick travels in the body because records are NODE-signed
// ([D5]): the record says "instance X vouches that user austin posted this".
// Layout is length-prefixed and deterministic — no maps, no field tags.
type PostBody struct {
	Author  string
	Subject string
	Text    string
}

// MarshalPostBody encodes a post body deterministically.
func MarshalPostBody(p PostBody) ([]byte, error) {
	if len(p.Author) > 16 {
		return nil, fmt.Errorf("author is %d bytes, limit is 16", len(p.Author))
	}
	if len(p.Subject) > 72 {
		return nil, fmt.Errorf("subject is %d bytes, limit is 72", len(p.Subject))
	}
	out := make([]byte, 0, 2+len(p.Author)+len(p.Subject)+len(p.Text))
	out = append(out, byte(len(p.Author)))
	out = append(out, p.Author...)
	out = append(out, byte(len(p.Subject)))
	out = append(out, p.Subject...)
	out = append(out, p.Text...)
	return out, nil
}

// UnmarshalPostBody decodes a post body.
func UnmarshalPostBody(b []byte) (PostBody, error) {
	var p PostBody
	if len(b) < 2 {
		return p, record.ErrTruncated
	}
	n := int(b[0])
	if len(b) < 1+n+1 {
		return p, record.ErrTruncated
	}
	p.Author = string(b[1 : 1+n])
	rest := b[1+n:]
	m := int(rest[0])
	if len(rest) < 1+m {
		return p, record.ErrTruncated
	}
	p.Subject = string(rest[1 : 1+m])
	p.Text = string(rest[1+m:])
	return p, nil
}

// IndexPost records the local authorship index for a stored POST record.
func (s *Store) IndexPost(ctx context.Context, id record.ID, author, subject string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO post_authors (record_id, author, subject) VALUES (?, ?, ?)
		 ON CONFLICT(record_id) DO NOTHING`, id[:], author, subject)
	if err != nil {
		return fmt.Errorf("index post: %w", err)
	}
	return nil
}

// ListPosts returns posts in an area, newest last.
func (s *Store) ListPosts(ctx context.Context, areaName string, limit int) ([]Post, error) {
	area, err := s.GetArea(ctx, areaName)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 100
	}

	// Resolve our own node ID BEFORE opening the result set below.
	//
	// The pool is capped at a single connection (see Open), so any query
	// issued while `rows` is still open waits for a connection that only
	// closing `rows` can release — a deadlock, not a slow path. Every method
	// here must finish its lookups before it starts streaming rows.
	self, selfErr := s.SelfNode(ctx)

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.ts, r.parent, r.origin, r.body, COALESCE(a.author, ''), COALESCE(a.subject, '')
		FROM records r
		LEFT JOIN post_authors a ON a.record_id = r.id
		WHERE r.area = ? AND r.type = ?
		ORDER BY r.ts, r.seq
		LIMIT ?`, area.Tag[:], uint8(record.TypePost), limit)
	if err != nil {
		return nil, fmt.Errorf("list posts: %w", err)
	}
	defer rows.Close()

	var out []Post
	for rows.Next() {
		var p Post
		var id, parent, origin, body []byte
		if err := rows.Scan(&id, &p.TS, &parent, &origin, &body, &p.Author, &p.Subject); err != nil {
			return nil, err
		}
		copy(p.ID[:], id)
		if len(parent) == record.IDLen {
			copy(p.Parent[:], parent)
		}
		p.Area = area.Name

		if pb, err := UnmarshalPostBody(body); err == nil {
			p.Body = pb.Text
			if p.Author == "" {
				p.Author = pb.Author
			}
			if p.Subject == "" {
				p.Subject = pb.Subject
			}
		}
		if selfErr == nil && len(origin) == 8 {
			p.Local = string(origin) == string(self.ID[:])
			var oid [8]byte
			copy(oid[:], origin)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPosts returns how many posts an area holds.
func (s *Store) CountPosts(ctx context.Context, areaName string) (int, error) {
	area, err := s.GetArea(ctx, areaName)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE area = ? AND type = ?`,
		area.Tag[:], uint8(record.TypePost)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count posts: %w", err)
	}
	return n, nil
}
