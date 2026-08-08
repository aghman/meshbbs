package record

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

// MaxFileNameLen bounds a file's name.
//
// This is the canonical definition — the store's column CHECK follows it —
// because the name travels in every FILE record and the wire is the tighter
// constraint. 64 bytes is generous next to a DOS 8.3 name and is the largest
// single text field a catalog entry carries.
const MaxFileNameLen = 64

// MaxFileDescLen bounds a file's description.
//
// Legacy BBS FILES.BBS descriptions ran to about 45 characters and said plenty.
// 80 is generous by that standard and deliberately not more: at ~10 originated
// packets per node per day (§1.1), description length trades directly against
// how many files a node can announce at all. A description that doubles the
// cost of a catalog entry is one fewer entry.
const MaxFileDescLen = 80

// MaxFileTags and MaxFileTagLen bound the tag list.
//
// Small because nothing populates tags yet and the budget is the budget. They
// exist because §6.5 specifies them and an absent list costs one byte; the
// limits can rise before the Phase 6 freeze if something starts setting them.
const (
	MaxFileTags   = 3
	MaxFileTagLen = 12
)

// FileHashLen is the width of the content hash a FILE record carries.
//
// TRUNCATED from the blob store's full 32-byte BLAKE3, for the same reason
// record IDs are 16 bytes: on a 233-byte MTU, 16 recovered bytes is 7% of a
// packet, and 128 bits is ample against accidental collision at any scale this
// network will reach.
//
// The honest cost: a peer willing to spend ~2^64 work could mint a FILE record
// whose truncated hash matches content we already hold, and we would then
// believe we hold their file. That is a mislabelled download, not a signature
// forgery or a way into the log — the record is still node-signed, and the
// blob store still verifies against the full hash locally. Worth the 16 bytes.
const FileHashLen = 16

// FileBody is the payload of a FILE record (§6.5).
//
// # What it does NOT carry
//
// The holding node. Every record already has an Origin, and a FILE record's
// origin IS the BBS holding the file — so a "held by" field would be eight
// bytes restating what the envelope says, on a link where §7.5 measures
// everything in bytes. The catalog UI reads Origin.
//
// The file's CONTENT, at any size. That is not an omission to be revisited: it
// is the §7.5 rule, and there is deliberately no record type that could carry
// it.
type FileBody struct {
	// Name is the filename within its area, unique there.
	Name string

	// Size is the content length in bytes. Advisory in the same sense a record
	// TS is: it describes content this node may not hold, so a receiver renders
	// it and does not compute from it.
	Size uint64

	// Hash is BLAKE3(content) truncated to FileHashLen. It is what lets any
	// receiving BBS answer "do I hold this?" without asking anyone (§6.5).
	Hash [FileHashLen]byte

	// Description is free text, possibly empty. Empty costs one byte.
	Description string

	// Tags are short labels for filtering. Nothing populates these yet — the
	// field is here because §6.5 specifies it and an absent tag list costs one
	// byte, which is cheaper than a wire-format change after the Phase 6 freeze.
	Tags []string
}

// FileBodyLen reports the encoded size of a body.
func FileBodyLen(f FileBody) int {
	n := 1 + len(f.Name) +
		uvarintLen(f.Size) +
		FileHashLen +
		1 + len(f.Description) +
		1
	for _, t := range f.Tags {
		n += 1 + len(t)
	}
	return n
}

// uvarintLen is how many bytes binary.AppendUvarint will use.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// MarshalFileBody encodes a FileBody.
//
// Layout: len(name) | name | size(uvarint) | hash(16) | len(desc) | desc |
// tagCount | (len(tag) | tag)*
//
// There is no flags byte. An absent description and an absent tag list are
// each a single zero length, which costs exactly what a flag bit would have
// and cannot disagree with the field it describes — two encodings of "no
// description" is the second wire form this codebase has now been bitten by
// five times.
func MarshalFileBody(f FileBody) ([]byte, error) {
	if f.Name == "" {
		return nil, fmt.Errorf("file name is empty")
	}
	if err := validateLabel("file name", f.Name, MaxFileNameLen); err != nil {
		return nil, err
	}
	// Path separators would let a name address something other than an entry in
	// its own area once a receiver renders it into a path. Refused at the
	// codec, so no consumer has to think about it.
	for _, r := range f.Name {
		if r == '/' || r == '\\' {
			return nil, fmt.Errorf("file name may not contain %q", r)
		}
	}
	if f.Name == "." || f.Name == ".." {
		return nil, fmt.Errorf("%q is not a file name", f.Name)
	}
	if err := validateLabel("file description", f.Description, MaxFileDescLen); err != nil {
		return nil, err
	}
	if f.Hash == ([FileHashLen]byte{}) {
		// An all-zero hash addresses nothing. Refusing it here means a catalog
		// entry can never advertise content no BBS could ever match.
		return nil, fmt.Errorf("file hash is all zero")
	}
	if len(f.Tags) > MaxFileTags {
		return nil, fmt.Errorf("file has %d tags, limit is %d", len(f.Tags), MaxFileTags)
	}
	for _, t := range f.Tags {
		if t == "" {
			return nil, fmt.Errorf("file tag is empty")
		}
		if err := validateLabel("file tag", t, MaxFileTagLen); err != nil {
			return nil, err
		}
	}

	buf := make([]byte, 0, FileBodyLen(f))
	buf = append(buf, byte(len(f.Name)))
	buf = append(buf, f.Name...)
	buf = binary.AppendUvarint(buf, f.Size)
	buf = append(buf, f.Hash[:]...)
	buf = append(buf, byte(len(f.Description)))
	buf = append(buf, f.Description...)
	buf = append(buf, byte(len(f.Tags)))
	for _, t := range f.Tags {
		buf = append(buf, byte(len(t)))
		buf = append(buf, t...)
	}
	return buf, nil
}

