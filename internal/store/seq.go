package store

import (
	"context"
	"fmt"
	"os"

	"github.com/aghman/meshbbs/internal/record"
)

// ensureDir creates a directory with restrictive permissions.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return nil
}

// SeqState is this node's incarnation counter and, for compatibility with
// callers that still want one number, the highest sequence it has issued in any
// area.
//
// The incarnation is genuinely global: §6.2.1 rule 3 uses it to tell peers that
// this node's history needs re-verification, which is a statement about the log
// as a whole and not about one area.
type SeqState struct {
	HighWater   uint64
	Incarnation uint32
}

// SeqState returns the current state.
func (s *Store) SeqState(ctx context.Context) (SeqState, error) {
	var st SeqState
	if err := s.db.QueryRowContext(ctx,
		`SELECT incarnation FROM seq_state WHERE only_row = 1`).Scan(&st.Incarnation); err != nil {
		return SeqState{}, fmt.Errorf("read seq state: %w", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(high_water), 0) FROM area_seq_state`).Scan(&st.HighWater); err != nil {
		return SeqState{}, fmt.Errorf("read area high-water marks: %w", err)
	}
	return st, nil
}

// AreaHighWater returns the highest sequence issued in one area.
func (s *Store) AreaHighWater(ctx context.Context, area record.AreaTag) (uint64, error) {
	var high uint64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(high_water, 0) FROM area_seq_state WHERE area = ?`, area[:]).Scan(&high)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read area high-water: %w", err)
	}
	return high, nil
}

// NextSeq durably reserves and returns the next sequence number for records
// this node authors.
//
// This is the enforcement point for §6.2.1 rule 3, and the ordering matters:
// the high-water mark is committed BEFORE the caller may publish a record using
// the number. Doing it the other way round means a crash between publishing and
// recording lets the number be reused with different content — and because a
// peer that has seen seq <= N will never request N again, that divergence is
// permanent and completely silent. Burning a sequence number on a crash is
// free; reusing one is unrecoverable.
func (s *Store) NextSeq(ctx context.Context, area record.AreaTag) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin seq transaction: %w", err)
	}
	defer tx.Rollback()

	var high uint64
	err = tx.QueryRowContext(ctx,
		`SELECT high_water FROM area_seq_state WHERE area = ?`, area[:]).Scan(&high)
	if err != nil && !isNoRows(err) {
		return 0, fmt.Errorf("read area seq high-water: %w", err)
	}

	next := high + 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO area_seq_state (area, high_water, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(area) DO UPDATE SET high_water = excluded.high_water, updated_at = excluded.updated_at`,
		area[:], next, s.now()); err != nil {
		return 0, fmt.Errorf("advance area seq high-water: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit seq advance: %w", err)
	}
	return next, nil
}

// CheckSeqIntegrity verifies that the durable high-water mark is at least as
// large as the highest sequence number actually present in our own log.
//
// A high-water mark BELOW our stored records means the database was restored
// from a backup taken before those records were written, or the seq_state row
// was rolled back independently. Either way we are at risk of reissuing
// sequence numbers that peers have already accepted with different content.
//
// The response is to repair the high-water mark and bump the incarnation
// counter, which is the signal peers use to know this origin's log needs
// re-verification rather than assuming continuity (§6.2.1 rule 3).
//
// It returns true if a regression was detected and repaired.
func (s *Store) CheckSeqIntegrity(ctx context.Context, selfID []byte) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin integrity check: %w", err)
	}
	defer tx.Rollback()

	// Every area is checked, because a restore can leave any one of them
	// behind. One regression anywhere is enough to make this node's log
	// untrustworthy to peers, so the incarnation bumps once for all of them
	// rather than once each.
	rows, err := tx.QueryContext(ctx,
		`SELECT r.area, MAX(r.seq) FROM records r WHERE r.origin = ? GROUP BY r.area`, selfID)
	if err != nil {
		return false, fmt.Errorf("read local sequences by area: %w", err)
	}
	type behind struct {
		area []byte
		max  uint64
	}
	var regressed []behind
	for rows.Next() {
		var area []byte
		var maxSeq uint64
		if err := rows.Scan(&area, &maxSeq); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan local sequence: %w", err)
		}
		regressed = append(regressed, behind{area: area, max: maxSeq})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate local sequences: %w", err)
	}

	var repaired []behind
	for _, b := range regressed {
		var high uint64
		err := tx.QueryRowContext(ctx,
			`SELECT high_water FROM area_seq_state WHERE area = ?`, b.area).Scan(&high)
		if err != nil && !isNoRows(err) {
			return false, fmt.Errorf("read area high-water: %w", err)
		}
		if b.max > high {
			repaired = append(repaired, b)
		}
	}
	if len(repaired) == 0 {
		return false, nil
	}

	for _, b := range repaired {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO area_seq_state (area, high_water, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(area) DO UPDATE SET high_water = excluded.high_water, updated_at = excluded.updated_at`,
			b.area, b.max, s.now()); err != nil {
			return false, fmt.Errorf("repair area high-water: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE seq_state SET incarnation = incarnation + 1, updated_at = ? WHERE only_row = 1`,
		s.now()); err != nil {
		return false, fmt.Errorf("bump incarnation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit seq repair: %w", err)
	}
	return true, nil
}
