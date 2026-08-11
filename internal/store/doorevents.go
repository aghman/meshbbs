package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

// MaxQueuedDoorEvents bounds how many un-flushed events one door may hold.
//
// A queue with no ceiling is a disk-filling primitive handed to a third-party
// binary, which is not a thing §9.4's threat model should contain. The number
// is generous next to what the budget can actually carry — a league area's
// share is on the order of one packet a day, so a hundred queued events is
// already far more backlog than the mesh will clear — and reaching it means the
// door is emitting faster than the mesh can ever drain, which is worth telling
// the door about rather than absorbing.
const MaxQueuedDoorEvents = 100

// ErrDoorEventQueueFull is returned when a door has reached that ceiling.
var ErrDoorEventQueueFull = errors.New("this door has more queued events than the mesh can carry")

// QueuedDoorEvent is one event waiting to be batched into a DOOR_EVENT record.
type QueuedDoorEvent struct {
	ID         int64
	Door       string
	Area       string
	Game       string
	Kind       uint8
	Actor      string
	Target     string
	TargetNode identity.NodeID
	Payload    []byte
	QueuedAt   int64
}

// QueueDoorEvent records one event for the next batch (§9.5).
//
// It does not transmit and does not promise to. §6.5 is emphatic that printing
// "queued for next exchange" when nothing will satisfy the request is worse
// than a spinner, so what this returns is the truth available at the time: the
// event is recorded, and the flusher will batch it when the area's share of the
// budget allows.
func (s *Store) QueueDoorEvent(ctx context.Context, ev QueuedDoorEvent) error {
	var queued int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_event_queue WHERE door = ?`, ev.Door).Scan(&queued); err != nil {
		return fmt.Errorf("count queued door events: %w", err)
	}
	if queued >= MaxQueuedDoorEvents {
		return fmt.Errorf("%w: %d waiting", ErrDoorEventQueueFull, queued)
	}

	var node any
	if ev.Target != "" {
		node = ev.TargetNode[:]
	}
	// A nil slice binds as SQL NULL, and the column is NOT NULL. An absent
	// payload is an EMPTY payload here, matching the wire, where absent and
	// empty are the same single zero length byte.
	payload := ev.Payload
	if payload == nil {
		payload = []byte{}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO door_event_queue
		    (door, area, game, kind, actor, target, target_node, payload, queued_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.Door, ev.Area, ev.Game, int(ev.Kind), ev.Actor, ev.Target, node,
		payload, s.now())
	if err != nil {
		return fmt.Errorf("queue door event: %w", err)
	}
	return nil
}

// QueuedDoorEvents returns everything waiting, oldest first.
//
// Ordered by id rather than queued_at because two events in the same second are
// normal — a door reports a fight as several events at once — and the flusher
// has to put them on the wire in the order they happened.
func (s *Store) QueuedDoorEvents(ctx context.Context, area, game string) ([]QueuedDoorEvent, error) {
	query := `SELECT id, door, area, game, kind, actor, target, target_node, payload, queued_at
	          FROM door_event_queue`
	var args []any
	if area != "" {
		query += ` WHERE area = ? AND game = ?`
		args = append(args, area, game)
	}
	query += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read queued door events: %w", err)
	}
	defer rows.Close()

	var out []QueuedDoorEvent
	for rows.Next() {
		var ev QueuedDoorEvent
		var node []byte
		var kind int
		if err := rows.Scan(&ev.ID, &ev.Door, &ev.Area, &ev.Game, &kind,
			&ev.Actor, &ev.Target, &node, &ev.Payload, &ev.QueuedAt); err != nil {
			return nil, err
		}
		ev.Kind = uint8(kind)
		if len(node) == identity.NodeIDLen {
			copy(ev.TargetNode[:], node)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// CountQueuedDoorEvents reports how many events are waiting for a door.
func (s *Store) CountQueuedDoorEvents(ctx context.Context, door string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM door_event_queue WHERE door = ?`, door).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count queued door events: %w", err)
	}
	return n, nil
}

// DeleteQueuedDoorEvents removes events by id, after they have been sent or
// expired.
//
// Takes ids rather than a predicate because the flusher decided which ones, on
// a snapshot it read a moment ago. Deleting by "everything older than X" would
// re-derive that decision against a table another goroutine may have added to,
// and would eventually delete something that was never sent.
func (s *Store) DeleteQueuedDoorEvents(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM door_event_queue WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete queued door event %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// DoorEventGroups lists the (area, game) pairs that have anything waiting.
//
// One record is about one game, so this is the set of records that could be
// built right now, and it is what the flusher iterates.
func (s *Store) DoorEventGroups(ctx context.Context) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT area, game FROM door_event_queue GROUP BY area, game ORDER BY area, game`)
	if err != nil {
		return nil, fmt.Errorf("list door event groups: %w", err)
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var area, game string
		if err := rows.Scan(&area, &game); err != nil {
			return nil, err
		}
		out = append(out, [2]string{area, game})
	}
	return out, rows.Err()
}
