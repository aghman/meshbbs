// Package conformance holds the frozen BSMP wire-format vectors of design §12.6
// and the code that derives them from their own inputs.
//
// # Why this exists
//
// Every other test that touches an encoder is a round trip: encode, decode,
// assert you got back what you put in. That proves the two halves agree with
// each other and says nothing about WHICH bytes they agreed on. An encoder that
// started writing record timestamps little-endian would pass all of them, and
// the break would surface as a peer that silently rejects every signature.
//
// `[D10]` commits the project to freezing BSMP publicly in Phase 6 and to asking
// for a Meshtastic portnum after that. Both are promises to strangers, and a
// promise about bytes needs a witness of those bytes that no amount of
// self-consistent refactoring can move. testdata/v1 is that witness.
//
// # The corpus is self-describing, and that is the whole design
//
// Each vector carries its INPUT alongside its expected output. The test rebuilds
// the output from the input through the real encoders and compares against the
// hex frozen in the file — the expectation lives in the file and is never
// recomputed by the thing being tested. That is also what makes the corpus
// usable by an implementation that is not this one, which §12.6 says is the
// actual point of registering a portnum.
//
// The Derive* functions below are shared with the generator in
// tools/conformance. Sharing them is correct: they are the mapping from input to
// bytes, which is precisely what is under test. What must never be shared is the
// expectation, and it is not — it is checked-in JSON.
//
// # Adding vectors
//
// The corpus is append-only, and kept in generations. See testdata/README.md.
package conformance

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/meshlink"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

// A Generation is one directory of frozen vectors and the wire versions in
// force when it was written.
//
// # Why generations rather than one directory per format
//
// BSMP is three independently versioned formats — records, bundles and gossip —
// and they do not move together. A directory per format would be the tidier
// model on paper and would immediately need cross-directory bookkeeping to say
// which combination was ever shipped together, which is the only thing a
// third-party implementation actually needs to know. So a generation is a
// SNAPSHOT of all three, numbered, and a new one is cut whenever any of them
// changes.
//
// # What an old generation is for
//
// It does not become dead weight and it is not deleted. It becomes the
// cross-version corpus §12.6's third bullet asks for, and it tests in both
// directions at once:
//
//   - Layers whose version did NOT change must still encode byte-identically.
//     That is the useful half nobody thinks to check — a format bump elsewhere
//     is exactly when an unrelated encoder gets disturbed by accident.
//   - Layers whose version DID change must now be REJECTED, with a version
//     error rather than a truncation. That is §7.1's pre-freeze drop-and-log
//     stated as an assertion.
type Generation struct {
	// N is the generation number, and the directory it lives in.
	N int
	// Record, Bundle and Gossip are the wire versions frozen in it.
	Record, Bundle, Gossip uint8
}

// Dir is where this generation's vectors live, relative to the package.
func (g Generation) Dir() string { return fmt.Sprintf("testdata/v%d", g.N) }

// Generations are every corpus ever frozen, oldest first.
//
// Append only. Removing one would discard the only evidence of what a shipped
// build put on the air, which is the thing a third party would test against.
var Generations = []Generation{
	{N: 1, Record: 1, Bundle: 1, Gossip: 1},
	// Gossip 2 added the dictionary byte to the digest header (§7.4). Records
	// and bundles did not move, and generation 1's vectors for them must still
	// hold — which is asserted rather than assumed.
	{N: 2, Record: 1, Bundle: 1, Gossip: 2},
}

// Current is the generation this build speaks and the only one it can write.
func Current() Generation { return Generations[len(Generations)-1] }

// Hex renders a byte slice as a lowercase hex string in JSON.
//
// The corpus is meant to be read by a human diffing a pull request and by an
// implementation that is not written in Go, so every byte string in it is hex
// rather than base64: hex survives being quoted in a bug report, lines up in a
// diff, and has exactly one encoding.
type Hex []byte

func (h Hex) MarshalJSON() ([]byte, error) {
	return json.Marshal(hex.EncodeToString(h))
}

func (h *Hex) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("not hex: %w", err)
	}
	*h = raw
	return nil
}

// ---------------------------------------------------------------------------
// Layer 0 — identity
// ---------------------------------------------------------------------------

