// Package gossip implements anti-entropy replication (design §7.3).
//
// # The problem this package exists to solve
//
// v0.1 of the design proposed broadcasting a digest every 15–30 minutes. At the
// 50-instance scale of [D2] that is fatal: 50 nodes each sending a 100-byte
// digest every 30 minutes, charged at the flood multiplier, consumes about
// **11% of the channel** — more than the entire 5% airtime budget, before a
// single post is carried. §7.3 calls this the digest storm.
//
// Four mitigations, all required, all implemented here:
//
//  1. Digests never carry full version vectors. A digest carries 10 bytes per
//     area — tag, rolling hash, count — and a hash mismatch triggers a UNICAST
//     exchange of the real vector. See Digest below.
//  2. The interval scales with peer count, clamped by an actual airtime budget
//     rather than a guess. See schedule.go.
//  3. Digests piggyback on any bundle already being sent, so a node with normal
//     traffic almost never sends a standalone one.
//  4. A digest that would carry no information is suppressed.
//
// Anti-entropy is a safety net, not the delivery path. Content propagates by
// opportunistic push; this is what catches what push missed, and what heals a
// node that was offline for a week. There is no session and no handshake, which
// is precisely why it survives links that a FidoNet-style polling session
// cannot (§7.3).
package gossip

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

// FormatVersion is the gossip wire version. Bumping it is a protocol break.
//
// Version 2 added the dictionary byte to the digest header (§7.4). Bumped rather
// than quietly extended, and it could not have been anything else: DecodeDigest
// checks an exact length, so a v1 parser fed a v2 digest fails on the length
// rather than on the version and reports nothing a sysop can act on. §7.1's
// pre-freeze policy makes the break itself cheap — development releases promise
// each other nothing — but "gossip version 1, this build speaks 2" is the part
// worth paying a byte to say.
const FormatVersion uint8 = 2

// MsgType discriminates gossip messages. Values are wire format and must never
// be renumbered.
type MsgType uint8

const (
	// MsgDigest is broadcast: per-area rolling hash and count.
	MsgDigest MsgType = 1
	// MsgVectorReq is unicast: "send me your full vector for these areas".
	MsgVectorReq MsgType = 2
	// MsgVector is unicast: a full version vector for one area.
	MsgVector MsgType = 3
	// MsgRangeReq is unicast: the specific record ranges the sender lacks.
	MsgRangeReq MsgType = 4
)

var msgNames = map[MsgType]string{
	MsgDigest: "DIGEST", MsgVectorReq: "VECTOR_REQ",
	MsgVector: "VECTOR", MsgRangeReq: "RANGE_REQ",
}

func (m MsgType) String() string {
	if n, ok := msgNames[m]; ok {
		return n
	}
	return fmt.Sprintf("MsgType(%d)", uint8(m))
}

// Valid reports whether m is a known message type. Unknown types are rejected
// rather than ignored-and-relayed: anything reachable by a holder of the
// channel PSK is attack surface (§12.5).
func (m MsgType) Valid() bool { _, ok := msgNames[m]; return ok }

// MeshMTU is the Meshtastic usable payload, and the budget every control
// message is sized against.
const MeshMTU = 233

// TransportOverhead is the headroom left for whatever frames a gossip message
// onto a link — a type byte, an area tag, a fragment header. Reserving it here
// means the limits below stay true regardless of which transport carries them.
const TransportOverhead = 9

// MaxControlMessage is the largest gossip control message that still fits one
// mesh packet.
const MaxControlMessage = MeshMTU - TransportOverhead

// digestHeader is type, version, dictionary and area count.
//
// The dictionary byte cost nothing in area capacity, which is worth recording
// because it was the question asked before spending it: MaxAreas is a floor
// division, and (224-3)/10 and (224-4)/10 are both 22.
const digestHeader = 4

// MaxAreas bounds the areas in one digest, derived so that a full digest is
// always one packet.
//
// One packet is not a nicety. A control message spanning several packets has to
// be fragmented, which means it can arrive partially and need its own repair —
// a request that itself requires reliable delivery, which is the recursion the
// whole no-session design exists to avoid. So the limits are derived from the
// MTU rather than chosen, and TestControlMessagesFitOnePacket enforces it.
//
// Full version vectors are the deliberate exception: at fifty instances one is
// about 500 bytes, and §7.3 accepts that it spans packets. That is precisely
// why they are exchanged on demand instead of broadcast every cycle.
const MaxAreas = (MaxControlMessage - digestHeader) / 10