// UnmarshalFileBody parses a FILE record body.
func UnmarshalFileBody(b []byte) (FileBody, error) {
	var f FileBody
	rest := b

	take := func(n int, what string) ([]byte, error) {
		if len(rest) < n {
			return nil, fmt.Errorf("%w: FILE body ended inside %s", ErrTruncated, what)
		}
		out := rest[:n]
		rest = rest[n:]
		return out, nil
	}

	nameLen, err := take(1, "the name length")
	if err != nil {
		return f, err
	}
	if nameLen[0] == 0 {
		return f, fmt.Errorf("file name is empty")
	}
	name, err := take(int(nameLen[0]), "the name")
	if err != nil {
		return f, err
	}
	f.Name = string(name)

	size, n := binary.Uvarint(rest)
	if n <= 0 {
		return FileBody{}, fmt.Errorf("%w: FILE body has no readable size", ErrTruncated)
	}
	rest = rest[n:]
	f.Size = size

	hash, err := take(FileHashLen, "the hash")
	if err != nil {
		return FileBody{}, err
	}
	copy(f.Hash[:], hash)

	descLen, err := take(1, "the description length")
	if err != nil {
		return FileBody{}, err
	}
	desc, err := take(int(descLen[0]), "the description")
	if err != nil {
		return FileBody{}, err
	}
	f.Description = string(desc)

	tagCount, err := take(1, "the tag count")
	if err != nil {
		return FileBody{}, err
	}
	if int(tagCount[0]) > MaxFileTags {
		return FileBody{}, fmt.Errorf("FILE body claims %d tags, limit is %d",
			tagCount[0], MaxFileTags)
	}
	for i := 0; i < int(tagCount[0]); i++ {
		tagLen, err := take(1, "a tag length")
		if err != nil {
			return FileBody{}, err
		}
		if tagLen[0] == 0 {
			return FileBody{}, fmt.Errorf("file tag is empty")
		}
		tag, err := take(int(tagLen[0]), "a tag")
		if err != nil {
			return FileBody{}, err
		}
		f.Tags = append(f.Tags, string(tag))
	}

	// Trailing bytes are a second encoding of the same record, and the ID is
	// derived from the bytes rather than the fields — so accepting them would
	// let one logical file have two content addresses.
	if len(rest) != 0 {
		return FileBody{}, fmt.Errorf("FILE body has %d trailing bytes", len(rest))
	}

	// Validate AFTER parsing, so a hostile body is rejected by the same rules
	// that governed writing it rather than by whatever the reader assumes.
	if err := validateLabel("file name", f.Name, MaxFileNameLen); err != nil {
		return FileBody{}, err
	}
	for _, r := range f.Name {
		if r == '/' || r == '\\' {
			return FileBody{}, fmt.Errorf("file name may not contain %q", r)
		}
	}
	if f.Name == "." || f.Name == ".." {
		return FileBody{}, fmt.Errorf("%q is not a file name", f.Name)
	}
	if err := validateLabel("file description", f.Description, MaxFileDescLen); err != nil {
		return FileBody{}, err
	}
	if f.Hash == ([FileHashLen]byte{}) {
		return FileBody{}, fmt.Errorf("file hash is all zero")
	}
	for _, t := range f.Tags {
		if err := validateLabel("file tag", t, MaxFileTagLen); err != nil {
			return FileBody{}, err
		}
	}

	// Require the input to be the canonical encoding of what it parsed to —
	// the same structural defence decodeCanonical applies to the envelope, and
	// for the same reason one layer down.
	//
	// binary.Uvarint accepts overlong forms, so a size of 0 encoded as 80 00
	// parses fine and re-encodes as 00: one logical file with two wire forms,
	// and since the record ID is a hash of the bytes, two content addresses for
	// one file that anti-entropy would carry separately forever. Found by
	// FuzzUnmarshalFileBody within seconds of it existing.
	//
	// Comparing re-encoded bytes covers any future subtlety as well, rather
	// than adding a minimality check per varint and hoping that is all of them.
	reencoded, err := MarshalFileBody(f)
	if err != nil {
		return FileBody{}, fmt.Errorf("FILE body did not survive re-encoding: %w", err)
	}
	if !bytes.Equal(reencoded, b) {
		return FileBody{}, fmt.Errorf(
			"non-canonical FILE body: the same entry has another wire form")
	}
	return f, nil
}