// KeyVector pins the derivation from a raw Ed25519 seed to everything a node ID
// is rendered as.
//
// The SEED is the frozen artifact, not a call to identity.GenerateNodeKey. Tests
// elsewhere build keys with rng.TestSecret, which is reproducible but depends on
// internal/rng's stream and on how ed25519.GenerateKey reads from it — two
// things a corpus that must outlive this codebase cannot rest on.
type KeyVector struct {
	Name      string `json:"name"`
	Note      string `json:"note,omitempty"`
	Seed      Hex    `json:"seed"`
	PublicKey Hex    `json:"public_key"`
	NodeID    Hex    `json:"node_id"`
	Compact   string `json:"compact"`
	Grouped   string `json:"grouped"`
	Short     string `json:"short"`
	Words     string `json:"words"`
}

// KeySet maps vector names to the keys they describe.
type KeySet map[string]identity.NodeKey

// KeyFromSeed rebuilds a node key from its frozen seed.
func KeyFromSeed(seed []byte) (identity.NodeKey, error) {
	if len(seed) != ed25519.SeedSize {
		return identity.NodeKey{}, fmt.Errorf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return identity.NodeKey{Public: priv.Public().(ed25519.PublicKey), Private: priv}, nil
}

// DeriveKey computes the expected halves of a KeyVector from its seed.
func DeriveKey(name string, seed []byte) (KeyVector, error) {
	key, err := KeyFromSeed(seed)
	if err != nil {
		return KeyVector{}, err
	}
	id := key.ID()
	return KeyVector{
		Name:      name,
		Seed:      seed,
		PublicKey: Hex(key.Public),
		NodeID:    Hex(id[:]),
		Compact:   id.Compact(),
		Grouped:   id.String(),
		Short:     id.Short(),
		Words:     id.Words(),
	}, nil
}

// ---------------------------------------------------------------------------
// Layer 1 — records
// ---------------------------------------------------------------------------

// RecordInput is everything needed to rebuild one record.
//
// Area is the area NAME rather than its tag, so a vector pins AreaTagFor as well
// as the envelope; the derived tag is recorded alongside so a failure says which
// of the two moved. An EMPTY name means the zero tag rather than the tag of the
// empty string — that is what NewNodeRecord and NewSuccessionRecord write, since
// those records belong to no area, and a vector that derived AreaTagFor("")
// instead would freeze four bytes no record has ever carried.
//
// Body is raw hex rather than a typed body description. The typed encoders have
// their own vectors in bodies.json, and keeping the two apart means an envelope
// failure and a body failure cannot be mistaken for each other.
type RecordInput struct {
	Key    string `json:"key"`
	Seq    uint64 `json:"seq"`
	TS     uint32 `json:"ts"`
	Type   uint8  `json:"type"`
	Area   string `json:"area"`
	Parent Hex    `json:"parent,omitempty"`
	Body   Hex    `json:"body"`
}

// RecordOutput is the frozen half of a record vector.
type RecordOutput struct {
	AreaTag   Hex `json:"area_tag"`
	Canonical Hex `json:"canonical"`
	ID        Hex `json:"id"`
	Signature Hex `json:"signature"`
	Marshal   Hex `json:"marshal"`
}

// RecordVector is one entry in records.json.
type RecordVector struct {
	Name  string      `json:"name"`
	Note  string      `json:"note,omitempty"`
	Input RecordInput `json:"input"`
	RecordOutput
}

// DeriveRecord builds and signs the record an input describes.
func DeriveRecord(in RecordInput, keys KeySet) (*record.Record, RecordOutput, error) {
	key, ok := keys[in.Key]
	if !ok {
		return nil, RecordOutput{}, fmt.Errorf("no key named %q in keys.json", in.Key)
	}
	if !record.Type(in.Type).Valid() {
		return nil, RecordOutput{}, fmt.Errorf("unknown record type %d", in.Type)
	}

	var tag record.AreaTag
	if in.Area != "" {
		tag = record.AreaTagFor(in.Area)
	}
	r := record.Record{
		Origin: key.ID(),
		Seq:    in.Seq,
		TS:     in.TS,
		Type:   record.Type(in.Type),
		Area:   tag,
		Body:   in.Body,
	}
	if len(in.Parent) > 0 {
		if len(in.Parent) != record.IDLen {
			return nil, RecordOutput{}, fmt.Errorf("parent is %d bytes, want %d", len(in.Parent), record.IDLen)
		}
		copy(r.Parent[:], in.Parent)
	}

	signed, err := record.New(key, r)
	if err != nil {
		return nil, RecordOutput{}, err
	}
	id := signed.ID()
	return signed, RecordOutput{
		AreaTag:   Hex(tag[:]),
		Canonical: Hex(signed.SignedBytes()),
		ID:        Hex(id[:]),
		Signature: Hex(signed.Signature()),
		Marshal:   Hex(signed.Marshal()),
	}, nil
}

// ---------------------------------------------------------------------------
// Layer 1b — record bodies
// ---------------------------------------------------------------------------

// Body kinds. These are the typed payload encoders, each of which has its own
// length rules and its own history of second wire forms.
const (
	BodyNode       = "node"
	BodyProfile    = "profile"
	BodySuccession = "succession"
	BodyFile       = "file"
	BodyDoorEvent  = "door_event"
)

// BodyVector is one entry in bodies.json. Input is decoded per Kind.
type BodyVector struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Note    string          `json:"note,omitempty"`
	Input   json.RawMessage `json:"input"`
	Encoded Hex             `json:"encoded"`
}

