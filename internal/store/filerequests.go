package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// MaxOpenFileRequests bounds how many outstanding requests one user may hold.
//
// A request is not a download. It is a claim on somebody's USB stick, on the
// disk of a board that has not agreed to anything, and on a car journey that
// may be a week away — so "queue the whole catalog and see what turns up" has
// to be unavailable, and a small number is what makes a sysop's `--files`
// export a thing they can look at and understand.
//
// Sixteen rather than a rounder number for no reason beyond it being clearly
// more than a person asks for in one sitting and clearly less than a listing.
const MaxOpenFileRequests = 16

var (
	// ErrFileRequestExists is returned when this user has already asked.
	ErrFileRequestExists = errors.New("you have already asked for that file")
	// ErrFileRequestQueueFull is returned at MaxOpenFileRequests.
	ErrFileRequestQueueFull = errors.New("you have too many files on request already")
	// ErrFileAlreadyHeld is returned for content this BBS has.
	ErrFileAlreadyHeld = errors.New("this BBS already holds that file")
)

// FileRequest is one file somebody asked for and cannot fetch (§6.5).
type FileRequest struct {
	ID   int64
	Area string
	// Name is where the bytes get filed here when they land. It does not
	// travel — see migration 0011 on why a request names a hash.
	Name string
	// Hash is the truncated content hash the FILE record announced.
	Hash [record.FileHashLen]byte
	// Holder is the node that announced it, which is advisory: the carrier
	// that answers may be written by a board that merely holds a copy.
	Holder      identity.NodeID
	Nick        string
	RequestedAt int64
	ArrivedAt   int64
	NotifiedAt  int64
	// Note records the one outcome that is neither pending nor clean: the
	// bytes arrived and could not be filed, because the name was taken in the
	// meantime.
	Note string
}

// Arrived reports whether the bytes have landed.
func (r FileRequest) Arrived() bool { return r.ArrivedAt != 0 }

// Filed reports whether the arrival produced a catalog row a user can download.
func (r FileRequest) Filed() bool { return r.Arrived() && r.Note == "" }

