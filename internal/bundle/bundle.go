// Package bundle implements L2: framing and compression of record batches
// (design §7.4, §6.1.3).
//
// # Why bundles exist at all
//
// At the §1.1 budget a node originates roughly ten full packets a day. Sending
// one packet per post would spend the fixed header on every post and burn two
// seconds of airtime on a forty-character message. Batching amortises the
// framing across records and — more importantly — lets the compressor find
// redundancy BETWEEN records, which is where most of the win is.
//
// # What hoists out of records
//
// A bundle is per-area and usually single-origin, so the 4-byte area tag, the
// 8-byte origin ID and a base timestamp are paid once per bundle rather than
// once per record (§6.1.3). Records carry a 1-byte index into an origin table
// instead of a full ID, which is what makes 8-byte self-certifying node IDs
// cheaper on the wire than the 4-byte numeric addresses they replaced, at any
// bundle size of three or more.
package bundle

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/klauspost/compress/zstd"
)

// FormatVersion is the bundle framing version. Like the record format it is
// frozen at the Phase 6 wire freeze (§7.1).
const FormatVersion uint8 = 1

// Limits. These bound what a hostile or buggy peer can make us allocate.
const (
	// MaxRecords per bundle. Well above any realistic batch; the point is to
	// have a bound at all.
	MaxRecords = 256
	// MaxOrigins in the per-bundle origin table.
	MaxOrigins = 64
	// MaxDecompressed caps zstd output.
	//
	// This is the decompression-bomb defence from §7.4: a 233-byte packet can
	// expand to gigabytes, and §8.1 says to assume everyone on the channel is
	// hostile. The limit is enforced in the streaming decoder, not checked
	// afterwards, so the memory is never allocated.
	MaxDecompressed = 1 << 20 // 1 MiB
)

var (
	ErrTruncated  = errors.New("truncated bundle")
	ErrTooMany    = errors.New("bundle exceeds its declared limits")
	ErrBomb       = errors.New("bundle decompresses beyond the allowed size")
	ErrEmptyBatch = errors.New("bundle contains no records")
	// ErrNotCanonical is returned when input is a non-canonical encoding of what
	// it parses to: one logical bundle must have exactly one wire form.
	ErrNotCanonical = errors.New("non-canonical bundle encoding")
)

// Bundle is a batch of records sharing an area.
type Bundle struct {
	// Area is the area tag every record in the bundle belongs to. Hoisted out
	// of the records themselves.
	Area record.AreaTag
	// BaseTS is the reference timestamp; records carry small deltas from it.
	BaseTS uint32
	// DictID selects the compression dictionary. Nodes announce which they
	// hold in their digests (§7.4).
	DictID uint8
	// Records in the bundle.
	Records []*record.Record
}

// Pack serialises and compresses a bundle.
//
// The compression covers the whole batch rather than each record, so zstd can
// exploit cross-record redundancy — quoting conventions, signature blocks,
// repeated nicks. That is the difference between roughly 1.3x on a lone post
// and 3-5x with a trained dictionary (§7.4).
func Pack(b *Bundle, dict *Dictionary) ([]byte, error) {
	if len(b.Records) == 0 {
		return nil, ErrEmptyBatch
	}
	if len(b.Records) > MaxRecords {
		return nil, fmt.Errorf("%w: %d records, limit %d", ErrTooMany, len(b.Records), MaxRecords)
	}

	payload, err := encodeBody(b)
	if err != nil {
		return nil, err
	}

	compressed, err := dict.Compress(payload)
	if err != nil {
		return nil, err
	}

	// Header travels UNCOMPRESSED so a receiver can check the format version
	// and dictionary before spending anything on decompression.
	out := make([]byte, 0, 3+len(compressed))
	out = append(out, FormatVersion, dict.ID())
	out = binary.AppendUvarint(out, uint64(len(payload)))
	out = append(out, compressed...)
	return out, nil
}