// The per-kind input shapes. Where a field is a key or a node, the vector names
// a key from keys.json rather than repeating its bytes: one place to look when a
// node ID in a vector needs explaining.
type (
	NodeBodyInput struct {
		Key          string `json:"key"`
		DisplayName  string `json:"display_name"`
		SysopContact string `json:"sysop_contact"`
		Incarnation  uint32 `json:"incarnation"`
	}
	ProfileBodyInput struct {
		Nick  string `json:"nick"`
		DMKey Hex    `json:"dm_key"`
		Flags uint8  `json:"flags"`
	}
	SuccessionBodyInput struct {
		SuccessorKey string `json:"successor_key"`
		Effective    uint32 `json:"effective"`
	}
	FileBodyInput struct {
		Name        string   `json:"name"`
		Size        uint64   `json:"size"`
		Hash        Hex      `json:"hash"`
		Description string   `json:"description"`
		Tags        []string `json:"tags,omitempty"`
	}
	DoorEventInput struct {
		Kind      uint8  `json:"kind"`
		Actor     string `json:"actor"`
		Target    string `json:"target,omitempty"`
		TargetKey string `json:"target_key,omitempty"`
		Payload   Hex    `json:"payload,omitempty"`
	}
	DoorEventBodyInput struct {
		Game   string           `json:"game"`
		Events []DoorEventInput `json:"events"`
	}
)

