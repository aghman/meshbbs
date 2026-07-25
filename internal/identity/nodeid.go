// Package identity implements node identity per design §6.1.
//
// A node's ID *is* its key: NodeID = BLAKE3(ed25519_pubkey)[:8]. This is
// self-certifying — anyone holding a record can hash the claimed pubkey and
// check it against the ID — which is why there is no registry, no address
// authority, and no way to squat an identity you do not hold the key for.
//
// 8 bytes (64 bits) is chosen against *adversarial* collision, not accidental:
// an attacker who could grind a keypair hashing to a target ID could present
// forged records to peers that have not yet learned the real pubkey. 32 bits is
// minutes on a GPU, 48 bits is hours-to-days, 64 bits is out of reach.
package identity

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"

	"lukechampine.com/blake3"
)

// NodeIDLen is the length of a node ID in bytes (§6.1.1).
const NodeIDLen = 8

// NodeID is the self-certifying identifier of a BBS instance.
type NodeID [NodeIDLen]byte

// NodeIDFromPublicKey derives a NodeID from an Ed25519 public key.
func NodeIDFromPublicKey(pub ed25519.PublicKey) NodeID {
	sum := blake3.Sum256(pub)
	var id NodeID
	copy(id[:], sum[:NodeIDLen])
	return id
}

// Matches reports whether pub hashes to this NodeID. This is the check that
// makes the ID self-certifying; every place that accepts a pubkey claiming an
// ID must call it.
func (id NodeID) Matches(pub ed25519.PublicKey) bool {
	return id == NodeIDFromPublicKey(pub)
}

// IsZero reports whether the ID is unset.
func (id NodeID) IsZero() bool { return id == NodeID{} }

// String returns the full 13-character Crockford base32 rendering, grouped for
// readability: K7QM-4X2P-B9TFR. This is the canonical display form (§6.1.4.2).
func (id NodeID) String() string { return Group(EncodeBase32(id[:])) }

// Compact returns the ungrouped 13-character rendering, for config files, log
// lines, and anywhere a single token is wanted.
func (id NodeID) Compact() string { return EncodeBase32(id[:]) }

// Short returns the first 8 characters, git-short-hash style. Use it for
// everyday display; never for a security decision such as confirming an
// allowlist entry, where the full ID must be shown.
func (id NodeID) Short() string {
	s := EncodeBase32(id[:])
	return s[:8]
}

// Words returns the six-word BIP-39 rendering, for reading aloud over voice
// radio or a phone (§6.1.4.2). It encodes exactly the same 64 bits as the
// base32 form — this is a second rendering, not a second identifier.
func (id NodeID) Words() string { return strings.Join(EncodeWords(id), "-") }

// ParseNodeID accepts either rendering — grouped or ungrouped base32, or six
// words separated by spaces or hyphens — and returns the NodeID.
func ParseNodeID(s string) (NodeID, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return NodeID{}, errors.New("empty node ID")
	}

	// Word form: six tokens that are not a valid base32 string.
	fields := strings.FieldsFunc(trimmed, func(r rune) bool {
		return r == ' ' || r == '-' || r == '\t'
	})
	if len(fields) == WordCount {
		if id, err := DecodeWords(fields); err == nil {
			return id, nil
		}
		// Fall through: a hyphen-grouped base32 string also splits into
		// multiple fields, so a word-decode failure is not yet an error.
	}

	b, err := DecodeBase32(strings.Join(fields, ""))
	if err != nil {
		return NodeID{}, fmt.Errorf("not a valid node ID (expected %d base32 characters or %d words): %w",
			base32Len(NodeIDLen), WordCount, err)
	}
	if len(b) != NodeIDLen {
		return NodeID{}, fmt.Errorf("node ID is %d bytes, got %d", NodeIDLen, len(b))
	}
	var id NodeID
	copy(id[:], b)
	return id, nil
}

// MarshalText implements encoding.TextMarshaler, so NodeIDs round-trip through
// TOML and JSON in their compact form.
func (id NodeID) MarshalText() ([]byte, error) { return []byte(id.Compact()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *NodeID) UnmarshalText(b []byte) error {
	parsed, err := ParseNodeID(string(b))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