// Unpack decompresses and parses a bundle.
func Unpack(data []byte, dicts *DictionarySet) (*Bundle, error) {
	if len(data) < 3 {
		return nil, ErrTruncated
	}
	if data[0] != FormatVersion {
		return nil, fmt.Errorf("unsupported bundle format version %d (this build speaks %d)",
			data[0], FormatVersion)
	}
	dictID := data[1]

	declared, n := binary.Uvarint(data[2:])
	if n <= 0 {
		return nil, ErrTruncated
	}
	// Reject the declared size before decompressing anything.
	if declared > MaxDecompressed {
		return nil, fmt.Errorf("%w: declares %d bytes, limit %d", ErrBomb, declared, MaxDecompressed)
	}

	dict, err := dicts.Get(dictID)
	if err != nil {
		return nil, err
	}

	payload, err := dict.Decompress(data[2+n:], int(declared))
	if err != nil {
		return nil, err
	}
	return decodeBody(payload)
}

// EncodeBody and DecodeBody expose the uncompressed framing to the conformance
// corpus (§12.6).
//
// The corpus freezes THIS, not Pack's output. Pack runs the body through zstd,
// and those bytes move whenever the compressor is upgraded or the dictionary is
// retrained — §7.4 asks for exactly that retraining before the Phase 6 freeze.
// Pinning them would freeze a library's behaviour and call it a wire format. The
// framing below is the part that is actually the format, and it is deterministic
// by construction.
func EncodeBody(b *Bundle) ([]byte, error) { return encodeBody(b) }

// DecodeBody parses the uncompressed framing. See EncodeBody.
func DecodeBody(b []byte) (*Bundle, error) { return decodeBody(b) }

// encodeBody renders the uncompressed bundle body.
//
// Layout:
//
//	area(4) | baseTS(4 BE) | originCount(1) | origin[8]... |
//	recordCount(uvarint) | record*
//
// where each record is:
//
//	originIdx(1) | tsDelta(varint) | recLen(uvarint) | recordBytes
//
// The record bytes are the record's own canonical encoding plus signature,
// unchanged. That matters: §6.2.1 requires verification to use the exact bytes
// that were signed, so a bundle must carry them verbatim rather than
// re-deriving them from parsed fields.
func encodeBody(b *Bundle) ([]byte, error) {
	// Build the origin table in first-appearance order, which is deterministic
	// given the record order and avoids a map iteration (§6.2.1 rule 2).
	var origins []identity.NodeID
	index := make(map[identity.NodeID]int, 4)
	for _, r := range b.Records {
		if _, ok := index[r.Origin]; !ok {
			index[r.Origin] = len(origins)
			origins = append(origins, r.Origin)
		}
	}
	if len(origins) > MaxOrigins {
		return nil, fmt.Errorf("%w: %d origins, limit %d", ErrTooMany, len(origins), MaxOrigins)
	}

	buf := make([]byte, 0, 16+len(origins)*identity.NodeIDLen+len(b.Records)*128)
	buf = append(buf, b.Area[:]...)
	buf = binary.BigEndian.AppendUint32(buf, b.BaseTS)
	buf = append(buf, byte(len(origins)))
	for _, o := range origins {
		buf = append(buf, o[:]...)
	}
	buf = binary.AppendUvarint(buf, uint64(len(b.Records)))

	for _, r := range b.Records {
		raw := r.Marshal()
		buf = append(buf, byte(index[r.Origin]))
		// Signed delta: a record may predate the bundle's base timestamp when
		// a batch mixes freshly authored and backfilled records.
		buf = binary.AppendVarint(buf, int64(r.TS)-int64(b.BaseTS))
		buf = binary.AppendUvarint(buf, uint64(len(raw)))
		buf = append(buf, raw...)
	}
	return buf, nil
}

