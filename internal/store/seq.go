package store

import (
	"context"
	"fmt"
	"os"
)

// ensureDir creates a directory with restrictive permissions.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}
	return nil
}

// SeqState is the durable sequence high-water mark and incarnation counter.
type SeqState struct {
	HighWater   uint64
	Incarnation uint32
}

// SeqState returns the current state.
func (s *Store) SeqState(ctx context.Context) (SeqState, error) {
	var st SeqState
	err := s.db.QueryRowContext(ctx,
		`SELECT high_water, incarnation FROM seq_state WHERE only_row = 1`).
		Scan(&st.HighWater, &st.Incarnation)
	if err != nil {
		return SeqState{}, fmt.Errorf("read seq state: %w", err)
	}
	return st, nil
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
func (s *Store) NextSeq(ctx context.Context) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin seq transaction: %w", err)
	}
	defer tx.Rollback()

	var high uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT high_water FROM seq_state WHERE only_row = 1`).Scan(&high); err != nil {
		return 0, fmt.Errorf("read seq high-water: %w", err)
	}

	next := high + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE seq_state SET high_water = ?, updated_at = ? WHERE only_row = 1`,
		next, s.now()); err != nil {
		return 0, fmt.Errorf("advance seq high-water: %w", err)
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

	var high uint64
	var incarnation uint32
	if err := tx.QueryRowContext(ctx,
		`SELECT high_water, incarnation FROM seq_state WHERE only_row = 1`).
		Scan(&high, &incarnation); err != nil {
		return false, fmt.Errorf("read seq state: %w", err)
	}

	// COALESCE so an empty log yields 0 rather than NULL.
	var maxSeq uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM records WHERE origin = ?`, selfID).
		Scan(&maxSeq); err != nil {
		return false, fmt.Errorf("read max local seq: %w", err)
	}

	if maxSeq <= high {
		return false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE seq_state SET high_water = ?, incarnation = ?, updated_at = ? WHERE only_row = 1`,
		maxSeq, incarnation+1, s.now()); err != nil {
		return false, fmt.Errorf("repair seq high-water: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit seq repair: %w", err)
	}
	return true, nil
}
