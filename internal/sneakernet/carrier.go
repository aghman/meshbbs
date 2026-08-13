// Package sneakernet moves records between instances on removable media
// (design §7.5, §6.5 fetch path 2).
//
// # Why this exists at all
//
// `[D8]` settled that the mesh carries catalogs and never file bytes, and named
// exactly two fetch paths: direct IP from a holding BBS, or "queued for the next
// sneakernet exchange". The second one has never existed. §6.5 records the
// consequence honestly — v0.17 REMOVED the "queued for next exchange" wording
// from the file browser, because printing a promise nothing would keep is worse
// than the spinner that bullet warns against.
//
// It is also the only federation path available to an instance with neither a
// radio in range nor an IP route, which on a mesh network is not a corner case.
//
// # Why a container of bundles rather than a new format
//
// internal/bundle is the reviewed, canonical, decompression-bombed-defended
// unit of "some records from one area", and it is what the mesh already speaks.
// Defining a second record encoding for USB sticks would mean two formats to
// freeze at Phase 6 `[D10]` and two places for a canonicalisation bug to live.
//
// So a carrier is a list of bundles plus the sender's version vectors, and the
// bundle codec is untouched. What the container adds is the part a 233-byte
// datagram never needed: many areas at once, and a size bound appropriate to a
// medium with no MTU.
//
// # Why the vectors travel with the records
//
// Anti-entropy over a radio is a conversation — digest, vector, range request,
// records. A hand-off is not: somebody takes a stick away and brings it back
// another day, so there is no round trip to have. §6.5 already assumes this
// when it says a request is "satisfied at the next bundle exchange" rather than
// at this one.
//
// Carrying the sender's vectors makes the asymmetry work. A carrier says both
// "here is what I have" and "here is what I know", so the receiver can write a
// reply on the same stick without ever having spoken to the sender. Two trips,
// which is what the design already described.
//
// # The threat model is worse than a peer
//
// A hostile peer is rate-limited by the channel and has to get past a link. A
// file on removable media has neither: an attacker chooses the bytes AND the
// size, and a sysop plugs it in on purpose. Every count and length here is
// bounded before it is allocated against, and the bundle codec's existing
// MaxDecompressed does the rest.
//
// Nothing in the container is signed and nothing needs to be. Every record
// inside carries its own signature and is verified on the way into the log, so
// a forged carrier cannot introduce a record its claimed origin did not write.
// The origin and vector fields are hints about what to send back; the worst a
// liar achieves is that somebody wastes a trip.
package sneakernet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

// Magic identifies a carrier file.
//
// A magic number rather than trusting the extension, because the input is a
// file somebody found on a stick and the first question is whether it is one of
// ours at all. Getting a clear "this is not a meshbbs carrier" beats a
// truncation error from halfway through a length field.
var Magic = [4]byte{'M', 'B', 'X', 'F'}

// FormatVersion is the carrier envelope's version, independent of the bundle
// format inside it (bundle.FormatVersion) and of the record format inside that.
// Three layers, three versions: a carrier can gain a field without touching
// what the mesh speaks.
const FormatVersion uint8 = 1

// Limits. See the package comment on why these are stricter in spirit than the
// mesh's: the medium has no MTU and no rate limit.
const (
	// MaxBundles in one carrier. At bundle.MaxRecords each, that is 262,144
	// records — far past any real exchange, which is the point of a bound.
	MaxBundles = 1024
	// MaxVectors is one per area the sender federates.
	MaxVectors = 256
	// MaxCarrierBytes bounds the whole file before anything is parsed.
	//
	// 64 MiB is generous for records — the largest legal bundle is a megabyte
	// decompressed and they compress hard — and deliberately not "whatever fits
	// on the stick". A carrier is read into memory to be verified, so its
	// ceiling is a memory ceiling.
	MaxCarrierBytes = 64 << 20
)

