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

	"github.com/aghman/meshbbs/internal/blobstore"
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
//
// Version 2 added the requests section (§6.5 fetch path 2). Bumped rather than
// quietly extended, even though nothing has frozen yet and `[D10]`'s freeze is
// a Phase 6 deliverable: a v1 carrier fed to this build would otherwise die
// inside the request count with a truncation error, and "carrier format version
// 1, this build speaks 2" is what a sysop holding a stick from a board on an
// older build can act on.
//
// Version 3 added MinDictionary, for the same reason one layer down. §7.4's
// dictionary negotiation works on the mesh because a digest arrives before the
// traffic does — and it can never work here, because a stick has no round trip
// and the person carrying it may be walking toward a board this one has never
// met. So the requirement is declared up front instead of negotiated, and the
// far end can refuse in one sentence before parsing anything.
const FormatVersion uint8 = 3

// Limits. See the package comment on why these are stricter in spirit than the
// mesh's: the medium has no MTU and no rate limit.
const (
	// MaxBundles in one carrier. At bundle.MaxRecords each, that is 262,144
	// records — far past any real exchange, which is the point of a bound.
	MaxBundles = 1024
	// MaxVectors is one per area the sender federates.
	MaxVectors = 256
	// MaxBlobRefs bounds how many files one exchange carries.
	MaxBlobRefs = 256
	// MaxRequests bounds how many files one carrier may ask for.
	//
	// Matched to MaxBlobRefs on purpose: asking for more than a carrier could
	// possibly answer in one trip is not a request, it is a wish. A queue
	// longer than this drains oldest-first over successive hand-offs, which is
	// what store.OpenFileRequestHashes orders for.
	MaxRequests = MaxBlobRefs
	// MaxBlobBytes bounds one file's contents.
	//
	// Blobs are the one thing here that does not fit in memory and is not meant
	// to: the bodies stream through blobstore.Put, which hashes as it writes. So
	// this is a disk and patience bound rather than a memory one, and it is
	// generous because the whole point of sneakernet is the files the mesh will
	// never carry (§7.5).
	MaxBlobBytes = 256 << 20
	// MaxCarrierBytes bounds the MANIFEST before anything is parsed.
	//
	// 64 MiB is generous for records — the largest legal bundle is a megabyte
	// decompressed and they compress hard — and deliberately not "whatever fits
	// on the stick". The manifest is read into memory to be verified, so its
	// ceiling is a memory ceiling. File bodies are NOT counted here; they never
	// enter memory.
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
	// MinDictionary is the highest compression dictionary any bundle in here
	// was packed with, and therefore the lowest a reader must hold (§7.4).
	//
	// # Why a stick declares instead of negotiating
	//
	// On the mesh a node learns what its peers can read from their digests and
	// picks a dictionary everybody holds. A carrier has no peers and no round
	// trip: it is written once, carried by a human, and handed to a board that
	// may never have been heard from. There is nothing to negotiate with, ever,
	// so the only honest options are to declare the requirement or to guess.
	//
	// Declaring it costs one byte on a medium with no MTU and turns the failure
	// from "truncated bundle" somewhere inside an import into "this carrier
	// needs dictionary 2, this build holds up to 1" before a single record is
	// read. That distinction is the whole reason for the version bump — the
	// per-bundle dictionary ID was already on the wire, but only reachable
	// after committing to the parse.
	MinDictionary uint8
	// Vectors say what the sender held per area when it wrote this, so a
	// receiver can compute a reply without a conversation.
	Vectors map[record.AreaTag]*vv.Vector
	// Bundles are the records themselves, still in the mesh's own format.
	Bundles [][]byte
	// Blobs are the file bodies that FOLLOW this manifest, in this order.
	//
	// # Why file bytes may ride here and nowhere else
	//
	// §7.5 forbids file content in a record at the type level — a FILE record's
	// body must parse as a catalog entry, and every field of one is bounded, so
	// there is nowhere to put content and no threshold to relax. That rule is
	// absolute and is not weakened here: a blob is not in a record and not in a
	// bundle, it is a separate section of a file on a stick.
	//
	// Which is exactly what `[D8]` said: "catalogs replicate; bytes move over IP
	// or sneakernet". This is the second of those two, and the asymmetry is the
	// design's rather than an exception to it — a stick has no airtime.
	//
	// Only the REFERENCES are in the manifest. Declaring what follows before it
	// arrives is what lets a receiver refuse a 200 GB stick without reading it,
	// which is the streaming form of bounding a length before allocating.
	Blobs []BlobRef

	// Requests are the files this carrier's writer wants back (§6.5 path 2).
	//
	// # Why a request is a hash and nothing else
	//
	// The writer does not hold the file. All it has ever seen is the FILE
	// record somebody announced, which carries a truncated BLAKE3 rather than
	// all 32 bytes — so the truncation is not a choice made here, it is the
	// whole of what the requester knows.
	//
	// It is also the stronger way to ask. "Send me utils/kermit.zip" is
	// answered by whatever the holder happens to have filed under that name;
	// "send me the bytes hashing to this" is answered by the bytes or not at
	// all, and the receiver re-hashes on the way in. Content addressing is
	// what makes an unsigned container safe, and it makes the request safe for
	// the same reason.
	//
	// A carrier that asks for nothing is the ordinary case, and asking costs
	// 16 bytes on a medium with no airtime budget — so this rides on both
	// legs, outward and reply, rather than being a mode.
	Requests []WireHash
}

