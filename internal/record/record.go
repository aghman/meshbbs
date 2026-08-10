// Package record implements the append-only signed record log (design §6.2)
// and its canonical encoding (§6.2.1).
//
// # The three hazards this package exists to prevent
//
// §6.2.1 names three ways to silently break every signature in the network.
// All three are addressed here, and the reasons are worth restating because
// each failure is silent rather than loud:
//
//  1. The canonical encoding is FROZEN and versioned independently of any
//     storage schema. Verification uses the bytes that were signed, retained
//     verbatim (see Signed), never a re-serialization of parsed fields.
//  2. Encoding is deterministic. No map iteration, no floating point, no
//     platform-dependent widths. TestEncodingDeterminism asserts this.
//  3. Sequence numbers never regress. That is enforced in the store, not here,
//     but Record carries the (Origin, Seq) coordinate the invariant protects.
//
// # Wire format
//
// Fields are emitted in a fixed order with no field tags — the layout IS the
// schema, which is what makes a 233-byte MTU survivable (§1). A leading format
// byte allows exactly one thing to change later: the format.
package record

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"lukechampine.com/blake3"
)

// FormatVersion is the canonical encoding version. Bumping it is a wire-format
// change: it requires new conformance vectors (§12.6) and, after the Phase 6
// freeze, a compatibility story for both directions.
const FormatVersion uint8 = 1

// IDLen is the length of a record ID. 16 bytes (128 bits) is ample collision
// resistance at this scale, and it is what a `parent` reference costs on the
// wire, so it is deliberately not 32 (§6.2).
const IDLen = 16

// MaxBodyLen bounds a record body before compression. Without a cap, a hostile
// or merely buggy peer can force unbounded fragment buffering (§6.2.1).
const MaxBodyLen = 8 << 10 // 8 KiB

// ID is the content address of a record: BLAKE3(canonical_body)[:16].
//
// It is DERIVED, never transmitted — the receiver recomputes it from fields it
// already has, saving 16 bytes (7% of a mesh packet) per record (§6.2).
type ID [IDLen]byte

func (id ID) String() string { return identity.EncodeBase32(id[:]) }
func (id ID) IsZero() bool   { return id == ID{} }

// Type discriminates record kinds (§6.2). Values are part of the wire format
// and must never be renumbered.
type Type uint8

const (
	TypePost       Type = 1
	TypeDM         Type = 2
	TypeProfile    Type = 3
	TypeNode       Type = 4
	TypeSuccession Type = 5
	TypeArea       Type = 6
	TypeFile       Type = 7
	TypeTombstone  Type = 8
	TypeVote       Type = 9
	TypeDoorEvent  Type = 10
)

var typeNames = map[Type]string{
	TypePost: "POST", TypeDM: "DM", TypeProfile: "PROFILE", TypeNode: "NODE",
	TypeSuccession: "SUCCESSION", TypeArea: "AREA", TypeFile: "FILE",
	TypeTombstone: "TOMBSTONE", TypeVote: "VOTE", TypeDoorEvent: "DOOR_EVENT",
}

func (t Type) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("Type(%d)", uint8(t))
}

// Valid reports whether t is a known record type. Unknown types are rejected
// rather than relayed: a peer must not be able to inject records whose
// semantics we cannot evaluate.
func (t Type) Valid() bool { _, ok := typeNames[t]; return ok }

// AreaTag is the truncated hash of an area name. It hoists to the bundle
// header on the wire (§6.2), so it is paid once per bundle rather than per
// record.
type AreaTag [4]byte

// AreaTagFor derives the tag for an area name.
func AreaTagFor(name string) AreaTag {
	sum := blake3.Sum256([]byte(name))
	var t AreaTag
	copy(t[:], sum[:4])
	return t
}

// Record is an immutable, signed entry in the log.
//
// Construct one with New, which computes the canonical bytes, the derived ID,
// and the signature together — a Record is not meaningful until signed.
type Record struct {
	Origin identity.NodeID // authoring instance
	Seq    uint64          // per-origin monotonic sequence
	TS     uint32          // unix seconds; ADVISORY only (§6.2.1)
	Type   Type
	Area   AreaTag
	Parent ID     // zero if this record has no parent
	Body   []byte // type-specific payload

	// id is the derived content address.
	id ID
	// signed is the exact byte sequence that was signed. Retaining it is the
	// §6.2.1 rule-1 defence: verification never re-serializes parsed fields,
	// so an encoder change cannot invalidate history.
	signed []byte
	// sig is the Ed25519 signature over signed, by the origin node key.
	sig [64]byte
}

func (r *Record) ID() ID            { return r.id }
func (r *Record) Signature() []byte { s := r.sig; return s[:] }

