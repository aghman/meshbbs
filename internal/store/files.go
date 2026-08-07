package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/blobstore"
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

// MaxFileNameLen bounds a file's name. It matches the schema CHECK, and it is
// generous next to MaxNickLen because a filename is not paid for per DM — but
// it is still bounded, because the name travels in every FILE record.
const MaxFileNameLen = 64

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