// WireHash is a content hash as a request names it: BLAKE3 truncated to what a
// FILE record carries.
//
// An alias rather than a defined type, so a record.FileBody's hash is one of
// these without a conversion. The two are the same thing and giving them
// different names would invite a build that keeps them in step by hand.
type WireHash = [record.FileHashLen]byte

// BlobRef names a file body carried after the manifest.
//
// The hash is the identity, not a label: a receiver recomputes it while
// streaming and refuses a mismatch. That is what makes an unsigned container
// safe to take file bytes from — a carrier cannot lie about what a blob is,
// only about which blobs it has.
type BlobRef struct {
	Hash blobstore.Hash
	Size uint64
}

// Encode serialises a carrier.
//
// Layout:
//
//	magic(4) | version(1) | origin(8) | created(4)
//	vectorCount(uvarint) | (area(4) | len(uvarint) | vectorBytes)*
//	bundleCount(uvarint) | (len(uvarint) | bundleBytes)*
//	blobCount(uvarint) | (hash(32) | size(uvarint))*
//	requestCount(uvarint) | hash(16)*
//
// Vectors are written in area order and bundles in the order given. Sorting the
// vectors is what makes the encoding canonical: they arrive from a map, and Go
// randomises map iteration (§6.2.1 rule 2), so an unsorted walk would give one
// carrier a different form on every write.
//
// Requests come last, after the blob references, because the bodies follow the
// manifest and the section that describes them should be the one nearest to
// them. A reader that stops early has the declarations it needs to bound what
// is coming; the requests are for the writer of the NEXT carrier, who is in no
// hurry.
func Encode(c *Carrier) ([]byte, error) {
	if len(c.Vectors) > MaxVectors {
		return nil, fmt.Errorf("%w: %d vectors, limit is %d", ErrTooMany, len(c.Vectors), MaxVectors)
	}
	if len(c.Bundles) > MaxBundles {
		return nil, fmt.Errorf("%w: %d bundles, limit is %d", ErrTooMany, len(c.Bundles), MaxBundles)
	}
	if len(c.Blobs) > MaxBlobRefs {
		return nil, fmt.Errorf("%w: %d blobs, limit is %d", ErrTooMany, len(c.Blobs), MaxBlobRefs)
	}
	if len(c.Requests) > MaxRequests {
		return nil, fmt.Errorf("%w: %d requests, limit is %d", ErrTooMany, len(c.Requests), MaxRequests)
	}

	var buf []byte
	buf = append(buf, Magic[:]...)
	buf = append(buf, FormatVersion)
	buf = append(buf, c.Origin[:]...)
	buf = binary.BigEndian.AppendUint32(buf, c.CreatedAt)
	buf = append(buf, c.MinDictionary)

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

	buf = binary.AppendUvarint(buf, uint64(len(c.Blobs)))
	for _, b := range c.Blobs {
		if b.Hash.IsZero() {
			return nil, errors.New("carrier declares a blob with no hash")
		}
		if b.Size > MaxBlobBytes {
			return nil, fmt.Errorf("%w: a blob declares %d bytes, limit is %d",
				ErrTooMany, b.Size, MaxBlobBytes)
		}
		buf = append(buf, b.Hash[:]...)
		buf = binary.AppendUvarint(buf, b.Size)
	}

	buf = binary.AppendUvarint(buf, uint64(len(c.Requests)))
	seen := make(map[WireHash]bool, len(c.Requests))
	for _, h := range c.Requests {
		if h == (WireHash{}) {
			return nil, errors.New("carrier asks for a file with no hash")
		}
		// One logical carrier, one wire form: asking for the same hash twice
		// is the same request written down twice, and permitting it would let
		// two encodings mean the same thing. The caller de-duplicates by
		// asking the store for distinct hashes; this is the structural check
		// that says so.
		if seen[h] {
			return nil, fmt.Errorf("%w: a request appears twice (%x)", ErrNotCanonical, h[:4])
		}
		seen[h] = true
		buf = append(buf, h[:]...)
	}

	if len(buf) > MaxCarrierBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(buf))
	}
	return buf, nil
}