// SignedBytes returns the exact bytes that were signed. Callers must treat the
// result as read-only.
func (r *Record) SignedBytes() []byte { return r.signed }

// HasParent reports whether this record threads under another.
func (r *Record) HasParent() bool { return !r.Parent.IsZero() }

// flagHasParent marks the presence of the 16-byte parent field, which is
// omitted for top-level records so they do not pay for it (§6.2).
const flagHasParent uint8 = 1 << 0

// canonicalBytes renders the signable representation.
//
// The layout is fixed and deliberately boring:
//
//	format(1) | flags(1) | type(1) | origin(8) | seq(uvarint) |
//	ts(4, BE) | area(4) | [parent(16)] | bodyLen(uvarint) | body
//
// Note there is no map anywhere in this function, and no iteration whose order
// could vary. That is not an accident (§6.2.1 rule 2).
func (r *Record) canonicalBytes() ([]byte, error) {
	if !r.Type.Valid() {
		return nil, fmt.Errorf("unknown record type %d", uint8(r.Type))
	}
	if len(r.Body) > MaxBodyLen {
		return nil, fmt.Errorf("body is %d bytes, limit is %d (§6.2.1)", len(r.Body), MaxBodyLen)
	}
	if r.Origin.IsZero() {
		return nil, errors.New("record has no origin")
	}

	var flags uint8
	if r.HasParent() {
		flags |= flagHasParent
	}

	// Exact capacity: this is a byte budget, and §12.7 asserts it in CI.
	size := 1 + 1 + 1 + identity.NodeIDLen + binary.MaxVarintLen64 + 4 + 4 +
		binary.MaxVarintLen64 + len(r.Body)
	if r.HasParent() {
		size += IDLen
	}
	buf := make([]byte, 0, size)

	buf = append(buf, FormatVersion, flags, uint8(r.Type))
	buf = append(buf, r.Origin[:]...)
	buf = binary.AppendUvarint(buf, r.Seq)
	buf = binary.BigEndian.AppendUint32(buf, r.TS)
	buf = append(buf, r.Area[:]...)
	if r.HasParent() {
		buf = append(buf, r.Parent[:]...)
	}
	buf = binary.AppendUvarint(buf, uint64(len(r.Body)))
	buf = append(buf, r.Body...)
	return buf, nil
}

// deriveID computes BLAKE3(canonical)[:16].
func deriveID(canonical []byte) ID {
	sum := blake3.Sum256(canonical)
	var id ID
	copy(id[:], sum[:IDLen])
	return id
}

// New builds and signs a record with key. The key must be the origin's key —
// New verifies that, because signing as an origin you are not is a bug that
// would otherwise only surface on a remote peer.
func New(key identity.NodeKey, r Record) (*Record, error) {
	if r.Origin.IsZero() {
		r.Origin = key.ID()
	}
	if r.Origin != key.ID() {
		return nil, fmt.Errorf("cannot sign a record whose origin is %s with the key for %s",
			r.Origin, key.ID())
	}

	canonical, err := r.canonicalBytes()
	if err != nil {
		return nil, err
	}
	// §7.5: a FILE record carries a catalog entry and nothing else. Checked
	// here so no caller can mint one carrying file content, at any size.
	if err := checkNoFileContent(&r); err != nil {
		return nil, err
	}
	// §9.5: a DOOR_EVENT carries a batch of bounded events. Same reason, same
	// place — without this the generic 8 KiB body makes the payload bound a
	// suggestion rather than a rule.
	if err := checkDoorEventBody(&r); err != nil {
		return nil, err
	}
	out := r
	out.Body = append([]byte(nil), r.Body...) // defensive copy: records are immutable
	out.signed = canonical
	out.id = deriveID(canonical)
	copy(out.sig[:], key.Sign(canonical))
	return &out, nil
}

// Verify checks the record's signature against pub, and confirms that pub is
// in fact the origin's key.
//
// The self-certifying ID check (§6.1.1) is the whole point: a caller cannot
// accidentally verify against an attacker-supplied key, because the key must
// hash to the origin ID already embedded in the signed bytes.
func (r *Record) Verify(pub []byte) error {
	if len(r.signed) == 0 {
		return errors.New("record has no signed bytes; it was not built by New or Decode")
	}
	if !r.Origin.Matches(pub) {
		return fmt.Errorf("public key does not match origin %s", r.Origin)
	}
	if !verifySig(pub, r.signed, r.sig[:]) {
		return fmt.Errorf("signature verification failed for record %s", r.id)
	}
	if got := deriveID(r.signed); got != r.id {
		return fmt.Errorf("record ID %s does not match its content (%s)", r.id, got)
	}
	return nil
}