var (
	// ErrTruncated is returned when input ends mid-field.
	ErrTruncated = errors.New("truncated gossip message")
	// ErrTooMany is returned when a message exceeds its declared limits.
	ErrTooMany = errors.New("gossip message exceeds its limits")
	// ErrNotCanonical is returned when input is a non-canonical encoding of
	// what it parses to. See the note on Decode.
	ErrNotCanonical = errors.New("non-canonical gossip encoding")
	// ErrUnsupportedVersion is returned for a message from a build speaking a
	// different gossip version.
	//
	// Distinguishable on purpose. §7.1's pre-freeze policy is drop-and-log, and
	// the two halves of that need separating: "a peer is on another release" is a
	// thing a sysop fixes by upgrading somebody, while a truncation or a
	// canonicality failure is a bug or an attack. Before this existed they were
	// the same unstructured error, and §12.6's cross-version vectors need to
	// assert the difference rather than match on a string.
	ErrUnsupportedVersion = errors.New("unsupported gossip version")
)

// AreaState is one area's entry in a digest.
//
// Ten bytes: the whole point of the digest design. A full version vector for
// one area at 50 instances is about 500 bytes — three mesh packets — and
// broadcasting that every cycle is the storm §7.3 rejects.
type AreaState struct {
	// Tag identifies the area.
	Tag record.AreaTag
	// Hash fingerprints the version vector. A mismatch proves divergence; a
	// match does not prove convergence, but a 32-bit collision on state that
	// two honest nodes actually hold is rare enough that the next cycle
	// catches it.
	Hash [4]byte
	// Count is the total records the sender holds for this area, SATURATING at
	// 65535.
	//
	// Its only job is to answer "who is ahead?" so the node that is behind
	// pulls rather than both nodes pushing. Once an area exceeds 65535 records
	// both sides saturate and that hint degrades to a coin flip — at which
	// point the full-vector exchange resolves it anyway, at the cost of one
	// extra round trip in a mature area. That is the price of keeping the
	// entry at ten bytes, and ten bytes is what makes ten areas fit in one
	// packet.
	Count uint16
}

// SaturatingCount converts a record total to the digest's 16-bit field.
func SaturatingCount(n uint64) uint16 {
	if n > 0xFFFF {
		return 0xFFFF
	}
	return uint16(n)
}

// AreaStateFrom builds a digest entry from a version vector.
func AreaStateFrom(tag record.AreaTag, v *vv.Vector) AreaState {
	return AreaState{Tag: tag, Hash: v.Hash(), Count: SaturatingCount(v.Count())}
}

// Digest is the broadcast heartbeat: what this node holds, per area, in ten
// bytes each.
type Digest struct {
	// Dictionary is the highest compression dictionary ID this node holds
	// (§7.4).
	//
	// One byte, and "highest held" rather than a bitmap of what is held, because
	// §7.4 promises old dictionaries stay supported and bundle.DefaultDictionarySet
	// holds 0..N: a gap is not a state this design permits, so a bitmap would
	// spend seven bits describing something unrepresentable.
	//
	// It rides on the digest because the digest is the thing every peer already
	// hears on a schedule, which is what §7.4 meant by "announce in their
	// digest". The cost is small enough to state exactly: at 8600µs/byte and a
	// flood multiplier of 4, one byte is 34.4ms of channel per digest, and a
	// fifty-node mesh on a five-hour interval spends about 8.3 seconds of
	// channel a day on it — roughly 0.2% of §1.1's federation allocation.
	Dictionary uint8

	Areas []AreaState
}

// NewDigest builds a digest over a set of area vectors.
//
// The dictionary is left zero and set by the caller, because this package has no
// business importing the compression code: gossip is anti-entropy, and the ID is
// a number it carries rather than a capability it understands.
func NewDigest(areas map[record.AreaTag]*vv.Vector) *Digest {
	d := &Digest{Areas: make([]AreaState, 0, len(areas))}
	for tag, v := range areas {
		d.Areas = append(d.Areas, AreaStateFrom(tag, v))
	}
	// Sorted so the encoding is canonical. Go randomises map iteration, and an
	// unsorted walk here would give one logical digest many wire forms — the
	// §6.2.1 rule-2 failure that has now bitten this codebase three times.
	sort.Slice(d.Areas, func(i, j int) bool { return lessTag(d.Areas[i].Tag, d.Areas[j].Tag) })
	return d
}

