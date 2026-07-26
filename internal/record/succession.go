package record

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

// SuccessionBody is the payload of a SUCCESSION record (§6.1.6).
//
// # Why this record exists at all
//
// The node ID *is* the key: ID = BLAKE3(pubkey)[:8]. So rotating the key means
// becoming a different node, and without a way to say "that was me", every
// rotation would silently orphan a node's entire history and every peer
// relationship it had. SUCCESSION is that statement, signed by the OLD key
// because only the old key can speak for the old identity.
//
// The record's own envelope carries the rest: Origin is the OLD node ID, and
// the signature is by the OLD key. Only the forward-looking half lives here.
type SuccessionBody struct {
	// Successor is the new node ID.
	Successor identity.NodeID

	// NewPublicKey is the successor's full Ed25519 key, so the claim
	// self-verifies: a reader confirms BLAKE3(NewPublicKey)[:8] == Successor
	// without asking anyone. Carrying only the ID would make this record a
	// pointer to an identity the reader could not yet check, and the whole
	// design's refusal to have a registry rests on never needing to.
	NewPublicKey ed25519.PublicKey

	// Effective is when the handover takes place, in unix seconds.
	//
	// Records from the old ID with ts > Effective are rejected; those with
	// ts <= Effective stay valid, so a lossy mesh does not lose the tail of the
	// predecessor's log while the succession is still propagating (§6.1.6
	// guardrail 2).
	Effective uint32
}

// SuccessionBodyLen is the fixed encoded size: 8 + 32 + 4.
const SuccessionBodyLen = identity.NodeIDLen + ed25519.PublicKeySize + 4

// MarshalSuccessionBody encodes a SuccessionBody.
//
// Layout: successor(8) | new_pubkey(32) | effective(4, BE). Fixed width, so
// there is no length field to disagree with and exactly one wire form.
func MarshalSuccessionBody(s SuccessionBody) ([]byte, error) {
	if len(s.NewPublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("successor public key is %d bytes, want %d",
			len(s.NewPublicKey), ed25519.PublicKeySize)
	}
	// Check the self-certifying property at construction as well as on receipt.
	// A node that emits a SUCCESSION nobody can verify has silently ended its
	// own identity, and it will not find out until its peers stop following.
	if got := identity.NodeIDFromPublicKey(s.NewPublicKey); got != s.Successor {
		return nil, fmt.Errorf("successor public key hashes to %s, not to the declared successor %s",
			got, s.Successor)
	}
	if s.Successor.IsZero() {
		return nil, fmt.Errorf("successor ID is zero")
	}

	buf := make([]byte, 0, SuccessionBodyLen)
	buf = append(buf, s.Successor[:]...)
	buf = append(buf, s.NewPublicKey...)
	buf = binary.BigEndian.AppendUint32(buf, s.Effective)
	return buf, nil
}

// UnmarshalSuccessionBody parses a SUCCESSION record body.
func UnmarshalSuccessionBody(b []byte) (SuccessionBody, error) {
	var s SuccessionBody
	if len(b) != SuccessionBodyLen {
		return s, fmt.Errorf("%w: SUCCESSION body is %d bytes, want exactly %d",
			ErrTruncated, len(b), SuccessionBodyLen)
	}
	copy(s.Successor[:], b[:identity.NodeIDLen])
	s.NewPublicKey = append(ed25519.PublicKey(nil),
		b[identity.NodeIDLen:identity.NodeIDLen+ed25519.PublicKeySize]...)
	s.Effective = binary.BigEndian.Uint32(b[identity.NodeIDLen+ed25519.PublicKeySize:])

	if s.Successor.IsZero() {
		return SuccessionBody{}, fmt.Errorf("SUCCESSION names a zero successor ID")
	}
	// Self-certification, checked at the parser boundary so no caller can skip
	// it. This is the entire security of auto-follow: a SUCCESSION whose key
	// does not hash to its successor is a redirect to an identity the sender
	// cannot prove exists.
	if got := identity.NodeIDFromPublicKey(s.NewPublicKey); got != s.Successor {
		return SuccessionBody{}, fmt.Errorf(
			"SUCCESSION public key hashes to %s but names successor %s", got, s.Successor)
	}
	return s, nil
}

// NewSuccessionRecord builds a SUCCESSION signed by the OLD key.
//
// oldKey must be the predecessor's key: a succession signed by anyone else is
// a stranger asserting where someone else's identity went.
func NewSuccessionRecord(oldKey identity.NodeKey, seq uint64, ts uint32,
	successor identity.NodeID, newPub ed25519.PublicKey, effective uint32) (*Record, error) {

	if successor == oldKey.ID() {
		return nil, fmt.Errorf("a node cannot succeed itself (%s)", successor)
	}
	body, err := MarshalSuccessionBody(SuccessionBody{
		Successor:    successor,
		NewPublicKey: newPub,
		Effective:    effective,
	})
	if err != nil {
		return nil, err
	}
	return New(oldKey, Record{
		Origin: oldKey.ID(),
		Seq:    seq,
		TS:     ts,
		Type:   TypeSuccession,
		Body:   body,
	})
}

// VerifySuccessionRecord validates a SUCCESSION end to end.
//
// It needs the predecessor's public key, which the caller already holds from
// that node's NODE record — unlike a NODE record, a SUCCESSION is NOT
// self-contained, and deliberately so. It is signed by the old key and only
// carries the new one, so verifying it requires already knowing the identity
// being handed over. A record that vouched for both ends would let a stranger
// mint a succession for a node nobody had heard of.
func VerifySuccessionRecord(r *Record, predecessorPub ed25519.PublicKey) (SuccessionBody, error) {
	if r.Type != TypeSuccession {
		return SuccessionBody{}, fmt.Errorf("record %s is a %s, not a SUCCESSION", r.ID(), r.Type)
	}
	body, err := UnmarshalSuccessionBody(r.Body)
	if err != nil {
		return SuccessionBody{}, fmt.Errorf("SUCCESSION %s: %w", r.ID(), err)
	}
	if !r.Origin.Matches(predecessorPub) {
		return SuccessionBody{}, fmt.Errorf(
			"SUCCESSION %s: the supplied key belongs to %s, but the record's origin is %s",
			r.ID(), identity.NodeIDFromPublicKey(predecessorPub), r.Origin)
	}
	if body.Successor == r.Origin {
		return SuccessionBody{}, fmt.Errorf("SUCCESSION %s: a node cannot succeed itself", r.ID())
	}
	if err := r.Verify(predecessorPub); err != nil {
		return SuccessionBody{}, fmt.Errorf("SUCCESSION %s: %w", r.ID(), err)
	}
	return body, nil
}
