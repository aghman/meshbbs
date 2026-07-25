package store

import (
	"context"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// DM is a direct message as listed. The body is never in this struct: reading
// it requires the recipient's passphrase (§8.2), so listing and reading are
// deliberately separate operations.
type DM struct {
	ID          record.ID
	Sender      string
	Recipient   string
	SenderNode  identity.NodeID
	Subject     string
	SentAt      int64
	ReadAt      int64
	SealedBytes []byte
}

// Unread reports whether the message has been opened.
func (d DM) Unread() bool { return d.ReadAt == 0 }

// DMBody is the wire body of a DM record (§6.4).
//
// Addressing is deliberately in the CLEAR ([D7]) — metadata privacy is
// explicitly not a requirement, and a readable recipient buys immediate
// bounces and per-recipient spam filtering. Only `Sealed` is encrypted.
type DMBody struct {
	Sender    string
	Recipient string
	Subject   string
	Sealed    []byte // keyring.Seal output; opaque to this node
}

// MarshalDMBody encodes a DM body deterministically.
func MarshalDMBody(d DMBody) ([]byte, error) {
	if len(d.Sender) > 16 {
		return nil, fmt.Errorf("sender is %d bytes, limit is 16", len(d.Sender))
	}
	if len(d.Recipient) > 16 {
		return nil, fmt.Errorf("recipient is %d bytes, limit is 16", len(d.Recipient))
	}
	if len(d.Subject) > 72 {
		return nil, fmt.Errorf("subject is %d bytes, limit is 72", len(d.Subject))
	}
	out := make([]byte, 0, 3+len(d.Sender)+len(d.Recipient)+len(d.Subject)+len(d.Sealed))
	out = append(out, byte(len(d.Sender)))
	out = append(out, d.Sender...)
	out = append(out, byte(len(d.Recipient)))
	out = append(out, d.Recipient...)
	out = append(out, byte(len(d.Subject)))
	out = append(out, d.Subject...)
	out = append(out, d.Sealed...)
	return out, nil
}

// UnmarshalDMBody decodes a DM body.
func UnmarshalDMBody(b []byte) (DMBody, error) {
	var d DMBody
	read := func(src []byte) (string, []byte, error) {
		if len(src) < 1 {
			return "", nil, record.ErrTruncated
		}
		n := int(src[0])
		if len(src) < 1+n {
			return "", nil, record.ErrTruncated
		}
		return string(src[1 : 1+n]), src[1+n:], nil
	}
	var err error
	rest := b
	if d.Sender, rest, err = read(rest); err != nil {
		return DMBody{}, err
	}
	if d.Recipient, rest, err = read(rest); err != nil {
		return DMBody{}, err
	}
	if d.Subject, rest, err = read(rest); err != nil {
		return DMBody{}, err
	}
	d.Sealed = append([]byte(nil), rest...)
	return d, nil
}

// IndexDM records the cleartext routing information for a stored DM record.
func (s *Store) IndexDM(ctx context.Context, id record.ID, body DMBody, senderNode identity.NodeID, sentAt int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO dm_index (record_id, sender, recipient, sender_node, subject, sent_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(record_id) DO NOTHING`,
		id[:], body.Sender, body.Recipient, senderNode[:], body.Subject, sentAt)
	if err != nil {
		return fmt.Errorf("index DM: %w", err)
	}
	return nil
}

// Inbox lists messages addressed to a user, newest first.
//
// This works entirely from cleartext routing data — no passphrase, no
// decryption. A user can see that mail arrived, and from whom, before
// unlocking anything.
func (s *Store) Inbox(ctx context.Context, nick string, limit int) ([]DM, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.record_id, d.sender, d.recipient, d.sender_node, d.subject, d.sent_at, d.read_at, r.body
		FROM dm_index d
		JOIN records r ON r.id = d.record_id
		WHERE d.recipient = ?
		ORDER BY d.sent_at DESC
		LIMIT ?`, nick, limit)
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}
	defer rows.Close()
	return scanDMs(rows)
}

// Outbox lists messages a user has sent.
func (s *Store) Outbox(ctx context.Context, nick string, limit int) ([]DM, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.record_id, d.sender, d.recipient, d.sender_node, d.subject, d.sent_at, d.read_at, r.body
		FROM dm_index d
		JOIN records r ON r.id = d.record_id
		WHERE d.sender = ?
		ORDER BY d.sent_at DESC
		LIMIT ?`, nick, limit)
	if err != nil {
		return nil, fmt.Errorf("list outbox: %w", err)
	}
	defer rows.Close()
	return scanDMs(rows)
}

type rowScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanDMs(rows rowScanner) ([]DM, error) {
	var out []DM
	for rows.Next() {
		var d DM
		var id, node, body []byte
		if err := rows.Scan(&id, &d.Sender, &d.Recipient, &node, &d.Subject,
			&d.SentAt, &d.ReadAt, &body); err != nil {
			return nil, err
		}
		copy(d.ID[:], id)
		copy(d.SenderNode[:], node)
		if parsed, err := UnmarshalDMBody(body); err == nil {
			d.SealedBytes = parsed.Sealed
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UnreadCount returns how many unopened messages a user has.
func (s *Store) UnreadCount(ctx context.Context, nick string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dm_index WHERE recipient = ? AND read_at = 0`, nick).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count unread: %w", err)
	}
	return n, nil
}

// MarkDMRead stamps a message as opened.
func (s *Store) MarkDMRead(ctx context.Context, id record.ID) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE dm_index SET read_at = ? WHERE record_id = ? AND read_at = 0`, s.now(), id[:])
	if err != nil {
		return fmt.Errorf("mark DM read: %w", err)
	}
	return nil
}