// DeriveBody encodes the body a vector describes.
func DeriveBody(kind string, raw json.RawMessage, keys KeySet) ([]byte, error) {
	lookup := func(name string) (identity.NodeKey, error) {
		k, ok := keys[name]
		if !ok {
			return identity.NodeKey{}, fmt.Errorf("no key named %q in keys.json", name)
		}
		return k, nil
	}

	switch kind {
	case BodyNode:
		var in NodeBodyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		key, err := lookup(in.Key)
		if err != nil {
			return nil, err
		}
		return record.MarshalNodeBody(record.NodeBody{
			PublicKey:    key.Public,
			DisplayName:  in.DisplayName,
			SysopContact: in.SysopContact,
			Incarnation:  in.Incarnation,
		})

	case BodyProfile:
		var in ProfileBodyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		var dm [record.X25519KeyLen]byte
		if len(in.DMKey) != len(dm) {
			return nil, fmt.Errorf("dm_key is %d bytes, want %d", len(in.DMKey), len(dm))
		}
		copy(dm[:], in.DMKey)
		return record.MarshalProfileBody(record.ProfileBody{
			Nick:  in.Nick,
			DMKey: dm,
			Flags: record.ProfileFlags(in.Flags),
		})

	case BodySuccession:
		var in SuccessionBodyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		succ, err := lookup(in.SuccessorKey)
		if err != nil {
			return nil, err
		}
		return record.MarshalSuccessionBody(record.SuccessionBody{
			Successor:    succ.ID(),
			NewPublicKey: succ.Public,
			Effective:    in.Effective,
		})

	case BodyFile:
		var in FileBodyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		var h [record.FileHashLen]byte
		if len(in.Hash) != len(h) {
			return nil, fmt.Errorf("hash is %d bytes, want %d", len(in.Hash), len(h))
		}
		copy(h[:], in.Hash)
		return record.MarshalFileBody(record.FileBody{
			Name:        in.Name,
			Size:        in.Size,
			Hash:        h,
			Description: in.Description,
			Tags:        in.Tags,
		})

	case BodyDoorEvent:
		var in DoorEventBodyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		events := make([]record.DoorEvent, 0, len(in.Events))
		for _, e := range in.Events {
			ev := record.DoorEvent{
				Kind:    e.Kind,
				Actor:   e.Actor,
				Target:  e.Target,
				Payload: e.Payload,
			}
			if e.TargetKey != "" {
				k, err := lookup(e.TargetKey)
				if err != nil {
					return nil, err
				}
				ev.TargetNode = k.ID()
			}
			events = append(events, ev)
		}
		return record.MarshalDoorEventBody(record.DoorEventBody{Game: in.Game, Events: events})

	default:
		return nil, fmt.Errorf("unknown body kind %q", kind)
	}
}

// ---------------------------------------------------------------------------
// Layer 2 — bundles
// ---------------------------------------------------------------------------

// BundleInput names records from records.json rather than repeating their bytes.
// The cross-file reference is deliberate: it makes a bundle vector a statement
// about COMPOSITION — the origin table, the timestamp deltas, the length
// prefixes — rather than a second copy of the record vectors.
type BundleInput struct {
	Area    string   `json:"area"`
	BaseTS  uint32   `json:"base_ts"`
	Records []string `json:"records"`
}

// BundleVector pins the UNCOMPRESSED body framing.
//
// Not the packed form: bundle.Pack runs the body through zstd, and that output
// moves whenever klauspost/compress changes its encoder or the dictionary is
// retrained (§7.4 wants exactly that before the freeze). Pinning it would freeze
// a compressor's behaviour rather than a format. What IS the format is
// bundle.EncodeBody, and it is deterministic by construction.
type BundleVector struct {
	Name  string      `json:"name"`
	Note  string      `json:"note,omitempty"`
	Input BundleInput `json:"input"`
	Body  Hex         `json:"body"`
}

// BundleDecodeVector is the other direction: a packed blob frozen once, which
// must decode to the named body forever.
//
// This is what covers compression without pinning a compressor. It also quietly
// enforces §7.4's promise that old dictionaries stay supported — retraining that
// edited dictionary 0 in place, rather than shipping a dictionary 1, would fail
// here and nowhere else.
type BundleDecodeVector struct {
	Name       string `json:"name"`
	Note       string `json:"note,omitempty"`
	DictID     uint8  `json:"dict_id"`
	Packed     Hex    `json:"packed"`
	ExpectBody Hex    `json:"expect_body"`
}

// DeriveBundleBody builds the uncompressed framing for a bundle vector.
func DeriveBundleBody(in BundleInput, records map[string]*record.Record) ([]byte, error) {
	b := &bundle.Bundle{Area: record.AreaTagFor(in.Area), BaseTS: in.BaseTS}
	for _, name := range in.Records {
		r, ok := records[name]
		if !ok {
			return nil, fmt.Errorf("no record vector named %q", name)
		}
		b.Records = append(b.Records, r)
	}
	return bundle.EncodeBody(b)
}

// ---------------------------------------------------------------------------
// Layer 3 — the control plane
// ---------------------------------------------------------------------------

// Control message kinds.
const (
	CtrlDigest    = "digest"
	CtrlVectorReq = "vector_req"
	CtrlVectorMsg = "vector_msg"
	CtrlRangeReq  = "range_req"
	CtrlVVEncode  = "vv_encode"
	CtrlVVHash    = "vv_hash"
)