func lessTag(a, b record.AreaTag) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Get returns the state for an area and whether it was present.
func (d *Digest) Get(tag record.AreaTag) (AreaState, bool) {
	for _, a := range d.Areas {
		if a.Tag == tag {
			return a, true
		}
	}
	return AreaState{}, false
}

// Size is the encoded byte length, for airtime budgeting before sending.
func (d *Digest) Size() int { return digestHeader + 10*len(d.Areas) }

// Encode serialises the digest.
//
//	type(1) | version(1) | dict(1) | area_count(1) | (tag[4] | hash[4] | count[2])*
//
// Areas ascend by tag, which is what makes the encoding canonical. Encode sorts
// rather than trusting the caller, so the canonical form is a property of this
// function alone — the same guarantee VectorReq.Encode and RangeReq.Encode give.
//
// That is not defensive tidiness, it is what makes the round-trip fuzz target
// work at all. The target asserts decode-then-encode reproduces the input; if
// Encode preserved whatever order the decoder handed it, a digest with
// descending areas would re-encode to identical bytes and the check would pass.
// Verified: with the decoder's ordering check deliberately removed and Encode
// not sorting, five million fuzz executions found nothing.
func (d *Digest) Encode() []byte {
	sort.Slice(d.Areas, func(i, j int) bool { return lessTag(d.Areas[i].Tag, d.Areas[j].Tag) })
	buf := make([]byte, 0, d.Size())
	buf = append(buf, byte(MsgDigest), FormatVersion, d.Dictionary, byte(len(d.Areas)))
	for _, a := range d.Areas {
		buf = append(buf, a.Tag[:]...)
		buf = append(buf, a.Hash[:]...)
		buf = binary.BigEndian.AppendUint16(buf, a.Count)
	}
	return buf
}

