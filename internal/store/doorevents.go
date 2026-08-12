package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
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

// DeliveredDoorEvent is one event read back out of the log for a door (§9.5).
//
// Flattened from records: one record carries up to
// record.MaxDoorEventsPerRecord events, and a door wants events rather than
// records. The cursor is still per-record, because a record either arrived or
// did not — there is no partial delivery to represent.
type DeliveredDoorEvent struct {
	// Cursor is the record's local arrival number. Every event out of one
	// record shares it.
	Cursor int64
	Origin identity.NodeID
	// At is the record's advisory timestamp (§6.2.1): rendered, never computed
	// from.
	At         int64
	Kind       uint8
	Actor      string
	Target     string
	TargetNode identity.NodeID
	Payload    []byte
}

// DoorEventsSince reads a league's events in the order they REACHED this node.
//
// # Why the cursor is local arrival order
//
// Not (origin, seq): records arrive out of order on a mesh as a matter of
// course, and a door that remembered a per-origin high-water mark would step
// past a record repaired an hour late and never ask for it again. Not
// received_at either — it is seconds, and a league night delivers a fight as
// several records inside one.
//
// See migration 0010 for why local_seq is an explicit counter rather than the
// rowid.
//
// truncated reports that retention has pruned records the caller had not seen:
// its cursor points below the oldest record still held, so there is a gap that
// no amount of polling will fill. A door told this can say "some results were
// lost" rather than quietly showing an incomplete league table.
func (s *Store) DoorEventsSince(ctx context.Context, area record.AreaTag, game string, after int64, limit int) (events []DeliveredDoorEvent, cursor int64, truncated bool, err error) {
	if limit <= 0 {
		limit = 200
	}

	// The oldest record still held for this area. If the caller is behind it,
	// something it had not read has already been pruned.
	var oldest sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(local_seq) FROM records WHERE area = ? AND type = ?`,
		area[:], int(record.TypeDoorEvent)).Scan(&oldest); err != nil {
		return nil, after, false, fmt.Errorf("find the oldest door event: %w", err)
	}
	if oldest.Valid && after > 0 && after < oldest.Int64-1 {
		truncated = true
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT local_seq, origin, ts, body
		FROM records
		WHERE area = ? AND type = ? AND local_seq > ?
		ORDER BY local_seq
		LIMIT ?`,
		area[:], int(record.TypeDoorEvent), after, limit)
	if err != nil {
		return nil, after, truncated, fmt.Errorf("read door events: %w", err)
	}
	defer rows.Close()

	cursor = after
	for rows.Next() {
		var local, ts int64
		var origin, body []byte
		if err := rows.Scan(&local, &origin, &ts, &body); err != nil {
			return nil, after, truncated, err
		}
		cursor = local

		decoded, err := record.UnmarshalDoorEventBody(body)
		if err != nil {
			// Stored records were validated on the way in, so this is a
			// corrupt database rather than a hostile peer. Skip the record and
			// keep the cursor moving: stopping here would wedge every future
			// poll behind one bad row.
			continue
		}
		if game != "" && decoded.Game != game {
			continue
		}
		var id identity.NodeID
		copy(id[:], origin)
		for _, ev := range decoded.Events {
			events = append(events, DeliveredDoorEvent{
				Cursor: local, Origin: id, At: ts,
				Kind: ev.Kind, Actor: ev.Actor,
				Target: ev.Target, TargetNode: ev.TargetNode,
				Payload: ev.Payload,
			})
		}
	}
	return events, cursor, truncated, rows.Err()
}

// LeagueSummary is one door area as a sysop needs to see it.
type LeagueSummary struct {
	Area      string
	Federated bool
	// Games are the game names that have actually arrived, whether or not
	// anything here plays them.
	Games []string
	// Received counts DOOR_EVENT records held for this area, and Events the
	// events inside them.
	Received int
	Events   int
	// Waiting counts events queued locally that have not been sent.
	Waiting int
	// Doors are the installed doors pointed at this league. Empty means this
	// node is carrying a league it does not play.
	Doors []string
}

// Leagues summarises every door area on this node.
//
// Includes leagues with no door installed, deliberately: that is the case a
// sysop most needs to see, because carrying a league costs airtime — this node
// serves those records to peers on request — and nothing else would ever
// mention it.
func (s *Store) Leagues(ctx context.Context) ([]LeagueSummary, error) {
	areas, err := s.ListDoorAreas(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]LeagueSummary, 0, len(areas))
	for _, a := range areas {
		sum := LeagueSummary{Area: a.Name, Federated: a.Federated}

		records, _, _, err := s.DoorEventsSince(ctx, a.Tag, "", 0, 100000)
		if err != nil {
			return nil, err
		}
		seenGame := map[string]bool{}
		seenCursor := map[int64]bool{}
		for _, ev := range records {
			sum.Events++
			seenCursor[ev.Cursor] = true
		}
		sum.Received = len(seenCursor)

		// The game names come from the bodies rather than from any table: a
		// game is whatever a door called it, and nothing here registers one.
		games, err := s.doorGamesIn(ctx, a.Tag)
		if err != nil {
			return nil, err
		}
		for _, g := range games {
			if !seenGame[g] {
				seenGame[g] = true
				sum.Games = append(sum.Games, g)
			}
		}

		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM door_event_queue WHERE area = ?`, a.Name).Scan(&sum.Waiting); err != nil {
			return nil, fmt.Errorf("count waiting door events: %w", err)
		}

		rows, err := s.db.QueryContext(ctx,
			`SELECT name FROM doors WHERE league_area = ? ORDER BY name`, a.Name)
		if err != nil {
			return nil, fmt.Errorf("find doors for league %s: %w", a.Name, err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				rows.Close()
				return nil, err
			}
			sum.Doors = append(sum.Doors, name)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		out = append(out, sum)
	}
	return out, nil
}

// doorGamesIn lists the distinct game names held in one league area.
func (s *Store) doorGamesIn(ctx context.Context, area record.AreaTag) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT body FROM records WHERE area = ? AND type = ? ORDER BY local_seq`,
		area[:], int(record.TypeDoorEvent))
	if err != nil {
		return nil, fmt.Errorf("read door event games: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		decoded, err := record.UnmarshalDoorEventBody(body)
		if err != nil {
			continue
		}
		if !seen[decoded.Game] {
			seen[decoded.Game] = true
			out = append(out, decoded.Game)
		}
	}
	return out, rows.Err()
}