// ControlVector is one entry in control.json.
type ControlVector struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Note    string          `json:"note,omitempty"`
	Input   json.RawMessage `json:"input"`
	Encoded Hex             `json:"encoded"`
}

// Control input shapes.
type (
	// DigestAreaInput is one digest entry. Areas are given in whatever order the
	// vector lists them, because Encode sorts and a vector with unsorted input is
	// how that gets pinned.
	DigestAreaInput struct {
		Area  string `json:"area"`
		Hash  Hex    `json:"hash"`
		Count uint16 `json:"count"`
	}
	DigestInput struct {
		Areas []DigestAreaInput `json:"areas"`
	}
	VectorReqInput struct {
		Areas []string `json:"areas"`
	}
	// VectorEntryInput is one (origin, seq) pair in a version vector.
	VectorEntryInput struct {
		Key string `json:"key"`
		Seq uint64 `json:"seq"`
	}
	VectorMsgInput struct {
		Area    string             `json:"area"`
		Entries []VectorEntryInput `json:"entries"`
	}
	RangeInput struct {
		Key  string `json:"key"`
		From uint64 `json:"from"`
		To   uint64 `json:"to"`
	}
	RangeReqInput struct {
		Area   string       `json:"area"`
		Ranges []RangeInput `json:"ranges"`
	}
	VectorInput struct {
		Entries []VectorEntryInput `json:"entries"`
	}
)

// DeriveControl encodes the control message a vector describes.
func DeriveControl(kind string, raw json.RawMessage, keys KeySet) ([]byte, error) {
	buildVector := func(entries []VectorEntryInput) (*vv.Vector, error) {
		v := vv.New()
		for _, e := range entries {
			k, ok := keys[e.Key]
			if !ok {
				return nil, fmt.Errorf("no key named %q in keys.json", e.Key)
			}
			v.Set(k.ID(), e.Seq)
		}
		return v, nil
	}

	switch kind {
	case CtrlDigest:
		var in DigestInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		d := &gossip.Digest{}
		for _, a := range in.Areas {
			var h [4]byte
			if len(a.Hash) != len(h) {
				return nil, fmt.Errorf("digest hash is %d bytes, want %d", len(a.Hash), len(h))
			}
			copy(h[:], a.Hash)
			d.Areas = append(d.Areas, gossip.AreaState{
				Tag: record.AreaTagFor(a.Area), Hash: h, Count: a.Count,
			})
		}
		return d.Encode(), nil

	case CtrlVectorReq:
		var in VectorReqInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		req := &gossip.VectorReq{}
		for _, a := range in.Areas {
			req.Areas = append(req.Areas, record.AreaTagFor(a))
		}
		return req.Encode(), nil

	case CtrlVectorMsg:
		var in VectorMsgInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		v, err := buildVector(in.Entries)
		if err != nil {
			return nil, err
		}
		msg := &gossip.VectorMsg{Area: record.AreaTagFor(in.Area), Vector: v}
		return msg.Encode(), nil

	case CtrlRangeReq:
		var in RangeReqInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		req := &gossip.RangeReq{Area: record.AreaTagFor(in.Area)}
		for _, rg := range in.Ranges {
			k, ok := keys[rg.Key]
			if !ok {
				return nil, fmt.Errorf("no key named %q in keys.json", rg.Key)
			}
			req.Ranges = append(req.Ranges, vv.Range{Origin: k.ID(), From: rg.From, To: rg.To})
		}
		return req.Encode(), nil

	case CtrlVVEncode:
		var in VectorInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		v, err := buildVector(in.Entries)
		if err != nil {
			return nil, err
		}
		return v.Encode(), nil

	case CtrlVVHash:
		var in VectorInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		v, err := buildVector(in.Entries)
		if err != nil {
			return nil, err
		}
		h := v.Hash()
		return h[:], nil

	default:
		return nil, fmt.Errorf("unknown control kind %q", kind)
	}
}

// ---------------------------------------------------------------------------
// Layer 3b — the fountain codec
// ---------------------------------------------------------------------------

// SymbolInput describes a symbol header to encode.
type SymbolInput struct {
	BundleID uint32 `json:"bundle_id"`
	Index    uint16 `json:"index"`
	K        uint8  `json:"k"`
	Data     Hex    `json:"data"`
}

