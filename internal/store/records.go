package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// ErrSeqConflict is returned when a record would occupy an (origin, seq)
// coordinate already held by different content. This is the silent-divergence
// case from §6.2.1 rule 3 caught at the moment it would happen — it is always
// worth an alert, never a silent overwrite.
var ErrSeqConflict = errors.New("a different record already occupies this (origin, seq) coordinate")

// PutRecord stores a record. It is idempotent: storing the same record twice
// is a no-op, which matters because the mesh floods and the same record
// arrives by several paths (§6.2).
//
// The caller must have verified the signature first. PutRecord persists the
// signed bytes verbatim (§6.2.1 rule 1) so later verification never depends on
// re-encoding.
func (s *Store) PutRecord(ctx context.Context, r *record.Record) error {
	id := r.ID()
	origin := r.Origin

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin put: %w", err)
	}
	defer tx.Rollback()

	// Already held? Content addressing makes this exact.
	var existing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id = ?`, id[:]).Scan(&existing); err != nil {
		return fmt.Errorf("check for existing record: %w", err)
	}
	if existing > 0 {
		return nil
	}

	// A different record at the same coordinate is a divergence, not a
	// duplicate. Refuse it loudly.
	//
	// The coordinate is (origin, AREA, seq): sequences are allocated per area
	// (migration 0003), so one origin holding seq 4 in two different areas is
	// ordinary, and only a clash within one area is equivocation.
	var conflicts int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE origin = ? AND area = ? AND seq = ?`,
		origin[:], r.Area[:], r.Seq).Scan(&conflicts); err != nil {
		return fmt.Errorf("check for seq conflict: %w", err)
	}
	if conflicts > 0 {
		return fmt.Errorf("%w: origin %s area %s seq %d", ErrSeqConflict, origin, r.Area, r.Seq)
	}

	var parent any
	if r.HasParent() {
		p := r.Parent
		parent = p[:]
	}

	area := r.Area
	sig := r.Signature()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO records (id, origin, seq, ts, type, area, parent, body, signed, sig, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id[:], origin[:], r.Seq, r.TS, uint8(r.Type), area[:], parent,
		r.Body, r.SignedBytes(), sig, s.now()); err != nil {
		return fmt.Errorf("insert record: %w", err)
	}
	return tx.Commit()
}

// GetRecord loads a record by ID, reconstructing it from the retained signed
// bytes rather than from the decomposed columns (§6.2.1 rule 1).
func (s *Store) GetRecord(ctx context.Context, id record.ID) (*record.Record, error) {
	var signed, sig []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT signed, sig FROM records WHERE id = ?`, id[:]).Scan(&signed, &sig)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load record: %w", err)
	}
	return record.Unmarshal(append(signed, sig...))
}

// HasRecord reports whether a record is already held.
func (s *Store) HasRecord(ctx context.Context, id record.ID) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM records WHERE id = ?`, id[:]).Scan(&n); err != nil {
		return false, fmt.Errorf("check record: %w", err)
	}
	return n > 0, nil
}

// CountRecords returns the total number of stored records.
func (s *Store) CountRecords(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM records`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count records: %w", err)
	}
	return n, nil
}

// HighWaterFor returns the highest contiguous sequence number held for an
// origin — the per-origin component of a version vector (§7.3).
//
// "Contiguous" is the operative word: with gaps at 1,2,4 the answer is 2, not
// 4, because a version vector asserts everything up to N has been received.
// HighWaterFor returns the highest sequence held for an origin in one area.
func (s *Store) HighWaterForArea(ctx context.Context, origin identity.NodeID, area record.AreaTag) (uint64, error) {
	var high uint64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM records WHERE origin = ? AND area = ?`,
		origin[:], area[:]).Scan(&high); err != nil {
		return 0, fmt.Errorf("read area high-water for %s: %w", origin, err)
	}
	return high, nil
}

func (s *Store) HighWaterFor(ctx context.Context, origin identity.NodeID) (uint64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq FROM records WHERE origin = ? ORDER BY seq ASC`, origin[:])
	if err != nil {
		return 0, fmt.Errorf("scan sequences: %w", err)
	}
	defer rows.Close()

	var high uint64
	for rows.Next() {
		var seq uint64
		if err := rows.Scan(&seq); err != nil {
			return 0, err
		}
		if seq != high+1 {
			break
		}
		high = seq
	}
	return high, rows.Err()
}