// canonicalUvarint reads a uvarint and rejects overlong encodings.
//
// binary.Uvarint accepts padding: 0x81 0x00 decodes to 1 exactly as 0x01 does.
// That gives one logical bundle several wire forms, which matters directly here
// because §7.2 derives the bundle ID from these bytes — a second wire form is a
// second bundle ID for identical content, and the resumable transmission that
// depends on stable IDs stops resuming without any visible error.
//
// The same defence exists in vv and gossip. It was missing here because the
// bundle body parser was effectively unfuzzed: reaching it through Unpack
// requires the fuzzer to synthesize a valid zstd frame first, so essentially
// every input died at decompression. See FuzzDecodeBody.
func canonicalUvarint(b []byte) (uint64, int, error) {
	val, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, ErrTruncated
	}
	if want := binary.AppendUvarint(nil, val); len(want) != n {
		return 0, 0, fmt.Errorf("%w: %d bytes encode a value needing %d", ErrNotCanonical, n, len(want))
	}
	return val, n, nil
}

// canonicalVarint is the signed counterpart, for timestamp deltas.
func canonicalVarint(b []byte) (int64, int, error) {
	val, n := binary.Varint(b)
	if n <= 0 {
		return 0, 0, ErrTruncated
	}
	if want := binary.AppendVarint(nil, val); len(want) != n {
		return 0, 0, fmt.Errorf("%w: %d bytes encode a value needing %d", ErrNotCanonical, n, len(want))
	}
	return val, n, nil
}