// DecodeDigest parses the Encode form.
//
// It rejects any input that is not the canonical encoding of what it parses to:
// areas must ascend and must not repeat. Two byte strings decoding to one
// digest would let a peer present identical state under a different
// fingerprint, which anti-entropy reads as permanent divergence — a livelock
// that burns airtime forever and looks like a flaky radio. This exact bug class
// has been found three times by fuzzing (overlong varints in the record codec
// and the version vector, unvalidated reserved bits in the fountain symbol
// header), so it is checked here by construction rather than waited for.
func DecodeDigest(b []byte) (*Digest, error) {
	// Type and version are read before any structural check, and the order is
	// load-bearing rather than stylistic.
	//
	// A version-1 digest is three bytes where this version's header is four, so
	// checking the length first reports a TRUNCATION for what is really an older
	// peer — sending a sysop to look at the radios when the answer is that
	// somebody needs upgrading. Every length rule below belongs to version 2 and
	// may not be applied to bytes that have not yet claimed to be version 2.
	// Found by the cross-version corpus (§12.6) on its first run.
	if len(b) < 2 {
		return nil, ErrTruncated
	}
	if MsgType(b[0]) != MsgDigest {
		return nil, fmt.Errorf("not a digest: message type %d", b[0])
	}
	if b[1] != FormatVersion {
		return nil, fmt.Errorf("%w: digest is gossip version %d, this build speaks %d",
			ErrUnsupportedVersion, b[1], FormatVersion)
	}
	if len(b) < digestHeader {
		return nil, ErrTruncated
	}
	n := int(b[3])
	if n > MaxAreas {
		return nil, fmt.Errorf("%w: digest declares %d areas, limit is %d", ErrTooMany, n, MaxAreas)
	}
	if len(b) != digestHeader+10*n {
		return nil, ErrTruncated
	}

	d := &Digest{Dictionary: b[2], Areas: make([]AreaState, 0, n)}
	p := digestHeader
	var prev record.AreaTag
	for i := 0; i < n; i++ {
		var a AreaState
		copy(a.Tag[:], b[p:p+4])
		copy(a.Hash[:], b[p+4:p+8])
		a.Count = binary.BigEndian.Uint16(b[p+8 : p+10])
		p += 10

		if i > 0 && !lessTag(prev, a.Tag) {
			return nil, fmt.Errorf("%w: digest areas are not in ascending order", ErrNotCanonical)
		}
		prev = a.Tag
		d.Areas = append(d.Areas, a)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Unicast messages
// ---------------------------------------------------------------------------

// VectorReq asks a peer for its full version vectors for specific areas.
//
// Unicast and on demand. This is the message that keeps full vectors off the
// broadcast channel, and it is only ever sent after a rolling-hash mismatch has
// proved there is something to learn.
type VectorReq struct {
	Areas []record.AreaTag
}

func (r *VectorReq) Encode() []byte {
	tags := append([]record.AreaTag(nil), r.Areas...)
	sort.Slice(tags, func(i, j int) bool { return lessTag(tags[i], tags[j]) })
	buf := make([]byte, 0, 3+4*len(tags))
	buf = append(buf, byte(MsgVectorReq), FormatVersion, byte(len(tags)))
	for _, t := range tags {
		buf = append(buf, t[:]...)
	}
	return buf
}

func DecodeVectorReq(b []byte) (*VectorReq, error) {
	// Version before structure. See DecodeDigest: a length rule belongs to the
	// version that defined it, so applying one to bytes that have not yet
	// claimed that version turns "an older peer" into "malformed input".
	if len(b) < 2 {
		return nil, ErrTruncated
	}
	if MsgType(b[0]) != MsgVectorReq {
		return nil, fmt.Errorf("not a vector request: message type %d", b[0])
	}
	if b[1] != FormatVersion {
		return nil, fmt.Errorf("%w: message is gossip version %d, this build speaks %d",
			ErrUnsupportedVersion, b[1], FormatVersion)
	}
	if len(b) < 3 {
		return nil, ErrTruncated
	}
	n := int(b[2])
	if n > MaxAreas {
		return nil, fmt.Errorf("%w: %d areas requested, limit is %d", ErrTooMany, n, MaxAreas)
	}
	if len(b) != 3+4*n {
		return nil, ErrTruncated
	}
	r := &VectorReq{Areas: make([]record.AreaTag, 0, n)}
	var prev record.AreaTag
	for i := 0; i < n; i++ {
		var t record.AreaTag
		copy(t[:], b[3+4*i:7+4*i])
		if i > 0 && !lessTag(prev, t) {
			return nil, fmt.Errorf("%w: requested areas are not in ascending order", ErrNotCanonical)
		}
		prev = t
		r.Areas = append(r.Areas, t)
	}
	return r, nil
}

// VectorMsg carries one area's full version vector, unicast.
type VectorMsg struct {
	Area   record.AreaTag
	Vector *vv.Vector
}

func (m *VectorMsg) Encode() []byte {
	body := m.Vector.Encode()
	buf := make([]byte, 0, 6+len(body))
	buf = append(buf, byte(MsgVector), FormatVersion)
	buf = append(buf, m.Area[:]...)
	return append(buf, body...)
}

func DecodeVectorMsg(b []byte) (*VectorMsg, error) {
	// Version before structure. See DecodeDigest: a length rule belongs to the
	// version that defined it, so applying one to bytes that have not yet
	// claimed that version turns "an older peer" into "malformed input".
	if len(b) < 2 {
		return nil, ErrTruncated
	}
	if MsgType(b[0]) != MsgVector {
		return nil, fmt.Errorf("not a vector message: message type %d", b[0])
	}
	if b[1] != FormatVersion {
		return nil, fmt.Errorf("%w: message is gossip version %d, this build speaks %d",
			ErrUnsupportedVersion, b[1], FormatVersion)
	}
	if len(b) < 6 {
		return nil, ErrTruncated
	}
	m := &VectorMsg{}
	copy(m.Area[:], b[2:6])
	v, err := vv.Decode(b[6:])
	if err != nil {
		return nil, err
	}
	m.Vector = v
	return m, nil
}

// MaxRanges is a DECODE-side allocation bound: the most ranges we will parse
// from one message, so a hostile peer cannot force a large allocation.
//
// It is deliberately not the sender's limit. §7.3 budgets "~10 bytes per
// range", which is true only while sequence numbers are small — the origin is
// 8 fixed bytes but the two varints grow, so at a million records per origin a
// range costs 14 bytes and at 2^35 it costs 18. Deriving a range count from an
// assumed width therefore produces a message that fits one packet early in an
// area's life and silently outgrows it later, which is a fragmentation bug that
// only appears on mature deployments.
//
// So senders bound the request in BYTES, by measuring: see FitRanges.
const MaxRanges = 32

// FitRanges returns the longest prefix of ranges whose encoded request fits
// within maxBytes.
//
// Measured rather than estimated, because the encoded width of a range depends
// on the magnitude of the sequence numbers in it. Ranges are taken in order, so
// callers should sort lowest-sequence-first: filling gaps from the bottom is
// what advances a contiguous high-water mark, and taking the newest records
// first would leave the vector stuck exactly where it was.
func FitRanges(area record.AreaTag, ranges []vv.Range, maxBytes int) []vv.Range {
	if len(ranges) > MaxRanges {
		ranges = ranges[:MaxRanges]
	}
	for n := len(ranges); n > 0; n-- {
		req := &RangeReq{Area: area, Ranges: ranges[:n]}
		if len(req.Encode()) <= maxBytes {
			return ranges[:n]
		}
	}
	return nil
}

// RangeReq asks for specific record ranges, unicast.
//
// This is the delta request of §7.3: the requester has compared full vectors
// and knows exactly what it lacks, so it asks for exactly that rather than
// triggering a full resend.
type RangeReq struct {
	Area   record.AreaTag
	Ranges []vv.Range
}

func (r *RangeReq) Encode() []byte {
	ranges := append([]vv.Range(nil), r.Ranges...)
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Origin != ranges[j].Origin {
			return lessNode(ranges[i].Origin, ranges[j].Origin)
		}
		return ranges[i].From < ranges[j].From
	})

	buf := make([]byte, 0, 7+len(ranges)*18)
	buf = append(buf, byte(MsgRangeReq), FormatVersion)
	buf = append(buf, r.Area[:]...)
	buf = append(buf, byte(len(ranges)))
	for _, rg := range ranges {
		buf = append(buf, rg.Origin[:]...)
		buf = binary.AppendUvarint(buf, rg.From)
		buf = binary.AppendUvarint(buf, rg.To-rg.From) // span, not absolute
	}
	return buf
}

