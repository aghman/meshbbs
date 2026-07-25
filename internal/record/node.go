package record

import (
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aghman/meshbbs/internal/identity"
)

// MaxDisplayNameLen bounds a node's self-declared display name.
const MaxDisplayNameLen = 32

// MaxSysopContactLen bounds the free-text sysop contact field.
const MaxSysopContactLen = 64

// NodeBody is the payload of a NODE record (§6.1.2).
//
// Its job is key distribution and metadata, NOT binding: the node ID is
// BLAKE3(PublicKey)[:8] by construction, so a NODE record is entirely
// self-validating and there is no first-seen rule, no conflict case, and
// nothing to squat. That is what makes it safe for any peer to rebroadcast
// another node's NODE record, which in turn is what makes bootstrapping a new
// instance easy: ask anyone for the roster.
type NodeBody struct {
	// PublicKey is the full Ed25519 key whose hash is the origin node ID.
	PublicKey ed25519.PublicKey

	// DisplayName is a self-declared label ("pnw-bbs", "Fog City"). It is NOT
	// unique, NOT authoritative, and never used for routing (§6.1.4). The UI
	// must always render it next to the short ID so a spoofed name is visibly
	// attached to the wrong identity.
	DisplayName string

	// SysopContact is free-text, for humans.
	SysopContact string

	// Incarnation increments whenever the node detects that its own sequence
	// numbers may have regressed — typically a restore from backup (§6.2.1
	// rule 3). Peers seeing a new incarnation know to treat this origin's log
	// as needing re-verification rather than assuming continuity.
	Incarnation uint32
}

// MarshalNodeBody encodes a NodeBody deterministically.
//
// Layout: pubkey(32) | incarnation(4, BE) | len(name) | name | len(contact) | contact
func MarshalNodeBody(n NodeBody) ([]byte, error) {
	if len(n.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(n.PublicKey), ed25519.PublicKeySize)
	}
	if err := validateLabel("display name", n.DisplayName, MaxDisplayNameLen); err != nil {
		return nil, err
	}
	if err := validateLabel("sysop contact", n.SysopContact, MaxSysopContactLen); err != nil {
		return nil, err
	}

	buf := make([]byte, 0, ed25519.PublicKeySize+4+2+len(n.DisplayName)+len(n.SysopContact))
	buf = append(buf, n.PublicKey...)
	buf = binary.BigEndian.AppendUint32(buf, n.Incarnation)
	buf = append(buf, byte(len(n.DisplayName)))
	buf = append(buf, n.DisplayName...)
	buf = append(buf, byte(len(n.SysopContact)))
	buf = append(buf, n.SysopContact...)
	return buf, nil
}

// UnmarshalNodeBody parses a NODE record body.
func UnmarshalNodeBody(b []byte) (NodeBody, error) {
	var n NodeBody
	p := 0
	if len(b) < ed25519.PublicKeySize+4+2 {
		return n, ErrTruncated
	}
	n.PublicKey = append(ed25519.PublicKey(nil), b[:ed25519.PublicKeySize]...)
	p += ed25519.PublicKeySize
	n.Incarnation = binary.BigEndian.Uint32(b[p:])
	p += 4

	readStr := func(what string, max int) (string, error) {
		if p >= len(b) {
			return "", ErrTruncated
		}
		l := int(b[p])
		p++
		if p+l > len(b) {
			return "", ErrTruncated
		}
		s := string(b[p : p+l])
		p += l
		if err := validateLabel(what, s, max); err != nil {
			return "", err
		}
		return s, nil
	}

	var err error
	if n.DisplayName, err = readStr("display name", MaxDisplayNameLen); err != nil {
		return NodeBody{}, err
	}
	if n.SysopContact, err = readStr("sysop contact", MaxSysopContactLen); err != nil {
		return NodeBody{}, err
	}
	if p != len(b) {
		return NodeBody{}, fmt.Errorf("%d trailing bytes in NODE body", len(b)-p)
	}
	return n, nil
}

// validateLabel rejects over-long, non-UTF-8, and control-character-bearing
// text. Display names arrive from strangers and are rendered into a terminal,
// so an unfiltered one is an ANSI injection waiting to happen.
func validateLabel(what, s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%s is %d bytes, limit is %d", what, len(s), max)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	if i := strings.IndexFunc(s, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}); i >= 0 {
		return fmt.Errorf("%s contains a control character at byte %d", what, i)
	}
	return nil
}

// NewNodeRecord builds the self-signed NODE record announcing key.
func NewNodeRecord(key identity.NodeKey, seq uint64, ts uint32, displayName, contact string, incarnation uint32) (*Record, error) {
	body, err := MarshalNodeBody(NodeBody{
		PublicKey:    key.Public,
		DisplayName:  displayName,
		SysopContact: contact,
		Incarnation:  incarnation,
	})
	if err != nil {
		return nil, err
	}
	return New(key, Record{
		Origin: key.ID(),
		Seq:    seq,
		TS:     ts,
		Type:   TypeNode,
		Body:   body,
	})
}

// VerifyNodeRecord validates a NODE record end to end without any external
// input: it parses the body, confirms the embedded public key hashes to the
// record's own origin ID, and verifies the signature with that key.
//
// This is the self-certifying property in one function. A caller needs no prior
// knowledge of the node — which is precisely why a roster can be fetched from
// an untrusted peer.
func VerifyNodeRecord(r *Record) (NodeBody, error) {
	if r.Type != TypeNode {
		return NodeBody{}, fmt.Errorf("record %s is a %s, not a NODE record", r.ID(), r.Type)
	}
	body, err := UnmarshalNodeBody(r.Body)
	if err != nil {
		return NodeBody{}, fmt.Errorf("NODE record %s: %w", r.ID(), err)
	}
	if !r.Origin.Matches(body.PublicKey) {
		return NodeBody{}, fmt.Errorf("NODE record %s: embedded public key hashes to %s, not to the record's origin %s",
			r.ID(), identity.NodeIDFromPublicKey(body.PublicKey), r.Origin)
	}
	if err := r.Verify(body.PublicKey); err != nil {
		return NodeBody{}, err
	}
	return body, nil
}
