package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// File is one named entry in a file area (§6.5).
//
// It describes a file THIS node holds: the bytes are in the blob store and the
// hash is the local 32-byte address, not the truncation that travels on the
// wire. A file held only by some other BBS has no row here — it exists as a
// FILE record in the log, and the catalog view is the join of the two.
type File struct {
	ID          int64
	Area        string
	Name        string
	Hash        blobstore.Hash
	Size        int64
	Description string
	Uploader    string
	UploadedAt  int64

	// Record is the FILE record announcing this file, or the zero ID if it has
	// not been published. Local-only areas never publish, so zero is the normal
	// state there rather than a pending one.
	Record record.ID
}

// Published reports whether a FILE record has been minted for this file.
func (f File) Published() bool { return !f.Record.IsZero() }

// ErrFileExists is returned when an area already holds that name.
var ErrFileExists = errors.New("a file with that name already exists in this area")

// MaxFileNameLen bounds a file's name.
//
// Defined by the wire format rather than here, and re-exported so callers in
// this package read one name. Two constants for one limit is two truths that
// drift: the looser one wins at the database and the tighter one then refuses
// to publish what was accepted, which surfaces as a file that exists locally
// and silently never reaches the network.
const MaxFileNameLen = record.MaxFileNameLen

// PutFile records a file this node now holds.
//
// The blob row and the catalog row are written in one transaction. Half of this
// pair is worse than neither: a catalog row whose blob is unknown would offer a
// download that cannot be served, and a blob row with no catalog entry would be
// bytes nothing can reach or garbage-collect.
//
// The caller writes the BYTES first, through the blob store, and calls this
// with the hash it got back. That order is deliberate — if this call fails, the
// worst outcome is an unreferenced blob on disk, which a maintenance pass can
// find and remove. The other order risks a row pointing at content that was
// never written.
func (s *Store) PutFile(ctx context.Context, areaName string, f File) (File, error) {
	area, err := s.GetFileArea(ctx, areaName)
	if err != nil {
		return File{}, err
	}
	f.Name = strings.TrimSpace(f.Name)
	if err := ValidateFileName(f.Name); err != nil {
		return File{}, err
	}
	if f.Hash.IsZero() {
		return File{}, errors.New("file has no content hash")
	}
	if f.Size < 0 {
		return File{}, fmt.Errorf("file size is negative (%d)", f.Size)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return File{}, err
	}
	defer tx.Rollback()

	// The blob may already be here: that is the dedup case, and it is the
	// common one for a file uploaded to a second area.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO blobs (hash, size, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(hash) DO NOTHING`,
		f.Hash[:], f.Size, s.now()); err != nil {
		return File{}, fmt.Errorf("record blob: %w", err)
	}

	uploadedAt := f.UploadedAt
	if uploadedAt == 0 {
		uploadedAt = s.now()
	}
	var recordID any
	if f.Published() {
		id := f.Record
		recordID = id[:]
	}

	res, err := tx.ExecContext(ctx,
		`INSERT INTO files (area_id, name, hash, description, uploader, uploaded_at, record_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		area.ID, f.Name, f.Hash[:], f.Description, f.Uploader, uploadedAt, recordID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return File{}, fmt.Errorf("%w: %s in %s", ErrFileExists, f.Name, area.Name)
		}
		return File{}, fmt.Errorf("record file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return File{}, err
	}

	f.ID, _ = res.LastInsertId()
	f.Area = area.Name
	f.UploadedAt = uploadedAt
	return f, nil
}

// ValidateFileName enforces file naming rules.
//
// Stricter than a filesystem's rules on purpose. This name reaches three places
// that are not a local directory: a FILE record on the wire, an SFTP path, and
// a terminal drawing a listing. Path separators and control characters are
// refused here so no later layer has to think about them.
func ValidateFileName(name string) error {
	if len(name) < 1 || len(name) > MaxFileNameLen {
		return fmt.Errorf("file name must be 1-%d characters, got %d", MaxFileNameLen, len(name))
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%q is not a file name", name)
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			return fmt.Errorf("file name may not contain %q", r)
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("file name contains a control character")
		}
	}
	return nil
}

// fileColumns is the projection every file scan uses.
const fileColumns = `f.id, a.name, f.name, f.hash, b.size, f.description, f.uploader,
	f.uploaded_at, COALESCE(f.record_id, x'')`

func scanFile(sc interface{ Scan(...any) error }) (File, error) {
	var f File
	var hash, recordID []byte
	if err := sc.Scan(&f.ID, &f.Area, &f.Name, &hash, &f.Size, &f.Description,
		&f.Uploader, &f.UploadedAt, &recordID); err != nil {
		return File{}, err
	}
	copy(f.Hash[:], hash)
	if len(recordID) == record.IDLen {
		copy(f.Record[:], recordID)
	}
	return f, nil
}

// ListFiles returns the files an area holds, newest last.
func (s *Store) ListFiles(ctx context.Context, areaName string) ([]File, error) {
	area, err := s.GetFileArea(ctx, areaName)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+fileColumns+`
		 FROM files f
		 JOIN areas a ON a.id = f.area_id
		 JOIN blobs b ON b.hash = f.hash
		 WHERE f.area_id = ?
		 ORDER BY f.uploaded_at, f.name`, area.ID)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFile loads one file by area and name.
func (s *Store) GetFile(ctx context.Context, areaName, name string) (File, error) {
	area, err := s.GetFileArea(ctx, areaName)
	if err != nil {
		return File{}, err
	}
	f, err := scanFile(s.db.QueryRowContext(ctx,
		`SELECT `+fileColumns+`
		 FROM files f
		 JOIN areas a ON a.id = f.area_id
		 JOIN blobs b ON b.hash = f.hash
		 WHERE f.area_id = ? AND f.name = ?`, area.ID, strings.TrimSpace(name)))
	if isNoRows(err) {
		return File{}, ErrNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("get file: %w", err)
	}
	return f, nil
}

// RenameFile moves a catalog entry, possibly into another area.
//
// The bytes do not move: the blob is addressed by content, and a rename changes
// nothing about the content. This is the clearest payoff of content addressing
// in the whole design — moving a 40 MB file between areas is an UPDATE.
//
// A renamed file loses its FILE record. The record announced a name in an area,
// and both may have just changed; re-announcing is the publisher's job, not
// something to fake by leaving a stale record attached.
func (s *Store) RenameFile(ctx context.Context, fromArea, fromName, toArea, toName string) error {
	src, err := s.GetFile(ctx, fromArea, fromName)
	if err != nil {
		return err
	}
	dst, err := s.GetFileArea(ctx, toArea)
	if err != nil {
		return err
	}
	toName = strings.TrimSpace(toName)
	if err := ValidateFileName(toName); err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE files SET area_id = ?, name = ?, record_id = NULL WHERE id = ?`,
		dst.ID, toName, src.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("%w: %s in %s", ErrFileExists, toName, dst.Name)
		}
		return fmt.Errorf("rename file: %w", err)
	}
	return nil
}