// TruncateFileHash narrows a full content hash to what the wire carries.
//
// Takes a slice rather than the blobstore's Hash type because this package sits
// below it: record is the wire format and must not depend on where bytes are
// stored.
func TruncateFileHash(full []byte) ([FileHashLen]byte, error) {
	var h [FileHashLen]byte
	if len(full) < FileHashLen {
		return h, fmt.Errorf("hash is %d bytes, need at least %d", len(full), FileHashLen)
	}
	copy(h[:], full[:FileHashLen])
	return h, nil
}

// NewFileRecord builds a FILE record signed by the NODE key.
//
// Signed by the node for the same reason a PROFILE is (§6.2, [D5]): users have
// no signing keys, and what the record asserts is "this instance holds this
// file", which is a claim only the instance can make and only it could be held
// to.
//
// The area is the FILE AREA's own tag. File areas and message areas share one
// tag namespace precisely so this needs no separate arm — to the sync engine a
// file area is an area, and its catalog reconciles by the same anti-entropy.
func NewFileRecord(key identity.NodeKey, seq uint64, ts uint32, area AreaTag, f FileBody) (*Record, error) {
	body, err := MarshalFileBody(f)
	if err != nil {
		return nil, err
	}
	return New(key, Record{
		Origin: key.ID(),
		Seq:    seq,
		TS:     ts,
		Type:   TypeFile,
		Area:   area,
		Body:   body,
	})
}

// VerifyFileRecord validates a FILE against its origin node's key.
func VerifyFileRecord(r *Record, originPub []byte) (FileBody, error) {
	if r.Type != TypeFile {
		return FileBody{}, fmt.Errorf("record %s is a %s, not a FILE", r.ID(), r.Type)
	}
	body, err := UnmarshalFileBody(r.Body)
	if err != nil {
		return FileBody{}, fmt.Errorf("FILE %s: %w", r.ID(), err)
	}
	if err := r.Verify(originPub); err != nil {
		return FileBody{}, fmt.Errorf("FILE %s: %w", r.ID(), err)
	}
	return body, nil
}

// FileSize reports the uncompressed on-wire cost of a catalog entry.
//
// Exported for the same reason ProfileSize is: §6.5's "roughly 120-200 bytes
// compressed" is an arithmetic claim about the airtime budget, and a reader
// should be able to check it rather than take it on trust. §12.7 asserts it.
func FileSize(f FileBody) int {
	// Record framing plus the body; the signature dominates, as everywhere.
	return 64 + 8 + 8 + 4 + 1 + 4 + FileBodyLen(f)
}

// checkNoFileContent enforces §7.5 at the type level.
//
// # The rule
//
// "The mesh Link implementation rejects file payloads. This is a type-level
// constraint, not a config option: FILE_DATA is not in the set of record types
// the mesh link will serialize. There is no sysop flag to turn it on and no
// size threshold below which it's allowed."
//
// # Why it lives here rather than in the link
//
// A link moves opaque datagrams; by the time bytes reach one, nothing knows
// they were a record. The place a file's content could actually get onto the
// air is a FILE record with a body far larger than a catalog entry — the
// generic path allows a body up to MaxBodyLen, which is 8 KiB, and 8 KiB is a
// small file.
//
// So the constraint is enforced where a FILE record is BUILT and where one is
// DECODED: a FILE body must parse as a catalog entry, and a catalog entry's
// fields are each bounded. A FILE record therefore cannot be large enough to
// carry content, on any link, by construction rather than by inspection. That
// also makes it link-independent, which is right — a catalog entry stuffed with
// file bytes is wrong over IP too, and §7.5's fetch path 1 is a direct transfer
// between BBSes, not a record.
//
// This is what closes the "it will be tempting to violate" door §7.5 names: the
// tempting change is a size threshold, and there is nowhere to put one.
func checkNoFileContent(r *Record) error {
	if r.Type != TypeFile {
		return nil
	}
	if _, err := UnmarshalFileBody(r.Body); err != nil {
		return fmt.Errorf("FILE record body is not a catalog entry (§7.5 forbids "+
			"file content in a record, at any size): %w", err)
	}
	return nil
}