// SymbolVector pins Symbol.Encode, including the index split across bytes 5
// and 7 that a reader would not guess from the field order.
type SymbolVector struct {
	Name    string      `json:"name"`
	Note    string      `json:"note,omitempty"`
	Input   SymbolInput `json:"input"`
	Encoded Hex         `json:"encoded"`
}

// MaskInput describes one repair symbol's source-symbol mask.
type MaskInput struct {
	SenderKey string `json:"sender_key"`
	BundleID  uint32 `json:"bundle_id"`
	Index     uint16 `json:"index"`
	K         int    `json:"k"`
}

// MaskVector is the highest-value entry in the corpus.
//
// The mask says which source symbols XOR into a repair symbol, and it NEVER
// travels on the wire — both ends derive it from (sender, bundle_id, index).
// An implementation that derives it differently does not fail: it decodes to
// garbage, silently, with every length and checksum agreeing. There is no other
// part of BSMP where being wrong is this quiet, and nothing outside the fountain
// package's own round-trip tests pins it today.
//
// Mask is a bitfield, LSB-first within each byte: source symbol i is included
// when Mask[i/8] & (1 << (i%8)) is set. Degree is the popcount, recorded so a
// vector that disagrees says whether the derivation drifted or only its order.
type MaskVector struct {
	Name   string    `json:"name"`
	Note   string    `json:"note,omitempty"`
	Input  MaskInput `json:"input"`
	Mask   Hex       `json:"mask"`
	Degree int       `json:"degree"`
}

// RepairInput describes a repair symbol produced by a real encoder.
type RepairInput struct {
	SenderKey string `json:"sender_key"`
	BundleID  uint32 `json:"bundle_id"`
	Payload   Hex    `json:"payload"`
	SymSize   int    `json:"sym_size"`
	Index     uint16 `json:"index"`
}

// RepairVector is the black-box companion to MaskVector: an actual symbol off an
// actual encoder. A mask derived differently changes these bytes, so this catches
// the same break through the path that ships, and the two together say whether
// the fault is in the derivation or in how the symbol is assembled from it.
type RepairVector struct {
	Name    string      `json:"name"`
	Note    string      `json:"note,omitempty"`
	Input   RepairInput `json:"input"`
	K       int         `json:"k"`
	Encoded Hex         `json:"encoded"`
}

// DeriveSymbol encodes a symbol header.
func DeriveSymbol(in SymbolInput) []byte {
	return fountain.Symbol{
		BundleID: in.BundleID, Index: in.Index, K: in.K, Data: in.Data,
	}.Encode()
}

// DeriveMask computes a repair symbol's mask as a packed bitfield.
func DeriveMask(in MaskInput, keys KeySet) ([]byte, int, error) {
	key, ok := keys[in.SenderKey]
	if !ok {
		return nil, 0, fmt.Errorf("no key named %q in keys.json", in.SenderKey)
	}
	bits := fountain.Mask(key.ID(), in.BundleID, in.Index, in.K)
	packed := make([]byte, (len(bits)+7)/8)
	degree := 0
	for i, on := range bits {
		if on {
			packed[i/8] |= 1 << uint(i%8)
			degree++
		}
	}
	return packed, degree, nil
}

// DeriveRepair produces a real symbol from a real encoder.
func DeriveRepair(in RepairInput, keys KeySet) ([]byte, int, error) {
	key, ok := keys[in.SenderKey]
	if !ok {
		return nil, 0, fmt.Errorf("no key named %q in keys.json", in.SenderKey)
	}
	enc, err := fountain.NewEncoder(key.ID(), in.BundleID, in.Payload, in.SymSize)
	if err != nil {
		return nil, 0, err
	}
	return enc.Symbol(in.Index).Encode(), enc.K(), nil
}

// ---------------------------------------------------------------------------
// Layer 3c — the mesh link
// ---------------------------------------------------------------------------

// Link frame kinds.
const (
	LinkAnnounce = "announce"
	LinkWhoIs    = "whois"
)