// HoldsBlob reports whether any file area holds this content.
//
// This is the question the network-wide catalog asks once per row: a FILE
// record whose hash is held here offers a download, and one that is not shows
// who does hold it (§6.5). It matches on the truncated wire hash as a prefix,
// because that is all a remote record carries.
func (s *Store) HoldsBlob(ctx context.Context, wireHash []byte) (bool, error) {
	if len(wireHash) == 0 {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blobs WHERE substr(hash, 1, ?) = ?`,
		len(wireHash), wireHash).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check for blob: %w", err)
	}
	return n > 0, nil
}

// SetFileRecord attaches the FILE record minted for a file.
func (s *Store) SetFileRecord(ctx context.Context, fileID int64, id record.ID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE files SET record_id = ? WHERE id = ?`, id[:], fileID)
	if err != nil {
		return fmt.Errorf("attach file record: %w", err)
	}
	return nil
}

// RemoveFile deletes a catalog entry and reports whether its blob is now
// unreferenced.
//
// It does NOT delete the bytes. Deciding that is the caller's, because the blob
// store and the database are two systems and only the caller knows whether a
// failure between them is recoverable. Returning the answer rather than acting
// on it keeps the transaction here honest: the row is gone or it is not.
func (s *Store) RemoveFile(ctx context.Context, areaName, name string) (orphaned bool, hash blobstore.Hash, err error) {
	f, err := s.GetFile(ctx, areaName, name)
	if err != nil {
		return false, blobstore.Hash{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, blobstore.Hash{}, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, f.ID); err != nil {
		return false, blobstore.Hash{}, fmt.Errorf("remove file: %w", err)
	}

	// Derived, not stored — see migration 0005 on why there is no refcount.
	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE hash = ?`, f.Hash[:]).Scan(&remaining); err != nil {
		return false, blobstore.Hash{}, err
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE hash = ?`, f.Hash[:]); err != nil {
			return false, blobstore.Hash{}, fmt.Errorf("remove blob row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, blobstore.Hash{}, err
	}
	return remaining == 0, f.Hash, nil
}

// CountFiles returns how many files an area holds.
func (s *Store) CountFiles(ctx context.Context, areaName string) (int, error) {
	area, err := s.GetFileArea(ctx, areaName)
	if err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE area_id = ?`, area.ID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count files: %w", err)
	}
	return n, nil
}

// CatalogEntry is one file known to the network (§6.5).
//
// It comes from the RECORD LOG, not the files table: the log is what peers
// replicate, so a file held by another BBS appears here and nowhere else. The
// same relationship ListPosts has to POST records — the log is the catalog, and
// the local tables say only what this node happens to hold.
type CatalogEntry struct {
	Name        string
	Size        int64
	Description string

	// Origin is the BBS that announced the file, which is also the BBS holding
	// it. There is no separate "held by" field on the wire because this IS it.
	Origin identity.NodeID

	// Local reports whether this node announced the entry.
	Local bool

	// Held reports whether this node has the content, matched on the truncated
	// wire hash. False is the ordinary case for a peer's file, not an error:
	// mesh replicates catalogs and never bytes (§7.5).
	Held bool

	// Holder is the friendliest name for Origin: the sysop's local petname if
	// there is one, else the node's own display name, else nothing — in which
	// case a caller falls back to the ID. Resolved here rather than per row by
	// the UI, which would be one query per file.
	Holder string

	// HolderContact is the holding sysop's contact string from their NODE
	// record, when they published one. It is the only actionable thing a user
	// has for a file this BBS cannot fetch, so it is worth carrying.
	HolderContact string

	// Uploader is the local nick that put the file here, and is empty for a
	// peer's file. A FILE record carries no uploader — the origin node is the
	// attribution the network gets (§6.5) — so this is knowable only for our
	// own.
	Uploader string

	TS int64
}

// ListCatalog returns everything the network has announced in a file area.
func (s *Store) ListCatalog(ctx context.Context, areaName string) ([]CatalogEntry, error) {
	area, err := s.GetFileArea(ctx, areaName)
	if err != nil {
		return nil, err
	}

	// Resolve our own ID BEFORE opening the result set: the pool is capped at
	// one connection, so a query issued while rows are open waits for a
	// connection only closing those rows can release. See ListPosts.
	self, selfErr := s.SelfNode(ctx)

	rows, err := s.db.QueryContext(ctx, `
		SELECT r.origin, r.ts, r.body
		FROM records r
		WHERE r.area = ? AND r.type = ?
		ORDER BY r.ts, r.seq`, area.Tag[:], uint8(record.TypeFile))
	if err != nil {
		return nil, fmt.Errorf("list catalog: %w", err)
	}

	type pending struct {
		entry CatalogEntry
		hash  []byte
	}
	var found []pending
	for rows.Next() {
		var origin, body []byte
		var ts int64
		if err := rows.Scan(&origin, &ts, &body); err != nil {
			rows.Close()
			return nil, err
		}
		// A body we cannot parse is skipped rather than fatal, the same way
		// ListPosts treats a post: one malformed record from one peer must not
		// blank the whole listing.
		fb, err := record.UnmarshalFileBody(body)
		if err != nil {
			continue
		}
		e := CatalogEntry{
			Name: fb.Name, Size: int64(fb.Size), Description: fb.Description, TS: ts,
		}
		if len(origin) == identity.NodeIDLen {
			copy(e.Origin[:], origin)
			e.Local = selfErr == nil && e.Origin == self.ID
		}
		found = append(found, pending{entry: e, hash: append([]byte(nil), fb.Hash[:]...)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}

	// Now the connection is free, ask what we hold.
	out := make([]CatalogEntry, 0, len(found))
	for _, p := range found {
		held, err := s.HoldsBlob(ctx, p.hash)
		if err != nil {
			return nil, err
		}
		p.entry.Held = held
		out = append(out, p.entry)
	}
	return out, nil
}

// ListAreaContents returns everything visible in a file area.
//
// # Why this is not just ListCatalog
//
// The record log is the network's catalog, but it is not the whole picture at
// either end. A LOCAL-only area publishes nothing, so its log entries do not
// exist and the files table is the only source. And a federated area's own
// files appear in both, where the local row knows things the record cannot
// carry — who uploaded it, and the full content hash.
//
// So this merges: every announced entry, plus every local file that has not
// been announced, with local rows enriched from the table. A peer announcing a
// name we also hold produces TWO rows, which is honest rather than tidy — they
// are different files on different BBSes that happen to share a name, and the
// holder column is what tells them apart.
func (s *Store) ListAreaContents(ctx context.Context, areaName string) ([]CatalogEntry, error) {
	entries, err := s.ListCatalog(ctx, areaName)
	if err != nil {
		return nil, err
	}
	local, err := s.ListFiles(ctx, areaName)
	if err != nil {
		return nil, err
	}

	// Enrich our own announced entries, and collect the names so the loop below
	// can tell an unannounced file from one already present.
	announced := map[string]int{}
	for i, e := range entries {
		if e.Local {
			announced[e.Name] = i
		}
	}
	for _, f := range local {
		if i, ok := announced[f.Name]; ok {
			entries[i].Uploader = f.Uploader
			continue
		}
		entries = append(entries, CatalogEntry{
			Name: f.Name, Size: f.Size, Description: f.Description,
			Local: true, Held: true, Uploader: f.Uploader, TS: f.UploadedAt,
		})
	}

	if err := s.labelHolders(ctx, entries); err != nil {
		return nil, err
	}

	// Oldest first, matching every other listing in the BBS.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].TS != entries[j].TS {
			return entries[i].TS < entries[j].TS
		}
		return entries[i].Name < entries[j].Name
	})
	return entries, nil
}

// labelHolders fills in Holder and HolderContact for peer entries.
//
// Two queries for the whole listing rather than two per row: an area with fifty
// files would otherwise be a hundred round trips through a pool capped at one
// connection.
func (s *Store) labelHolders(ctx context.Context, entries []CatalogEntry) error {
	var needed bool
	for _, e := range entries {
		if !e.Local && !e.Origin.IsZero() {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}

	aliases, err := s.ListAliases(ctx)
	if err != nil {
		return err
	}
	byNode := map[identity.NodeID]string{}
	for _, a := range aliases {
		// First alias wins: ListAliases orders by name, so this is stable
		// rather than dependent on which row the database returned first.
		if _, seen := byNode[a.NodeID]; !seen {
			byNode[a.NodeID] = a.Alias
		}
	}

	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return err
	}
	info := map[identity.NodeID]Node{}
	for _, n := range nodes {
		info[n.ID] = n
	}

	for i := range entries {
		if entries[i].Local || entries[i].Origin.IsZero() {
			continue
		}
		n := info[entries[i].Origin]
		entries[i].HolderContact = n.SysopContact
		if alias, ok := byNode[entries[i].Origin]; ok {
			entries[i].Holder = alias
			continue
		}
		entries[i].Holder = n.DisplayName
	}
	return nil
}