func decodeBody(b []byte) (*Bundle, error) {
	out := &Bundle{}
	p := 0

	need := func(n int) error {
		if p+n > len(b) {
			return ErrTruncated
		}
		return nil
	}

	if err := need(4 + 4 + 1); err != nil {
		return nil, err
	}
	copy(out.Area[:], b[p:p+4])
	p += 4
	out.BaseTS = binary.BigEndian.Uint32(b[p:])
	p += 4

	originCount := int(b[p])
	p++
	if originCount == 0 || originCount > MaxOrigins {
		return nil, fmt.Errorf("%w: %d origins", ErrTooMany, originCount)
	}
	if err := need(originCount * identity.NodeIDLen); err != nil {
		return nil, err
	}
	origins := make([]identity.NodeID, originCount)
	seenOrigin := make(map[identity.NodeID]bool, originCount)
	for i := range origins {
		copy(origins[i][:], b[p:p+identity.NodeIDLen])
		p += identity.NodeIDLen
		// A repeated origin gives records a choice of index for the same node,
		// so the same bundle could be written several ways.
		if seenOrigin[origins[i]] {
			return nil, fmt.Errorf("%w: origin %s appears twice in the table",
				ErrNotCanonical, origins[i])
		}
		seenOrigin[origins[i]] = true
	}

	count, n, err := canonicalUvarint(b[p:])
	if err != nil {
		return nil, err
	}
	p += n
	if count == 0 {
		return nil, ErrEmptyBatch
	}
	if count > MaxRecords {
		return nil, fmt.Errorf("%w: %d records", ErrTooMany, count)
	}

	out.Records = make([]*record.Record, 0, count)
	// The table must be exactly what encodeBody would build: every entry
	// referenced, in first-appearance order. An unused entry, or a table in any
	// other order, is a second wire form for the same set of records.
	nextExpected := 0
	for i := uint64(0); i < count; i++ {
		if err := need(1); err != nil {
			return nil, err
		}
		idx := int(b[p])
		p++
		if idx >= originCount {
			return nil, fmt.Errorf("record %d references origin %d, table has %d", i, idx, originCount)
		}
		if idx > nextExpected {
			return nil, fmt.Errorf("%w: record %d references origin %d before %d is used; "+
				"the table is not in first-appearance order", ErrNotCanonical, i, idx, nextExpected)
		}
		if idx == nextExpected {
			nextExpected++
		}

		delta, n, err := canonicalVarint(b[p:])
		if err != nil {
			return nil, err
		}
		p += n

		recLen, n, err := canonicalUvarint(b[p:])
		if err != nil {
			return nil, err
		}
		p += n
		if recLen > uint64(len(b)-p) {
			return nil, ErrTruncated
		}

		r, err := record.Unmarshal(b[p : p+int(recLen)])
		if err != nil {
			return nil, fmt.Errorf("record %d: %w", i, err)
		}
		p += int(recLen)

		// The hoisted fields must agree with what the record itself carries.
		// They are redundant on purpose — the record is the authority, since
		// its bytes are what the signature covers — so a mismatch means the
		// bundle framing is lying and the whole batch is suspect.
		if r.Origin != origins[idx] {
			return nil, fmt.Errorf("record %d: origin table says %s, record says %s",
				i, origins[idx], r.Origin)
		}
		if int64(r.TS) != int64(out.BaseTS)+delta {
			return nil, fmt.Errorf("record %d: timestamp delta disagrees with the record", i)
		}
		out.Records = append(out.Records, r)
	}

	if nextExpected != originCount {
		return nil, fmt.Errorf("%w: origin table declares %d entries but only %d are referenced",
			ErrNotCanonical, originCount, nextExpected)
	}
	if p != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after bundle", len(b)-p)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Compression (§7.4)
// ---------------------------------------------------------------------------

// Dictionary0Corpus is the content of dictionary 0.
//
// It lives here rather than in the code that wires up a server because it is a
// wire-format constant, not a configuration choice: two nodes holding different
// bytes under the same dictionary ID cannot read each other's bundles at all.
// The conformance corpus (§12.6) pins a bundle packed against it, which is what
// makes editing this string in place a red build.
//
// That matters more than it looks, because this corpus is KNOWN to be wrong and
// scheduled to be replaced. §7.4 records that it is a raw corpus of forum
// vocabulary written when a post was the only thing being compressed — it
// predates both FILE and DOOR_EVENT — and that a real `zstd --train` product
// must ship before the Phase 6 freeze. When that lands it must ship as
// dictionary 1, leaving this one untouched and supported, exactly as §7.4 says
// old dictionaries stay supported. Rewriting dictionary 0 would silently break
// every peer running an older build.
const Dictionary0Corpus = "subject: re: from wrote posted meshbbs node area post reply thread " +
	"http:// https:// the and that with have this for you are not "

// Dictionary0 builds the dictionary every node speaks by default.
func Dictionary0() (*Dictionary, error) {
	return NewRawDictionary(0, []byte(Dictionary0Corpus))
}

// Dictionary is a zstd dictionary plus its identifier.
//
// Dictionary compression is the highest-leverage optimisation available: a
// lone 400-byte post compresses about 1.3x with generic zstd because there is
// not enough data to build a model, but 3-5x against a dictionary that already
// holds common English fragments, quoting conventions and BBS vocabulary. At
// roughly ten packets a day that is the difference between eight posts and
// twenty-five.
type Dictionary struct {
	id      uint8
	raw     []byte
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// NewDictionary builds a dictionary from the format `zstd --train` produces.
//
// That format carries entropy tables as well as content, which is why a
// trained dictionary beats a raw corpus — and it is what §7.4 means by
// shipping a versioned dictionary in the binary. A nil or empty argument means
// plain zstd, which is the correct behaviour for dictionary 0.
//
// Use NewRawDictionary for a plain corpus with no dictionary framing. The two
// are kept separate deliberately: silently accepting either would make a
// mis-shipped file look like it worked while losing most of its benefit.
func NewDictionary(id uint8, trained []byte) (*Dictionary, error) {
	var encOpts []zstd.EOption
	var decOpts []zstd.DOption
	if len(trained) > 0 {
		encOpts = append(encOpts, zstd.WithEncoderDict(trained))
		decOpts = append(decOpts, zstd.WithDecoderDicts(trained))
	}
	return newDictionary(id, trained, encOpts, decOpts)
}

// NewRawDictionary builds a dictionary from raw content — a corpus with no
// zstd dictionary framing.
//
// Both ends must agree on the id, since a raw dictionary carries none of its
// own. Useful before a trained dictionary has been produced, and for tests.
func NewRawDictionary(id uint8, content []byte) (*Dictionary, error) {
	if len(content) == 0 {
		return NewDictionary(id, nil)
	}
	// The zstd dictionary id is a uint32 on the wire; ours is the single byte
	// carried in the bundle header, so widen it consistently at both ends.
	encOpts := []zstd.EOption{zstd.WithEncoderDictRaw(uint32(id), content)}
	decOpts := []zstd.DOption{zstd.WithDecoderDictRaw(uint32(id), content)}
	return newDictionary(id, content, encOpts, decOpts)
}

func newDictionary(id uint8, raw []byte, encOpts []zstd.EOption, decOpts []zstd.DOption) (*Dictionary, error) {
	// SpeedBestCompression: this runs perhaps ten times a day on a Raspberry
	// Pi, and every byte saved is airtime the whole network shares.
	encOpts = append(encOpts, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	// Bound the decoder's memory as well as its output.
	decOpts = append(decOpts, zstd.WithDecoderMaxMemory(MaxDecompressed))

	enc, err := zstd.NewWriter(nil, encOpts...)
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, decOpts...)
	if err != nil {
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	return &Dictionary{id: id, raw: raw, encoder: enc, decoder: dec}, nil
}

// ID returns the dictionary identifier carried in the bundle header.
func (d *Dictionary) ID() uint8 { return d.id }

// Compress encodes a payload.
func (d *Dictionary) Compress(payload []byte) ([]byte, error) {
	return d.encoder.EncodeAll(payload, nil), nil
}

// Decompress decodes a payload, refusing to exceed the declared size.
//
// The declared size is checked against MaxDecompressed by the caller before we
// get here; passing it in lets DecodeAll allocate exactly once and, more
// importantly, means a stream claiming a small size but expanding hugely is
// caught by the decoder rather than after the fact.
func (d *Dictionary) Decompress(data []byte, declared int) ([]byte, error) {
	if declared < 0 || declared > MaxDecompressed {
		return nil, ErrBomb
	}
	out, err := d.decoder.DecodeAll(data, make([]byte, 0, declared))
	if err != nil {
		return nil, fmt.Errorf("decompress bundle: %w", err)
	}
	if len(out) != declared {
		return nil, fmt.Errorf("%w: declared %d bytes, produced %d", ErrBomb, declared, len(out))
	}
	return out, nil
}

// Close releases the encoder and decoder.
func (d *Dictionary) Close() {
	if d.encoder != nil {
		d.encoder.Close()
	}
	if d.decoder != nil {
		d.decoder.Close()
	}
}

// DictionarySet holds the dictionaries a node can decode with.
//
// Old dictionaries stay supported when new ones ship (§7.4), so this is a set
// rather than a single value: a peer that has not upgraded still gets read.
type DictionarySet struct {
	byID map[uint8]*Dictionary
}

// NewDictionarySet builds a set containing at least dictionary 0 (plain zstd).
func NewDictionarySet(dicts ...*Dictionary) (*DictionarySet, error) {
	s := &DictionarySet{byID: map[uint8]*Dictionary{}}
	plain, err := NewDictionary(0, nil)
	if err != nil {
		return nil, err
	}
	s.byID[0] = plain
	for _, d := range dicts {
		s.byID[d.ID()] = d
	}
	return s, nil
}

// Get returns a dictionary by ID.
func (s *DictionarySet) Get(id uint8) (*Dictionary, error) {
	d, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("unknown compression dictionary %d; "+
			"the sender is using one this node does not hold", id)
	}
	return d, nil
}

// IDs returns the dictionary IDs held, for announcing in a digest.
func (s *DictionarySet) IDs() []uint8 {
	out := make([]uint8, 0, len(s.byID))
	for id := range s.byID {
		out = append(out, id)
	}
	// Sorted: this goes on the wire and into a digest.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Close releases every dictionary.
func (s *DictionarySet) Close() {
	for _, d := range s.byID {
		d.Close()
	}
}