// RequestFile queues a file for the next sneakernet exchange (§6.5 path 2).
//
// It does not transmit, does not promise a date, and says so wherever it is
// rendered. What it promises is the thing that is now true and was not before:
// the request is written down, it will ride the next carrier, and the person
// who asked will be told when the bytes arrive.
//
// Three refusals, all of them things a user can act on:
//
//   - Content this node already holds. The answer there is a download, not a
//     week's wait, and queueing it would be busywork the requester never sees
//     the end of.
//   - A second request from the same person for the same file. Asking twice
//     does not make a stick come sooner, and the row would be a duplicate
//     notification rather than a duplicate request — the carrier de-duplicates
//     by hash regardless.
//   - A name already taken in the area by different content. Checked HERE,
//     where the person can pick a moment to sort it out, rather than at
//     arrival, where the only witness is a sysop importing a stick.
func (s *Store) RequestFile(ctx context.Context, areaName, name string, hash [record.FileHashLen]byte, holder identity.NodeID, nick string) (FileRequest, error) {
	area, err := s.GetFileArea(ctx, areaName)
	if err != nil {
		return FileRequest{}, err
	}
	name = strings.TrimSpace(name)
	if err := ValidateFileName(name); err != nil {
		return FileRequest{}, err
	}
	if nick = strings.TrimSpace(nick); nick == "" {
		return FileRequest{}, errors.New("a file request needs an account to notify")
	}
	if hash == ([record.FileHashLen]byte{}) {
		return FileRequest{}, errors.New("that catalog entry carries no content hash")
	}

	held, err := s.HoldsBlob(ctx, hash[:])
	if err != nil {
		return FileRequest{}, err
	}
	if held {
		return FileRequest{}, ErrFileAlreadyHeld
	}

	if existing, err := s.GetFile(ctx, area.Name, name); err == nil {
		trunc, terr := record.TruncateFileHash(existing.Hash[:])
		if terr == nil && trunc == hash {
			// Same name, same content, and HoldsBlob said no: the blobs row is
			// gone from under the catalog row. That is a store to repair, not a
			// file to fetch off a stick.
			return FileRequest{}, ErrFileAlreadyHeld
		}
		return FileRequest{}, fmt.Errorf(
			"%s already holds a different file called %s; it is where this one would land, "+
				"so ask the sysop to rename one of them first", area.Name, name)
	} else if !errors.Is(err, ErrNotFound) {
		return FileRequest{}, err
	}

	var open int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM file_requests WHERE nick = ? AND arrived_at = 0`,
		nick).Scan(&open); err != nil {
		return FileRequest{}, fmt.Errorf("count open file requests: %w", err)
	}
	if open >= MaxOpenFileRequests {
		return FileRequest{}, fmt.Errorf("%w: %d are waiting, and the limit is %d",
			ErrFileRequestQueueFull, open, MaxOpenFileRequests)
	}

	var holderCol any
	if !holder.IsZero() {
		holderCol = holder[:]
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO file_requests (area_id, name, wire_hash, holder, nick, requested_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		area.ID, name, hash[:], holderCol, nick, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return FileRequest{}, fmt.Errorf("%w: %s in %s", ErrFileRequestExists, name, area.Name)
		}
		return FileRequest{}, fmt.Errorf("queue file request: %w", err)
	}

	req := FileRequest{
		Area: area.Name, Name: name, Hash: hash, Holder: holder,
		Nick: nick, RequestedAt: now,
	}
	req.ID, _ = res.LastInsertId()
	return req, nil
}

// fileRequestColumns is the projection every request scan uses.
const fileRequestColumns = `r.id, a.name, r.name, r.wire_hash, r.holder, r.nick,
	r.requested_at, r.arrived_at, r.notified_at, r.note`

func scanFileRequest(sc interface{ Scan(...any) error }) (FileRequest, error) {
	var r FileRequest
	var hash, holder []byte
	if err := sc.Scan(&r.ID, &r.Area, &r.Name, &hash, &holder, &r.Nick,
		&r.RequestedAt, &r.ArrivedAt, &r.NotifiedAt, &r.Note); err != nil {
		return FileRequest{}, err
	}
	copy(r.Hash[:], hash)
	if len(holder) == identity.NodeIDLen {
		copy(r.Holder[:], holder)
	}
	return r, nil
}

// ListFileRequests returns requests oldest first. An empty nick returns
// everyone's, which is the sysop's view: they are the one who carries the stick.
func (s *Store) ListFileRequests(ctx context.Context, nick string) ([]FileRequest, error) {
	query := `SELECT ` + fileRequestColumns + `
	          FROM file_requests r JOIN areas a ON a.id = r.area_id`
	var args []any
	if nick = strings.TrimSpace(nick); nick != "" {
		query += ` WHERE r.nick = ?`
		args = append(args, nick)
	}
	query += ` ORDER BY r.requested_at, r.id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list file requests: %w", err)
	}
	defer rows.Close()

	var out []FileRequest
	for rows.Next() {
		r, err := scanFileRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpenFileRequestHashes returns the distinct hashes still outstanding, oldest
// first, for a carrier to ask about.
//
// Distinct because the wire carries a want, not a wanter: two people asking for
// the same file is one hash on the stick and two people to tell afterwards.
//
// Oldest first so that a queue longer than one carrier drains in the order it
// was written rather than by whatever the database found convenient — somebody
// who asked a month ago should not be overtaken forever by this morning's
// request.
func (s *Store) OpenFileRequestHashes(ctx context.Context, limit int) ([][record.FileHashLen]byte, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT wire_hash FROM file_requests
		WHERE arrived_at = 0
		GROUP BY wire_hash
		ORDER BY MIN(requested_at), MIN(id)
		LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read open file requests: %w", err)
	}
	defer rows.Close()

	var out [][record.FileHashLen]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var h [record.FileHashLen]byte
		copy(h[:], raw)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SatisfyFileRequests files an arrived blob and closes the requests it answers.
//
// # Why this is a store method and not import glue
//
// Two things have to happen together or the result is a lie in one direction or
// the other: a catalog row so the file can actually be downloaded, and the
// request marked answered so nobody is told twice and no carrier asks again. A
// caller that did them separately would, on the failure between them, either
// re-request content it holds or hold content nobody can reach.
//
// The blob's BYTES are already on disk by the time this runs — the carrier
// streamed them straight through blobstore.Put, which is what keeps a 200 MB
// file from being a 200 MB allocation. What is missing is the two rows that
// make it a file rather than an orphan, and PutFile writes both.
//
// Returns the requests it closed, so the sysop importing the stick can say who
// got what. A hash nobody asked for closes nothing and returns nothing: that is
// an ordinary outcome for a carrier written with a blunt `--files`, not an
// error.
func (s *Store) SatisfyFileRequests(ctx context.Context, hash blobstore.Hash, size int64) ([]FileRequest, error) {
	wire, err := record.TruncateFileHash(hash[:])
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+fileRequestColumns+`
		FROM file_requests r JOIN areas a ON a.id = r.area_id
		WHERE r.wire_hash = ? AND r.arrived_at = 0
		ORDER BY r.requested_at, r.id`, wire[:])
	if err != nil {
		return nil, fmt.Errorf("find file requests for an arrival: %w", err)
	}
	var open []FileRequest
	for rows.Next() {
		r, err := scanFileRequest(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		open = append(open, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(open) == 0 {
		return nil, nil
	}

	// One catalog row per (area, name), however many people asked for it. Two
	// users wanting the same content under two names in one area is legal and
	// rare — the catalog can carry the same bytes twice — so this is keyed on
	// both rather than on the area alone.
	filed := map[string]string{}
	now := s.now()
	for i := range open {
		req := &open[i]
		key := req.Area + "/" + req.Name
		note, seen := filed[key]
		if !seen {
			note = s.fileArrival(ctx, *req, hash, size)
			filed[key] = note
		}
		req.ArrivedAt = now
		req.Note = note
		if _, err := s.db.ExecContext(ctx,
			`UPDATE file_requests SET arrived_at = ?, note = ? WHERE id = ?`,
			now, note, req.ID); err != nil {
			return nil, fmt.Errorf("close file request %d: %w", req.ID, err)
		}
	}
	return open, nil
}

// fileArrival writes the catalog row for arrived content, returning the note to
// record if it could not.
//
// # Why an arrival is not announced
//
// The row is written through PutFile rather than the file service, so no FILE
// record is minted and record_id stays NULL. The network already knows about
// this file — the entry we are answering came from the holder's own record —
// and a second origin announcing the same content would spend §1.1's shared
// airtime restating what every peer has. A sysop who wants this board named as
// a second holder can say so; it is not a side effect of a stick arriving.
//
// # Why the uploader is empty
//
// Nobody here uploaded it. An empty uploader is already the codebase's word for
// a file with no owner to be (File.MayDescribe), which is exactly right: the
// person who asked for it did not write it, and letting them edit the
// description would put their words on somebody else's file.
func (s *Store) fileArrival(ctx context.Context, req FileRequest, hash blobstore.Hash, size int64) string {
	_, err := s.PutFile(ctx, req.Area, File{
		Name: req.Name, Hash: hash, Size: size, UploadedAt: s.now(),
	})
	if err == nil {
		return ""
	}
	if !errors.Is(err, ErrFileExists) {
		return fmt.Sprintf("arrived, but filing it in %s failed: %v", req.Area, err)
	}

	// The name was taken between the request and the stick. If it was taken by
	// this very content, the request is answered and there is nothing to say.
	if existing, gerr := s.GetFile(ctx, req.Area, req.Name); gerr == nil && existing.Hash == hash {
		return ""
	}
	// Otherwise the bytes are here and unreachable under the name that was
	// asked for. Recorded rather than retried: asking the other board again
	// would spend a second hand-off on a collision at this end.
	return fmt.Sprintf("arrived, but %s already holds a different file called %s — "+
		"rename that one and the sysop can file this copy", req.Area, req.Name)
}

// UnnotifiedFileRequests returns what has landed for a user and not been
// mentioned to them.
//
// Separate from arrival because the two happen days apart and to different
// people: the bytes land under a sysop's hands at a command line, and the
// person who asked is on an SSH session that has not started yet (§6.5).
func (s *Store) UnnotifiedFileRequests(ctx context.Context, nick string) ([]FileRequest, error) {
	if nick = strings.TrimSpace(nick); nick == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+fileRequestColumns+`
		FROM file_requests r JOIN areas a ON a.id = r.area_id
		WHERE r.nick = ? AND r.arrived_at != 0 AND r.notified_at = 0
		ORDER BY r.arrived_at, r.id`, nick)
	if err != nil {
		return nil, fmt.Errorf("read arrived file requests: %w", err)
	}
	defer rows.Close()

	var out []FileRequest
	for rows.Next() {
		r, err := scanFileRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkFileRequestsNotified records that the user has been told.
func (s *Store) MarkFileRequestsNotified(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := s.now()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`UPDATE file_requests SET notified_at = ? WHERE id = ? AND arrived_at != 0`,
			now, id); err != nil {
			return fmt.Errorf("mark file request %d notified: %w", id, err)
		}
	}
	return tx.Commit()
}

// CancelFileRequest withdraws a request.
//
// Scoped to the nick that made it, because the row is per-person: a sysop
// cancelling on somebody's behalf would look identical to the file never having
// been asked for, and there is nothing here worth that ambiguity. An arrived
// request is still cancellable — the row is then only a notice, and clearing an
// old notice is the user's business.
func (s *Store) CancelFileRequest(ctx context.Context, id int64, nick string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM file_requests WHERE id = ? AND nick = ?`, id, strings.TrimSpace(nick))
	if err != nil {
		return fmt.Errorf("cancel file request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