// LinkVector is one entry in meshlink.json.
type LinkVector struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Note    string          `json:"note,omitempty"`
	Input   json.RawMessage `json:"input"`
	Encoded Hex             `json:"encoded"`
}

// Link input shapes.
type (
	AnnounceInput struct {
		Key   string `json:"key"`
		Radio uint32 `json:"radio"`
		At    int64  `json:"at"`
	}
	WhoIsInput struct {
		Target uint32 `json:"target"`
	}
)

// DeriveLink encodes the link frame a vector describes.
func DeriveLink(kind string, raw json.RawMessage, keys KeySet) ([]byte, error) {
	switch kind {
	case LinkAnnounce:
		var in AnnounceInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		key, ok := keys[in.Key]
		if !ok {
			return nil, fmt.Errorf("no key named %q in keys.json", in.Key)
		}
		return meshlink.EncodeAnnounce(key, in.Radio, time.Unix(in.At, 0).UTC()), nil

	case LinkWhoIs:
		var in WhoIsInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		return meshlink.EncodeWhoIs(in.Target), nil

	default:
		return nil, fmt.Errorf("unknown link kind %q", kind)
	}
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

// File names within Dir.
const (
	KeysFile     = "keys.json"
	RecordsFile  = "records.json"
	BodiesFile   = "bodies.json"
	BundlesFile  = "bundles.json"
	ControlFile  = "control.json"
	FountainFile = "fountain.json"
	LinkFile     = "meshlink.json"
)

// FormatVersion in a corpus file is its generation number, stamped so that a
// vector pasted into a bug report still says which snapshot it came from
// without the directory name travelling with it.

// The per-file envelopes. Every file carries format_version so a vector pasted
// into a bug report still says what it is.
type (
	KeysDoc struct {
		FormatVersion int         `json:"format_version"`
		Vectors       []KeyVector `json:"vectors"`
	}
	RecordsDoc struct {
		FormatVersion int            `json:"format_version"`
		Vectors       []RecordVector `json:"vectors"`
	}
	BodiesDoc struct {
		FormatVersion int          `json:"format_version"`
		Vectors       []BodyVector `json:"vectors"`
	}
	ControlDoc struct {
		FormatVersion int             `json:"format_version"`
		Vectors       []ControlVector `json:"vectors"`
	}
	LinkDoc struct {
		FormatVersion int          `json:"format_version"`
		Vectors       []LinkVector `json:"vectors"`
	}

	BundlesDoc struct {
		FormatVersion int                  `json:"format_version"`
		Vectors       []BundleVector       `json:"vectors"`
		DecodeOnly    []BundleDecodeVector `json:"decode_only"`
	}

	FountainDoc struct {
		FormatVersion int            `json:"format_version"`
		Symbols       []SymbolVector `json:"symbols"`
		Masks         []MaskVector   `json:"masks"`
		Repairs       []RepairVector `json:"repairs"`
	}
)

// Corpus is the whole frozen set.
type Corpus struct {
	Keys     KeysDoc
	Records  RecordsDoc
	Bodies   BodiesDoc
	Bundles  BundlesDoc
	Control  ControlDoc
	Fountain FountainDoc
	Link     LinkDoc
}

// KeySet builds the name-to-key map every other layer resolves against.
func (c *Corpus) KeySet() (KeySet, error) {
	out := make(KeySet, len(c.Keys.Vectors))
	for _, kv := range c.Keys.Vectors {
		key, err := KeyFromSeed(kv.Seed)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", kv.Name, err)
		}
		out[kv.Name] = key
	}
	return out, nil
}

// Load reads the corpus from dir.
func Load(dir string) (*Corpus, error) {
	c := &Corpus{}
	files := []struct {
		name string
		into any
	}{
		{KeysFile, &c.Keys},
		{RecordsFile, &c.Records},
		{BodiesFile, &c.Bodies},
		{BundlesFile, &c.Bundles},
		{ControlFile, &c.Control},
		{FountainFile, &c.Fountain},
		{LinkFile, &c.Link},
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		// An unknown field means the schema drifted from the corpus, which is a
		// mistake worth failing on rather than ignoring half a vector.
		dec.DisallowUnknownFields()
		if err := dec.Decode(f.into); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return c, nil
}
