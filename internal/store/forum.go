package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/record"
)

// Area is a federatable namespace: a forum message base (§6.3) or a file area
// (§6.5).
//
// Both kinds share this table and this type because at the protocol layer they
// are the same thing — an AreaTag with a version vector — and because the tag
// namespace is global, so letting the two kinds be named independently would
// let a message area and a file area collide into one tag. See migration 0005.
type Area struct {
	ID            int64
	Name          string
	Tag           record.AreaTag
	Kind          AreaKind
	Description   string
	Federated     bool
	ReadOnly      bool
	RetentionDays int
	CreatedAt     int64
}

// AreaKind says what an area holds.
type AreaKind string

const (
	// KindMessage is a forum message base (§6.3).
	KindMessage AreaKind = "message"
	// KindFile is a file area (§6.5).
	KindFile AreaKind = "file"
)

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
	return s.createArea(ctx, name, description, federated, KindMessage)
}

// CreateFileArea creates a file area (§6.5).
//
// Same opt-in rule as a message area, and for a sharper reason: a file area's
// records are the lowest priority class the governor has (§7.6), so a federated
// one that nobody wanted is airtime spent on catalog entries at the expense of
// everything above it.
func (s *Store) CreateFileArea(ctx context.Context, name, description string, federated bool) (Area, error) {
	return s.createArea(ctx, name, description, federated, KindFile)
}

func (s *Store) createArea(ctx context.Context, name, description string, federated bool, kind AreaKind) (Area, error) {
	name = strings.TrimSpace(name)
	if err := ValidateAreaName(name); err != nil {
		return Area{}, err
	}
	tag := record.AreaTagFor(name)

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO areas (name, tag, description, federated, kind, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, tag[:], description, boolToInt(federated), string(kind), s.now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			// Deliberately does not say which KIND of area already holds the
			// name. A file area and a message area cannot share one, because
			// they would share an AreaTag (migration 0005), and a sysop hitting
			// this needs to know the name is taken, not to go hunting.
			return Area{}, fmt.Errorf("%w: %q", ErrAreaExists, name)
		}
		return Area{}, fmt.Errorf("create area: %w", err)
	}
	id, _ := res.LastInsertId()
	return Area{
		ID: id, Name: name, Tag: tag, Kind: kind, Description: description,
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
	// The well-known tags are not a namespace a forum may join. A forum whose
	// name derives one of them would share a sequence space with the roster,
	// the directory or private mail, and — worse for the last of those — a
	// federated area sharing the mail tag is exactly the leak record.DMArea
	// exists to prevent. Compared by TAG rather than by name because the tag is
	// what collides.
	tag := record.AreaTagFor(name)
	for _, reserved := range []struct {
		tag  record.AreaTag
		what string
	}{
		{RosterArea, "the node roster"},
		{record.ProfileArea, "the user directory"},
		{record.DMArea, "private mail"},
	} {
		if tag == reserved.tag {
			return fmt.Errorf("area name %q is reserved for %s", name, reserved.what)
		}
	}
	return nil
}

// areaColumns is the projection every area scan uses, so the SELECT list and
// scanArea cannot drift apart.
const areaColumns = `id, name, tag, kind, description, federated, read_only, retention_days, created_at`

// scanArea reads one area row from the areaColumns projection.
func scanArea(sc interface{ Scan(...any) error }) (Area, error) {
	var a Area
	var tag []byte
	var kind string
	var fed, ro int
	if err := sc.Scan(&a.ID, &a.Name, &tag, &kind, &a.Description, &fed, &ro,
		&a.RetentionDays, &a.CreatedAt); err != nil {
		return Area{}, err
	}
	copy(a.Tag[:], tag)
	a.Kind = AreaKind(kind)
	a.Federated, a.ReadOnly = fed == 1, ro == 1
	return a, nil
}

// GetArea loads a MESSAGE area by name.
//
// Kind-scoped rather than generic, because the callers are the forum paths and
// a file area reaching one of them would mean posting into a catalog. Use
// GetFileArea for the other kind, or GetAnyArea when the kind is the question.
func (s *Store) GetArea(ctx context.Context, name string) (Area, error) {
	a, err := s.GetAnyArea(ctx, name)
	if err != nil {
		return Area{}, err
	}
	if a.Kind != KindMessage {
		return Area{}, fmt.Errorf("%w: %s is a file area", ErrWrongAreaKind, a.Name)
	}
	return a, nil
}

// GetFileArea loads a FILE area by name.
func (s *Store) GetFileArea(ctx context.Context, name string) (Area, error) {
	a, err := s.GetAnyArea(ctx, name)
	if err != nil {
		return Area{}, err
	}
	if a.Kind != KindFile {
		return Area{}, fmt.Errorf("%w: %s is a message area", ErrWrongAreaKind, a.Name)
	}
	return a, nil
}

// ErrWrongAreaKind is returned when an area exists but holds the other kind of
// thing. Distinct from ErrNotFound because the remedies differ: one is a typo,
// the other is a name already spent on something else.
var ErrWrongAreaKind = errors.New("area is of the wrong kind")

// GetAnyArea loads an area of either kind.
func (s *Store) GetAnyArea(ctx context.Context, name string) (Area, error) {
	a, err := scanArea(s.db.QueryRowContext(ctx,
		`SELECT `+areaColumns+` FROM areas WHERE name = ?`, strings.TrimSpace(name)))
	if isNoRows(err) {
		return Area{}, ErrNotFound
	}
	if err != nil {
		return Area{}, fmt.Errorf("get area: %w", err)
	}
	return a, nil
}

// ListAreas returns the message areas, ordered by name.
func (s *Store) ListAreas(ctx context.Context) ([]Area, error) {
	return s.listAreas(ctx, KindMessage)
}

// ListFileAreas returns the file areas, ordered by name.
func (s *Store) ListFileAreas(ctx context.Context) ([]Area, error) {
	return s.listAreas(ctx, KindFile)
}

func (s *Store) listAreas(ctx context.Context, kind AreaKind) ([]Area, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+areaColumns+` FROM areas WHERE kind = ? ORDER BY name`, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list areas: %w", err)
	}
	defer rows.Close()

	var out []Area
	for rows.Next() {
		a, err := scanArea(rows)
		if err != nil {
			return nil, err
		}
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