// Decode parses a standalone carrier manifest, bounding every allocation before
// making it. Trailing bytes are refused: one carrier, one wire form.
func Decode(data []byte) (*Carrier, error) {
	c, _, err := decode(data, false)
	return c, err
}

// decode parses a manifest, optionally permitting bytes after it, and reports
// how many it used.
//
// allowTrailing exists for exactly one caller: a carrier file, where the file
// BODIES follow the manifest and trailing bytes are the point rather than a
// defect. The canonical check still applies — to the prefix — so relaxing the
// rule here does not relax it for the manifest itself.
func decode(data []byte, allowTrailing bool) (*Carrier, int, error) {
	if len(data) > MaxCarrierBytes {
		return nil, 0, fmt.Errorf("%w: %d bytes, limit is %d", ErrTooLarge, len(data), MaxCarrierBytes)
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
		return nil, 0, ErrNotACarrier
	}
	if !bytes.Equal(magic, Magic[:]) {
		return nil, 0, fmt.Errorf("%w (magic is %x)", ErrNotACarrier, magic)
	}
	version, err := take(1, "the version")
	if err != nil {
		return nil, 0, err
	}
	if version[0] != FormatVersion {
		return nil, 0, fmt.Errorf("%w: carrier format version %d, this build speaks %d",
			ErrNotACarrier, version[0], FormatVersion)
	}

	c := &Carrier{Vectors: map[record.AreaTag]*vv.Vector{}}
	origin, err := take(identity.NodeIDLen, "the origin")
	if err != nil {
		return nil, 0, err
	}
	copy(c.Origin[:], origin)
	created, err := take(4, "the timestamp")
	if err != nil {
		return nil, 0, err
	}
	c.CreatedAt = binary.BigEndian.Uint32(created)

	minDict, err := take(1, "the minimum dictionary")
	if err != nil {
		return nil, 0, err
	}
	c.MinDictionary = minDict[0]

	vectorCount, err := uvarint("the vector count")
	if err != nil {
		return nil, 0, err
	}
	if vectorCount > MaxVectors {
		return nil, 0, fmt.Errorf("%w: claims %d vectors, limit is %d", ErrTooMany, vectorCount, MaxVectors)
	}
	for i := uint64(0); i < vectorCount; i++ {
		tag, err := take(4, "an area tag")
		if err != nil {
			return nil, 0, err
		}
		n, err := uvarint("a vector length")
		if err != nil {
			return nil, 0, err
		}
		if n > uint64(len(rest)) {
			return nil, 0, fmt.Errorf("%w: a vector claims %d bytes, %d remain", ErrTruncated, n, len(rest))
		}
		body, err := take(int(n), "a vector")
		if err != nil {
			return nil, 0, err
		}
		vec, err := vv.Decode(body)
		if err != nil {
			return nil, 0, fmt.Errorf("carrier vector %d: %w", i, err)
		}
		var a record.AreaTag
		copy(a[:], tag)
		if _, dup := c.Vectors[a]; dup {
			// Two vectors for one area is not a thing a writer produces, and
			// silently keeping the last would make the carrier's meaning depend
			// on parse order.
			return nil, 0, fmt.Errorf("%w: area %x appears twice", ErrNotCanonical, a[:])
		}
		c.Vectors[a] = vec
	}

	bundleCount, err := uvarint("the bundle count")
	if err != nil {
		return nil, 0, err
	}
	if bundleCount > MaxBundles {
		return nil, 0, fmt.Errorf("%w: claims %d bundles, limit is %d", ErrTooMany, bundleCount, MaxBundles)
	}
	for i := uint64(0); i < bundleCount; i++ {
		n, err := uvarint("a bundle length")
		if err != nil {
			return nil, 0, err
		}
		if n == 0 {
			return nil, 0, errors.New("carrier holds an empty bundle")
		}
		if n > uint64(len(rest)) {
			return nil, 0, fmt.Errorf("%w: a bundle claims %d bytes, %d remain", ErrTruncated, n, len(rest))
		}
		body, err := take(int(n), "a bundle")
		if err != nil {
			return nil, 0, err
		}
		// Copied rather than aliased: the caller may hold these after the input
		// slice is reused, and a bundle that mutates underneath the log is a
		// bug nobody would find.
		c.Bundles = append(c.Bundles, append([]byte(nil), body...))
	}

	blobCount, err := uvarint("the blob count")
	if err != nil {
		return nil, 0, err
	}
	if blobCount > MaxBlobRefs {
		return nil, 0, fmt.Errorf("%w: claims %d blobs, limit is %d", ErrTooMany, blobCount, MaxBlobRefs)
	}
	for i := uint64(0); i < blobCount; i++ {
		h, err := take(blobstore.HashLen, "a blob hash")
		if err != nil {
			return nil, 0, err
		}
		size, err := uvarint("a blob size")
		if err != nil {
			return nil, 0, err
		}
		if size > MaxBlobBytes {
			return nil, 0, fmt.Errorf("%w: blob %d declares %d bytes, limit is %d",
				ErrTooMany, i, size, MaxBlobBytes)
		}
		var ref BlobRef
		copy(ref.Hash[:], h)
		if ref.Hash.IsZero() {
			return nil, 0, errors.New("carrier declares a blob with no hash")
		}
		ref.Size = size
		c.Blobs = append(c.Blobs, ref)
	}

	requestCount, err := uvarint("the request count")
	if err != nil {
		return nil, 0, err
	}
	if requestCount > MaxRequests {
		return nil, 0, fmt.Errorf("%w: asks for %d files, limit is %d",
			ErrTooMany, requestCount, MaxRequests)
	}
	for i := uint64(0); i < requestCount; i++ {
		h, err := take(record.FileHashLen, "a request hash")
		if err != nil {
			return nil, 0, err
		}
		var want WireHash
		copy(want[:], h)
		if want == (WireHash{}) {
			return nil, 0, errors.New("carrier asks for a file with no hash")
		}
		c.Requests = append(c.Requests, want)
	}

	consumed := len(data) - len(rest)
	if len(rest) != 0 && !allowTrailing {
		return nil, 0, fmt.Errorf("%w: %d trailing bytes", ErrNotCanonical, len(rest))
	}

	// One carrier, one wire form. Same structural defence the record and bundle
	// codecs apply, and the one that catches an overlong uvarint without a
	// minimality check per field.
	reencoded, err := Encode(c)
	if err != nil {
		return nil, 0, fmt.Errorf("carrier did not survive re-encoding: %w", err)
	}
	if !bytes.Equal(reencoded, data[:consumed]) {
		return nil, 0, ErrNotCanonical
	}
	return c, consumed, nil
}

// sortAreas orders area tags, so a carrier built from a map is deterministic.
func sortAreas(areas []record.AreaTag) {
	for i := 1; i < len(areas); i++ {
		for j := i; j > 0 && bytes.Compare(areas[j][:], areas[j-1][:]) < 0; j-- {
			areas[j], areas[j-1] = areas[j-1], areas[j]
		}
	}
}