var (
	ErrNotACarrier = errors.New("not a meshbbs carrier file")
	ErrTruncated   = errors.New("truncated carrier")
	ErrTooMany     = errors.New("carrier exceeds its declared limits")
	ErrTooLarge    = errors.New("carrier is larger than the allowed size")
	// ErrNotCanonical mirrors the bundle codec's rule: one logical carrier has
	// exactly one wire form, so a re-encode must reproduce the input.
	ErrNotCanonical = errors.New("non-canonical carrier encoding")
)

// Carrier is what a stick holds.
type Carrier struct {
	// Origin is the node that wrote this. A claim, not a credential — see the
	// package comment.
	Origin identity.NodeID
	// CreatedAt is when, in seconds. Advisory in the same sense a record's TS
	// is (§6.2.1): it came from another machine's clock and is rendered, never
	// computed from.
	CreatedAt uint32
	// Vectors say what the sender held per area when it wrote this, so a
	// receiver can compute a reply without a conversation.
	Vectors map[record.AreaTag]*vv.Vector
	// Bundles are the records themselves, still in the mesh's own format.
	Bundles [][]byte
}

// Encode serialises a carrier.
//
// Layout:
//
//	magic(4) | version(1) | origin(8) | created(4)
//	vectorCount(uvarint) | (area(4) | len(uvarint) | vectorBytes)*
//	bundleCount(uvarint) | (len(uvarint) | bundleBytes)*
//
// Vectors are written in area order and bundles in the order given. Sorting the
// vectors is what makes the encoding canonical: they arrive from a map, and Go
// randomises map iteration (§6.2.1 rule 2), so an unsorted walk would give one
// carrier a different form on every write.
func Encode(c *Carrier) ([]byte, error) {
	if len(c.Vectors) > MaxVectors {
		return nil, fmt.Errorf("%w: %d vectors, limit is %d", ErrTooMany, len(c.Vectors), MaxVectors)
	}
	if len(c.Bundles) > MaxBundles {
		return nil, fmt.Errorf("%w: %d bundles, limit is %d", ErrTooMany, len(c.Bundles), MaxBundles)
	}

	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = append(buf, FormatVersion)
	buf = append(buf, c.Origin[:]...)
	buf = binary.BigEndian.AppendUint32(buf, c.CreatedAt)

	areas := make([]record.AreaTag, 0, len(c.Vectors))
	for a := range c.Vectors {
		areas = append(areas, a)
	}
	sortAreas(areas)

	buf = binary.AppendUvarint(buf, uint64(len(areas)))
	for _, a := range areas {
		enc := c.Vectors[a].Encode()
		buf = append(buf, a[:]...)
		buf = binary.AppendUvarint(buf, uint64(len(enc)))
		buf = append(buf, enc...)
	}

	buf = binary.AppendUvarint(buf, uint64(len(c.Bundles)))
	for _, b := range c.Bundles {
		if len(b) == 0 {
			return nil, errors.New("carrier holds an empty bundle")
		}
		buf = binary.AppendUvarint(buf, uint64(len(b)))
		buf = append(buf, b...)
	}

	if len(buf) > MaxCarrierBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(buf))
	}
	return buf, nil
}

