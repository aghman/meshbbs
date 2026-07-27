// Package meshlink is a link.Link over a Meshtastic radio (design §7.1).
//
// # The problem this package exists to solve
//
// Two addressing schemes meet here and neither can be changed. Above, the
// federation addresses peers by node ID — 8 bytes, BLAKE3 of an Ed25519 public
// key, self-certifying and with no registry (§6.1.1). Below, Meshtastic
// addresses radios by a 4-byte node number assigned by firmware. Nothing binds
// one to the other, and the design did not say how.
//
// Three ways to bind them were considered. Carrying the sender's 8-byte node ID
// in every packet is self-contained but costs 3.4% of a 233-byte MTU — and at
// K=15 symbols per bundle that is 120 bytes of identity where §6.1.3 went to
// deliberate trouble to pay 8. A 4-byte prefix halves the bill and still does
// not let a receiver resolve a peer it has never heard of, since a prefix is
// only meaningful against a roster you already hold.
//
// So the binding is LEARNED, from a signed ANNOUNCE, and no packet carries
// identity bytes at all. The announcement is self-certifying in exactly the way
// §6.1.1 makes node IDs self-certifying: it carries the public key, the ID is
// that key's hash, and the signature covers the radio number it is claiming. A
// receiver needs no authority, no registry and no prior knowledge of the peer.
package meshlink

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
)

// Frame types occupy the first byte of every BSMP payload.
//
// # Why the link reads a byte it does not own
//
// The layer above already spends byte 0 on a frame type (a gossip control
// message, a fountain symbol), so the link reads that byte rather than
// prepending one of its own. A second header byte would cost 1 byte per SYMBOL
// — about 15 bytes per bundle at K=15 — to buy layering purity on a link where
// §12.7 makes the byte budget a test.
//
// The consequence is that this registry is shared: L0 and L1 draw type codes
// from one space, and it lives here because the link is what has to
// discriminate. Values 1 and 2 are the layer above's and are passed through
// untouched, byte 0 included.
const (
	// FrameControl is a gossip control message (§7.3). Opaque here.
	FrameControl byte = 1
	// FrameSymbol is a fountain symbol (§7.2). Opaque here.
	FrameSymbol byte = 2
	// FrameAnnounce binds this radio to a node ID. Consumed by the link.
	FrameAnnounce byte = 3
	// FrameWhoIs asks an unattributed radio to announce itself. Consumed by
	// the link.
	FrameWhoIs byte = 4
)

// announceBody is the signed portion of an announcement:
//
//	byte 0      frame type
//	bytes 1-32  Ed25519 public key
//	bytes 33-36 radio node number, big endian
//	bytes 37-40 unix seconds, big endian
//
// followed by a 64-byte signature over all of it.
const (
	announceBodyLen = 1 + ed25519.PublicKeySize + 4 + 4
	announceLen     = announceBodyLen + ed25519.SignatureSize // 105
	whoIsLen        = 1
)

// Errors from parsing an announcement. They are distinguishable because a
// malformed announcement and a forged one deserve different log lines: the
// first is a bug or an old peer, the second is someone trying something.
var (
	ErrMalformedAnnounce = errors.New("meshlink: malformed announcement")
	ErrBadSignature      = errors.New("meshlink: announcement signature does not verify")
	ErrRadioMismatch     = errors.New("meshlink: announcement claims a different radio than it came from")
	ErrStaleAnnounce     = errors.New("meshlink: announcement is older than one already seen")
)

// Announcement is a peer's claim to a radio address, already verified.
type Announcement struct {
	// ID is derived from PublicKey, never read off the wire.
	ID        identity.NodeID
	PublicKey ed25519.PublicKey
	Radio     uint32
	At        time.Time
}

// EncodeAnnounce builds a signed announcement binding key to radio.
//
// The radio number is inside the SIGNED body, not merely in the Meshtastic
// header. That is the difference between an announcement and a replayable
// token: an attacker who captures this frame and rebroadcasts it from their own
// radio produces a signature over someone else's node number, which
// DecodeAnnounce rejects. Without the binding, any node could redirect a peer's
// unicast traffic to itself simply by echoing an announcement it overheard.
func EncodeAnnounce(key identity.NodeKey, radio uint32, at time.Time) []byte {
	buf := make([]byte, 0, announceLen)
	buf = append(buf, FrameAnnounce)
	buf = append(buf, key.Public...)
	buf = binary.BigEndian.AppendUint32(buf, radio)
	buf = binary.BigEndian.AppendUint32(buf, uint32(at.Unix()))
	return append(buf, key.Sign(buf)...)
}

// DecodeAnnounce verifies an announcement that arrived from radio `from`.
//
// Verification is entirely self-contained — hash the key, check the signature,
// check the binding — which is what makes a new peer usable the moment it is
// heard, with no registry to consult and nothing to trust first.
func DecodeAnnounce(payload []byte, from uint32) (Announcement, error) {
	if len(payload) != announceLen || payload[0] != FrameAnnounce {
		return Announcement{}, fmt.Errorf("%w: %d bytes", ErrMalformedAnnounce, len(payload))
	}
	body := payload[:announceBodyLen]
	sig := payload[announceBodyLen:]

	pub := ed25519.PublicKey(body[1 : 1+ed25519.PublicKeySize])
	if !ed25519.Verify(pub, body, sig) {
		return Announcement{}, ErrBadSignature
	}

	claimed := binary.BigEndian.Uint32(body[1+ed25519.PublicKeySize:])
	if claimed != from {
		return Announcement{}, fmt.Errorf("%w: claims 0x%08x, arrived from 0x%08x",
			ErrRadioMismatch, claimed, from)
	}

	secs := binary.BigEndian.Uint32(body[1+ed25519.PublicKeySize+4:])
	// The ID is computed from the key, never taken from the wire. There is no
	// field an attacker could put a different node's ID in.
	return Announcement{
		ID:        identity.NodeIDFromPublicKey(pub),
		PublicKey: append(ed25519.PublicKey(nil), pub...),
		Radio:     claimed,
		At:        time.Unix(int64(secs), 0).UTC(),
	}, nil
}

// EncodeWhoIs builds the one-byte request that asks a radio to announce itself.
//
// It exists because the alternative is announcing often enough that a new peer
// is learned promptly, and that is expensive: at 105 bytes, R=4 and fifty
// instances, a six-hourly broadcast announcement would spend about 1% of the
// whole channel — a fifth of the entire federation budget (§1.1) on saying
// hello. Asking costs one packet, exactly when there is something to learn.
func EncodeWhoIs() []byte { return []byte{FrameWhoIs} }