func DecodeRangeReq(b []byte) (*RangeReq, error) {
	// Version before structure. See DecodeDigest: a length rule belongs to the
	// version that defined it, so applying one to bytes that have not yet
	// claimed that version turns "an older peer" into "malformed input".
	if len(b) < 2 {
		return nil, ErrTruncated
	}
	if MsgType(b[0]) != MsgRangeReq {
		return nil, fmt.Errorf("not a range request: message type %d", b[0])
	}
	if b[1] != FormatVersion {
		return nil, fmt.Errorf("%w: message is gossip version %d, this build speaks %d",
			ErrUnsupportedVersion, b[1], FormatVersion)
	}
	if len(b) < 7 {
		return nil, ErrTruncated
	}
	r := &RangeReq{}
	copy(r.Area[:], b[2:6])
	n := int(b[6])
	if n > MaxRanges {
		return nil, fmt.Errorf("%w: %d ranges requested, limit is %d", ErrTooMany, n, MaxRanges)
	}

	p := 7
	var prev vv.Range
	for i := 0; i < n; i++ {
		if p+identityLen > len(b) {
			return nil, ErrTruncated
		}
		var rg vv.Range
		copy(rg.Origin[:], b[p:p+identityLen])
		p += identityLen

		from, read, err := canonicalUvarint(b[p:])
		if err != nil {
			return nil, err
		}
		p += read
		span, read, err := canonicalUvarint(b[p:])
		if err != nil {
			return nil, err
		}
		p += read

		if from == 0 {
			// Sequences start at 1, so a request from zero is malformed rather
			// than merely useless.
			return nil, errors.New("range request starts at sequence 0")
		}
		rg.From = from
		rg.To = from + span

		if i > 0 {
			if rg.Origin == prev.Origin && rg.From <= prev.From {
				return nil, fmt.Errorf("%w: ranges for one origin are not ascending", ErrNotCanonical)
			}
			if rg.Origin != prev.Origin && !lessNode(prev.Origin, rg.Origin) {
				return nil, fmt.Errorf("%w: range origins are not in ascending order", ErrNotCanonical)
			}
		}
		prev = rg
		r.Ranges = append(r.Ranges, rg)
	}
	if p != len(b) {
		return nil, fmt.Errorf("%d trailing bytes after range request", len(b)-p)
	}
	return r, nil
}
