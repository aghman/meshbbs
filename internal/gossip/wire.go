package gossip

import (
	"encoding/binary"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

const identityLen = identity.NodeIDLen

func lessNode(a, b identity.NodeID) bool {
	for i := 0; i < identityLen; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// canonicalUvarint reads a uvarint and rejects overlong encodings.
//
// binary.Uvarint accepts a padded encoding: 0x80 0x00 decodes to zero just as
// 0x00 does. That gives one logical value several wire forms, and anything
// comparing or hashing the encoded bytes then reads identical state as
// divergent. In an anti-entropy protocol that is worse than a parse error — it
// is a livelock where two converged nodes exchange deltas forever.
//
// This is the same defence as vv.canonicalUvarint and record's decodeCanonical.
// It is duplicated rather than shared because each package owns its own wire
// format, and a shared helper would invite a change in one to silently alter
// the others' accepted encodings.
func canonicalUvarint(b []byte) (uint64, int, error) {
	val, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, ErrTruncated
	}
	if want := binary.AppendUvarint(nil, val); len(want) != n {
		return 0, 0, fmt.Errorf("%w: %d bytes encode a value needing %d",
			ErrNotCanonical, n, len(want))
	}
	return val, n, nil
}

// PeekType reports the message type of an encoded gossip message.
//
// Callers dispatch on this. Unknown types are rejected by Valid rather than
// ignored, because a message we cannot evaluate is one we must not act on or
// relay (§12.5).
func PeekType(b []byte) (MsgType, error) {
	if len(b) < 2 {
		return 0, ErrTruncated
	}
	t := MsgType(b[0])
	if !t.Valid() {
		return 0, fmt.Errorf("unknown gossip message type %d", b[0])
	}
	if b[1] != FormatVersion {
		return 0, fmt.Errorf("unsupported gossip version %d, expected %d", b[1], FormatVersion)
	}
	return t, nil
}
