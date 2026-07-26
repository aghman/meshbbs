package record

import (
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

// MaxNickLen bounds a user nick. It is short on purpose: nicks travel in every
// DM header and every directory entry, and at ~108 B/s a generous limit is paid
// for by the whole channel (§1.1).
const MaxNickLen = 24

// X25519KeyLen is the size of a user's DM public key.
const X25519KeyLen = 32

// ProfileFlags carry per-user policy that peers must respect.
type ProfileFlags uint8

const (
	// FlagUnlisted marks a user who has opted out of the directory.
	//
	// Note this flag exists on a record that, by definition, was published. It
	// is not the mechanism for staying out of the directory — that mechanism is
	// publishing NO profile at all (§6.7). This flag is for the case where a
	// user WAS listed and later opted out: peers that already hold the old
	// profile need to be told to hide it, and a tombstone would also destroy the
	// key material needed to receive replies.
	FlagUnlisted ProfileFlags = 1 << 0
)

// ProfileBody is the payload of a PROFILE record (§6.7).
//
// This is what makes a user DM-addressable network-wide, and it is the record
// the design is most careful about publishing, because the arithmetic is
// brutal: at ~100 B each, fifty local users would spend two full days of a
// node's entire mesh allocation just announcing that they exist, before anyone
// posts anything. Hence lazy publication — see package profile.
type ProfileBody struct {
	// Nick is the user's handle, unique within their home node only. Global
	// uniqueness would need a registry, and the whole design refuses to have
	// one; a nick is therefore always qualified by its node (§6.1.4).
	Nick string

	// DMKey is the user's X25519 public key. Its presence here is what lets a
	// stranger on another instance send a sealed DM without a round trip —
	// which is the entire reason a directory exists at all (§8.2).
	DMKey [X25519KeyLen]byte

	// Flags carry policy peers must respect, currently just FlagUnlisted.
	Flags ProfileFlags
}

// ProfileBodyLen is the encoded size for a given nick length.
func ProfileBodyLen(nick string) int { return 1 + len(nick) + X25519KeyLen + 1 }

// MarshalProfileBody encodes a ProfileBody.
//
// Layout: len(nick) | nick | dm_key(32) | flags(1)
func MarshalProfileBody(p ProfileBody) ([]byte, error) {
	if p.Nick == "" {
		return nil, fmt.Errorf("profile nick is empty")
	}
	if err := validateLabel("nick", p.Nick, MaxNickLen); err != nil {
		return nil, err
	}
	if p.DMKey == ([X25519KeyLen]byte{}) {
		// An all-zero X25519 key is not a key; it is the one value that makes
		// the shared secret degenerate. Refusing it here means a profile can
		// never advertise an unusable DM address.
		return nil, fmt.Errorf("profile DM key is all zero")
	}

	buf := make([]byte, 0, ProfileBodyLen(p.Nick))
	buf = append(buf, byte(len(p.Nick)))
	buf = append(buf, p.Nick...)
	buf = append(buf, p.DMKey[:]...)
	buf = append(buf, byte(p.Flags))
	return buf, nil
}

// UnmarshalProfileBody parses a PROFILE record body.
func UnmarshalProfileBody(b []byte) (ProfileBody, error) {
	var p ProfileBody
	if len(b) < 1 {
		return p, ErrTruncated
	}
	n := int(b[0])
	if n == 0 {
		return p, fmt.Errorf("profile nick is empty")
	}
	if len(b) != 1+n+X25519KeyLen+1 {
		return p, fmt.Errorf("%w: PROFILE body is %d bytes, expected %d for a %d-byte nick",
			ErrTruncated, len(b), 1+n+X25519KeyLen+1, n)
	}
	p.Nick = string(b[1 : 1+n])
	if err := validateLabel("nick", p.Nick, MaxNickLen); err != nil {
		return ProfileBody{}, err
	}
	copy(p.DMKey[:], b[1+n:1+n+X25519KeyLen])
	if p.DMKey == ([X25519KeyLen]byte{}) {
		return ProfileBody{}, fmt.Errorf("profile DM key is all zero")
	}

	flags := b[len(b)-1]
	// Reject unknown flag bits rather than ignoring them. An ignored bit is a
	// second wire form for one logical profile, and this codebase has now been
	// bitten by that class four times (record varints, version vectors, symbol
	// header, bundle origin table).
	if flags&^byte(FlagUnlisted) != 0 {
		return ProfileBody{}, fmt.Errorf("profile flags 0x%02x set reserved bits", flags)
	}
	p.Flags = ProfileFlags(flags)
	return p, nil
}

// Unlisted reports whether the user has opted out of the directory.
func (p ProfileBody) Unlisted() bool { return p.Flags&FlagUnlisted != 0 }

// NewProfileRecord builds a PROFILE record signed by the NODE key.
//
// Signed by the node, not the user: users have no signing keys in tiers 1 and 2
// of the DM custody design (§8.2), only an X25519 keypair for encryption. The
// node vouching for its own users is exactly the trust model — a profile says
// "this instance asserts it hosts this nick", which is all a remote peer needs
// and all it could verify anyway.
func NewProfileRecord(key identity.NodeKey, seq uint64, ts uint32, p ProfileBody) (*Record, error) {
	body, err := MarshalProfileBody(p)
	if err != nil {
		return nil, err
	}
	return New(key, Record{
		Origin: key.ID(),
		Seq:    seq,
		TS:     ts,
		Type:   TypeProfile,
		Area:   ProfileArea,
		Body:   body,
	})
}

// ProfileArea is the well-known area profiles replicate in.
//
// A separate area so that a node can federate forum content without carrying
// the network user directory, which at fifty instances is the larger of the two.
var ProfileArea = AreaTagFor("_directory")

// VerifyProfileRecord validates a PROFILE against its origin node's key.
func VerifyProfileRecord(r *Record, originPub []byte) (ProfileBody, error) {
	if r.Type != TypeProfile {
		return ProfileBody{}, fmt.Errorf("record %s is a %s, not a PROFILE", r.ID(), r.Type)
	}
	body, err := UnmarshalProfileBody(r.Body)
	if err != nil {
		return ProfileBody{}, fmt.Errorf("PROFILE %s: %w", r.ID(), err)
	}
	if err := r.Verify(originPub); err != nil {
		return ProfileBody{}, fmt.Errorf("PROFILE %s: %w", r.ID(), err)
	}
	return body, nil
}

// ProfileSize reports the uncompressed on-wire cost of a profile, for the
// budget arithmetic in §6.7. Exported because that arithmetic is the entire
// justification for lazy publication, and a reader should be able to check it.
func ProfileSize(nick string) int {
	// Record framing plus the body: the signature dominates.
	return 64 + 8 + 8 + 4 + 1 + 4 + ProfileBodyLen(nick)
}
