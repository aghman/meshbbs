package record

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

func verifySig(pub, msg, sig []byte) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
}

// ErrTruncated is returned when input ends mid-field.
var ErrTruncated = errors.New("truncated record")

// Marshal returns the on-disk / on-wire form: the canonical signed bytes
// followed by the 64-byte signature.
//
// The signature trails rather than leads so that the signed prefix is
// contiguous and can be verified without copying.
func (r *Record) Marshal() []byte {
	out := make([]byte, 0, len(r.signed)+len(r.sig))
	out = append(out, r.signed...)
	out = append(out, r.sig[:]...)
	return out
}

// Unmarshal parses a record and retains the signed prefix verbatim (§6.2.1
// rule 1). It does NOT verify the signature — call Verify once the origin's
// public key is known, which on a lossy mesh may be after the NODE record
// arrives (§6.1.2).
func Unmarshal(b []byte) (*Record, error) {
	if len(b) < ed25519.SignatureSize+8 {
		return nil, ErrTruncated
	}
	sigOff := len(b) - ed25519.SignatureSize
	canonical := b[:sigOff]

	r, err := decodeCanonical(canonical)
	if err != nil {
		return nil, err
	}

	// Require the input to be the canonical encoding of what it parsed to.
	//
	// Without this, one logical record has several wire forms — an overlong
	// varint is the easy one — and since the record ID is a hash of these
	// bytes, each form gets a DIFFERENT ID. Content-addressed dedup is what
	// stops a flooding mesh reprocessing the same record from every path
	// (§6.2), so a peer able to mint new IDs for existing records could defeat
	// it and force repeated work for records that will fail verification
	// anyway.
	//
	// Re-encoding and comparing makes this structural rather than a promise to
	// check each field individually: any future encoding subtlety is covered
	// by the same test.
	reencoded, err := r.canonicalBytes()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(reencoded, canonical) {
		return nil, errors.New("non-canonical record encoding: the same record has another wire form")
	}
	// Retain the exact bytes rather than re-encoding. This is what makes an
	// encoder change unable to invalidate stored history.
	r.signed = append([]byte(nil), canonical...)
	r.id = deriveID(r.signed)
	copy(r.sig[:], b[sigOff:])
	return r, nil
}

// decodeCanonical parses the signable representation. It mirrors
// canonicalBytes exactly; any divergence is a wire-format bug, which is why
// §12.2's round-trip property test exists and why §12.6 freezes vectors.
func decodeCanonical(b []byte) (*Record, error) {
	var r Record
	p := 0

	need := func(n int) error {
		if p+n > len(b) {
			return ErrTruncated
		}
		return nil
	}

	if err := need(3); err != nil {
		return nil, err
	}
	version := b[p]
	flags := b[p+1]
	r.Type = Type(b[p+2])
	p += 3

	if version != FormatVersion {
		// Pre-freeze policy (§7.1): any version mismatch is drop-and-log. There
		// are no compatibility promises between development releases.
		return nil, fmt.Errorf("unsupported record format version %d (this build speaks %d)",
			version, FormatVersion)
	}
	if !r.Type.Valid() {
		return nil, fmt.Errorf("unknown record type %d", uint8(r.Type))
	}
	if unknown := flags &^ flagHasParent; unknown != 0 {
		return nil, fmt.Errorf("unknown record flags 0x%02x", unknown)
	}

	if err := need(identity.NodeIDLen); err != nil {
		return nil, err
	}
	copy(r.Origin[:], b[p:p+identity.NodeIDLen])
	p += identity.NodeIDLen

	seq, n := binary.Uvarint(b[p:])
	if n <= 0 {
		return nil, fmt.Errorf("%w: bad seq varint", ErrTruncated)
	}
	r.Seq = seq
	p += n

	if err := need(4); err != nil {
		return nil, err
	}
	r.TS = binary.BigEndian.Uint32(b[p:])
	p += 4

	if err := need(4); err != nil {
		return nil, err
	}
	copy(r.Area[:], b[p:p+4])
	p += 4

	if flags&flagHasParent != 0 {
		if err := need(IDLen); err != nil {
			return nil, err
		}
		copy(r.Parent[:], b[p:p+IDLen])
		p += IDLen
	}

	bodyLen, n := binary.Uvarint(b[p:])
	if n <= 0 {
		return nil, fmt.Errorf("%w: bad body length varint", ErrTruncated)
	}
	p += n
	if bodyLen > MaxBodyLen {
		// Reject before allocating: this is the resource-exhaustion guard from
		// §6.2.1, and it must fire on the declared length, not after reading.
		return nil, fmt.Errorf("declared body length %d exceeds limit %d", bodyLen, MaxBodyLen)
	}
	if err := need(int(bodyLen)); err != nil {
		return nil, err
	}
	r.Body = append([]byte(nil), b[p:p+int(bodyLen)]...)
	p += int(bodyLen)

	if p != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after record body", len(b)-p)
	}
	return &r, nil
}