// Decode parses a carrier, bounding every allocation before making it.
func Decode(data []byte) (*Carrier, error) {
	if len(data) > MaxCarrierBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit is %d", ErrTooLarge, len(data), MaxCarrierBytes)
	}
	rest := data

	take := func(n int, what string) ([]byte, error) {
		if len(rest) < n {
			return nil, fmt.Errorf("%w: ended inside %s", ErrTruncated, what)
		}
		out := rest[:n]
		rest = rest[n:]
		return out, nil
	}
	uvarint := func(what string) (uint64, error) {
		v, n := binary.Uvarint(rest)
		if n <= 0 {
			return 0, fmt.Errorf("%w: unreadable %s", ErrTruncated, what)
		}
		// binary.Uvarint accepts overlong forms, which would give one carrier
		// two wire forms. Caught structurally by the re-encode at the end, the
		// same defence the record and bundle codecs use.
		rest = rest[n:]
		return v, nil
	}

	magic, err := take(4, "the magic")
	if err != nil {
		return nil, ErrNotACarrier
	}
	if !bytes.Equal(magic, Magic[:]) {
		return nil, fmt.Errorf("%w (magic is %x)", ErrNotACarrier, magic)
	}
	version, err := take(1, "the version")
	if err != nil {
		return nil, err
	}
	if version[0] != FormatVersion {
		return nil, fmt.Errorf("%w: carrier format version %d, this build speaks %d",
			ErrNotACarrier, version[0], FormatVersion)
	}

	c := &Carrier{Vectors: map[record.AreaTag]*vv.Vector{}}
	origin, err := take(identity.NodeIDLen, "the origin")
	if err != nil {
		return nil, err
	}
	copy(c.Origin[:], origin)
	created, err := take(4, "the timestamp")
	if err != nil {
		return nil, err
	}
	c.CreatedAt = binary.BigEndian.Uint32(created)

	vectorCount, err := uvarint("the vector count")
	if err != nil {
		return nil, err
	}
	if vectorCount > MaxVectors {
		return nil, fmt.Errorf("%w: claims %d vectors, limit is %d", ErrTooMany, vectorCount, MaxVectors)
	}
	for i := uint64(0); i < vectorCount; i++ {
		tag, err := take(4, "an area tag")
		if err != nil {
			return nil, err
		}
		n, err := uvarint("a vector length")
		if err != nil {
			return nil, err
		}
		if n > uint64(len(rest)) {
			return nil, fmt.Errorf("%w: a vector claims %d bytes, %d remain", ErrTruncated, n, len(rest))
		}
		body, err := take(int(n), "a vector")
		if err != nil {
			return nil, err
		}
		vec, err := vv.Decode(body)
		if err != nil {
			return nil, fmt.Errorf("carrier vector %d: %w", i, err)
		}
		var a record.AreaTag
		copy(a[:], tag)
		if _, dup := c.Vectors[a]; dup {
			// Two vectors for one area is not a thing a writer produces, and
			// silently keeping the last would make the carrier's meaning depend
			// on parse order.
			return nil, fmt.Errorf("%w: area %x appears twice", ErrNotCanonical, a[:])
		}
		c.Vectors[a] = vec
	}

	bundleCount, err := uvarint("the bundle count")
	if err != nil {
		return nil, err
	}
	if bundleCount > MaxBundles {
		return nil, fmt.Errorf("%w: claims %d bundles, limit is %d", ErrTooMany, bundleCount, MaxBundles)
	}
	for i := uint64(0); i < bundleCount; i++ {
		n, err := uvarint("a bundle length")
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, errors.New("carrier holds an empty bundle")
		}
		if n > uint64(len(rest)) {
			return nil, fmt.Errorf("%w: a bundle claims %d bytes, %d remain", ErrTruncated, n, len(rest))
		}
		body, err := take(int(n), "a bundle")
		if err != nil {
			return nil, err
		}
		// Copied rather than aliased: the caller may hold these after the input
		// slice is reused, and a bundle that mutates underneath the log is a
		// bug nobody would find.
		c.Bundles = append(c.Bundles, append([]byte(nil), body...))
	}

	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrNotCanonical, len(rest))
	}

	// One carrier, one wire form. Same structural defence the record and bundle
	// codecs apply, and the one that catches an overlong uvarint without a
	// minimality check per field.
	reencoded, err := Encode(c)
	if err != nil {
		return nil, fmt.Errorf("carrier did not survive re-encoding: %w", err)
	}
	if !bytes.Equal(reencoded, data) {
		return nil, ErrNotCanonical
	}
	return c, nil
}

// sortAreas orders area tags, so a carrier built from a map is deterministic.
func sortAreas(areas []record.AreaTag) {
	for i := 1; i < len(areas); i++ {
		for j := i; j > 0 && bytes.Compare(areas[j][:], areas[j-1][:]) < 0; j-- {
			areas[j], areas[j-1] = areas[j-1], areas[j]
		}
	}
}
