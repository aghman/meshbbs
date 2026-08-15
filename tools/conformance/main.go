// Command conformance generates the frozen BSMP wire-format vectors of design
// §12.6 into internal/conformance/testdata/v1.
//
// # It is append-only, and that is the point
//
// §12.6 asks for a corpus "generated once and thereafter immutable". Immutable
// by convention lasts until the first red build, when regenerating is the
// obvious fix and is also exactly the wrong one: it turns a wire-format break
// into a passing test and a diff nobody reads closely. So immutability is
// mechanical here. For every vector already in the corpus this command
// recomputes the bytes and compares; a single difference is a fatal error naming
// the vector, not a rewrite. Only names absent from the corpus are appended.
//
// The refusal is the same one identity.SaveNodeKey makes about a node key, for
// the same reason: some artifacts are only worth anything if overwriting them
// takes a deliberate act outside the program.
//
// Run: go run ./tools/conformance
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/conformance"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/record"
	"lukechampine.com/blake3"
)

// corpusDir is where the vectors live, relative to the repository root.
const corpusDir = "internal/conformance/" + conformance.Dir

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "conformance: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		return err
	}

	keys, keyVectors, err := buildKeys()
	if err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	records, recordVectors, err := buildRecords(keys)
	if err != nil {
		return fmt.Errorf("records: %w", err)
	}
	bodyVectors, err := buildBodies(keys)
	if err != nil {
		return fmt.Errorf("bodies: %w", err)
	}
	bundleVectors, decodeVectors, err := buildBundles(records)
	if err != nil {
		return fmt.Errorf("bundles: %w", err)
	}
	controlVectors, err := buildControl(keys)
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	symbols, masks, repairs, err := buildFountain(keys)
	if err != nil {
		return fmt.Errorf("fountain: %w", err)
	}
	linkVectors, err := buildLink(keys)
	if err != nil {
		return fmt.Errorf("meshlink: %w", err)
	}

	docs := []doc{
		{conformance.KeysFile, []section{{"vectors", toAny(keyVectors)}}},
		{conformance.RecordsFile, []section{{"vectors", toAny(recordVectors)}}},
		{conformance.BodiesFile, []section{{"vectors", toAny(bodyVectors)}}},
		{conformance.BundlesFile, []section{
			{"vectors", toAny(bundleVectors)},
			{"decode_only", toAny(decodeVectors)},
		}},
		{conformance.ControlFile, []section{{"vectors", toAny(controlVectors)}}},
		{conformance.FountainFile, []section{
			{"symbols", toAny(symbols)},
			{"masks", toAny(masks)},
			{"repairs", toAny(repairs)},
		}},
		{conformance.LinkFile, []section{{"vectors", toAny(linkVectors)}}},
	}

	added := 0
	for _, d := range docs {
		n, err := merge(filepath.Join(corpusDir, d.file), d)
		if err != nil {
			return err
		}
		added += n
	}
	if added == 0 {
		fmt.Println("corpus is up to date; nothing appended")
	} else {
		fmt.Printf("appended %d vector(s)\n", added)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Keys
// ---------------------------------------------------------------------------

// keyNames are the identities every other layer refers to.
//
// Four rather than one because several properties are only visible with more
// than a single node: a bundle's origin table, a version vector's ordering, and
// the fountain mask's dependence on the SENDER all need at least two, and a
// three-origin range request needs a third that sorts between them.
var keyNames = []string{"node-a", "node-b", "node-c", "node-d"}

// keySeed derives a vector's Ed25519 seed from its name.
//
// Deterministic so the corpus can be rebuilt from nothing and audited, and
// obviously synthetic so nobody mistakes one for a key that ever guarded
// anything. The seeds are frozen in keys.json regardless; this only decides what
// they were the first time.
func keySeed(name string) []byte {
	sum := blake3.Sum256([]byte("meshbbs/conformance/v1/key/" + name))
	return sum[:]
}

func buildKeys() (conformance.KeySet, []conformance.KeyVector, error) {
	set := conformance.KeySet{}
	var out []conformance.KeyVector
	for _, name := range keyNames {
		kv, err := conformance.DeriveKey(name, keySeed(name))
		if err != nil {
			return nil, nil, err
		}
		key, err := conformance.KeyFromSeed(kv.Seed)
		if err != nil {
			return nil, nil, err
		}
		set[name] = key
		out = append(out, kv)
	}
	return set, out, nil
}

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// recordSpec is a record vector before its parent reference is resolved.
//
// ParentOf names an earlier vector rather than repeating its ID, because a
// parent IS a record ID and writing one by hand is how a vector ends up pinning
// a number that never came out of the hasher.
type recordSpec struct {
	name     string
	note     string
	parentOf string
	in       conformance.RecordInput
}

func buildRecords(keys conformance.KeySet) (map[string]*record.Record, []conformance.RecordVector, error) {
	// Bodies for the typed record kinds, so a record vector carries a payload a
	// real record would have rather than arbitrary bytes.
	profileBody, err := conformance.DeriveBody(conformance.BodyProfile, mustJSON(conformance.ProfileBodyInput{
		Nick: "austin", DMKey: repeatHex(0x21, 32), Flags: 0,
	}), keys)
	if err != nil {
		return nil, nil, err
	}
	nodeBody, err := conformance.DeriveBody(conformance.BodyNode, mustJSON(conformance.NodeBodyInput{
		Key: "node-a", DisplayName: "pnw-bbs", SysopContact: "sysop@pnw", Incarnation: 1,
	}), keys)
	if err != nil {
		return nil, nil, err
	}
	successionBody, err := conformance.DeriveBody(conformance.BodySuccession, mustJSON(conformance.SuccessionBodyInput{
		SuccessorKey: "node-b", Effective: 1700003600,
	}), keys)
	if err != nil {
		return nil, nil, err
	}
	fileBody, err := conformance.DeriveBody(conformance.BodyFile, mustJSON(conformance.FileBodyInput{
		Name: "meshbbs-0.20.tar.gz", Size: 1048576, Hash: repeatHex(0x5a, record.FileHashLen),
		Description: "source release",
	}), keys)
	if err != nil {
		return nil, nil, err
	}
	doorBody, err := conformance.DeriveBody(conformance.BodyDoorEvent, mustJSON(conformance.DoorEventBodyInput{
		Game: "tradewars",
		Events: []conformance.DoorEventInput{
			{Kind: 3, Actor: "austin", Target: "kestrel", TargetKey: "node-b", Payload: []byte{0x01, 0x02}},
		},
	}), keys)
	if err != nil {
		return nil, nil, err
	}

	const ts = 1700000000
	post := func(name, note string, mut func(*conformance.RecordInput)) recordSpec {
		in := conformance.RecordInput{
			Key: "node-a", Seq: 1, TS: ts, Type: uint8(record.TypePost),
			Area: "general", Body: []byte("Hello, mesh.\n"),
		}
		mut(&in)
		return recordSpec{name: name, note: note, in: in}
	}

	specs := []recordSpec{
		post("post-toplevel", "the ordinary case: no parent, so no parent field is written", func(*conformance.RecordInput) {}),
		{
			name:     "post-threaded",
			note:     "sets flagHasParent and carries the 16-byte parent that top-level records do not pay for",
			parentOf: "post-toplevel",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 2, TS: ts + 60, Type: uint8(record.TypePost),
				Area: "general", Body: []byte("Re: Hello, mesh.\n"),
			},
		},

		// Sequence numbers across every uvarint width boundary. A record ID is a
		// hash of these bytes, so an encoder that widened a varint would change
		// every ID in the network without changing any field.
		post("post-seq-0", "seq 0", func(in *conformance.RecordInput) { in.Seq = 0 }),
		post("post-seq-127", "last seq in a one-byte uvarint", func(in *conformance.RecordInput) { in.Seq = 127 }),
		post("post-seq-128", "first seq needing two bytes", func(in *conformance.RecordInput) { in.Seq = 128 }),
		post("post-seq-16383", "last seq in two bytes", func(in *conformance.RecordInput) { in.Seq = 16383 }),
		post("post-seq-16384", "first seq needing three bytes", func(in *conformance.RecordInput) { in.Seq = 16384 }),
		post("post-seq-max", "seq at the uint64 ceiling, ten uvarint bytes", func(in *conformance.RecordInput) {
			in.Seq = ^uint64(0)
		}),

		// Timestamps are four fixed big-endian bytes, and the endianness is the
		// thing a refactor is most likely to flip without noticing.
		post("post-ts-zero", "ts 0", func(in *conformance.RecordInput) { in.TS = 0 }),
		post("post-ts-max", "ts 0xffffffff, which pins the byte order", func(in *conformance.RecordInput) {
			in.TS = 0xFFFFFFFF
		}),

		// Body length crosses the same varint boundary as seq, one field later.
		post("post-body-empty", "zero-length body", func(in *conformance.RecordInput) { in.Body = nil }),
		post("post-body-127", "body length in one uvarint byte", func(in *conformance.RecordInput) {
			in.Body = bytes.Repeat([]byte("a"), 127)
		}),
		post("post-body-128", "body length needing two uvarint bytes", func(in *conformance.RecordInput) {
			in.Body = bytes.Repeat([]byte("a"), 128)
		}),
		post("post-body-max", "body at MaxBodyLen", func(in *conformance.RecordInput) {
			in.Body = bytes.Repeat([]byte("a"), record.MaxBodyLen)
		}),

		// A body that is not ASCII, because the encoding must be byte-transparent
		// and a helpful normalisation somewhere would be invisible otherwise.
		post("post-body-utf8", "multi-byte UTF-8 and a CP437-era block glyph", func(in *conformance.RecordInput) {
			in.Body = []byte("73 de Åßl █▓▒░\n")
		}),

		{
			name: "dm-basic", note: "DM in the reserved _mail area; the body is sealed bytes, opaque here",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 7, TS: ts, Type: uint8(record.TypeDM),
				Area: "_mail", Body: repeatHex(0xC3, 48),
			},
		},
		{
			name: "profile-listed", note: "PROFILE in the reserved _directory area",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 8, TS: ts, Type: uint8(record.TypeProfile),
				Area: "_directory", Body: profileBody,
			},
		},
		{
			name: "node-record", note: "NODE belongs to no area, so the area tag is zero rather than AreaTagFor(\"\")",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 9, TS: ts, Type: uint8(record.TypeNode),
				Area: "", Body: nodeBody,
			},
		},
		{
			name: "succession-record", note: "SUCCESSION is signed by the OLD key and names the new one; also arealess",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 10, TS: ts, Type: uint8(record.TypeSuccession),
				Area: "", Body: successionBody,
			},
		},
		{
			name: "area-record", note: "AREA metadata; body is opaque to the envelope",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 11, TS: ts, Type: uint8(record.TypeArea),
				Area: "general", Body: []byte("General chatter"),
			},
		},
		{
			name: "file-record", note: "FILE carries a catalog entry and never content (§7.5)",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 12, TS: ts, Type: uint8(record.TypeFile),
				Area: "files/uploads", Body: fileBody,
			},
		},
		{
			name: "tombstone-record", note: "TOMBSTONE over a record ID",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 13, TS: ts, Type: uint8(record.TypeTombstone),
				Area: "general", Body: repeatHex(0x11, record.IDLen),
			},
		},
		{
			name: "vote-record", note: "VOTE; body is opaque to the envelope",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 14, TS: ts, Type: uint8(record.TypeVote),
				Area: "general", Body: []byte{0x01},
			},
		},
		{
			name: "door-event-record", note: "DOOR_EVENT on a federated league area (§9.5)",
			in: conformance.RecordInput{
				Key: "node-a", Seq: 15, TS: ts, Type: uint8(record.TypeDoorEvent),
				Area: "league/tradewars", Body: doorBody,
			},
		},

		// A second origin, so bundle vectors have more than one to hoist.
		post("post-from-node-b", "same shape, different origin", func(in *conformance.RecordInput) {
			in.Key = "node-b"
			in.Seq = 3
			in.TS = ts + 120
			in.Body = []byte("From another board.\n")
		}),
		post("post-from-node-c", "a third origin, timestamped BEFORE the bundle base", func(in *conformance.RecordInput) {
			in.Key = "node-c"
			in.Seq = 4
			in.TS = ts - 3600
			in.Body = []byte("Backfilled.\n")
		}),
	}

	built := map[string]*record.Record{}
	var out []conformance.RecordVector
	for _, s := range specs {
		in := s.in
		if s.parentOf != "" {
			parent, ok := built[s.parentOf]
			if !ok {
				return nil, nil, fmt.Errorf("%s: no earlier vector named %q", s.name, s.parentOf)
			}
			id := parent.ID()
			in.Parent = append(conformance.Hex(nil), id[:]...)
		}
		r, outputs, err := conformance.DeriveRecord(in, keys)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", s.name, err)
		}
		built[s.name] = r
		out = append(out, conformance.RecordVector{
			Name: s.name, Note: s.note, Input: in, RecordOutput: outputs,
		})
	}
	return built, out, nil
}

// ---------------------------------------------------------------------------
// Bodies
// ---------------------------------------------------------------------------

func buildBodies(keys conformance.KeySet) ([]conformance.BodyVector, error) {
	type spec struct {
		name, kind, note string
		in               any
	}

	specs := []spec{
		{"node-min", conformance.BodyNode, "both labels empty, each costing one length byte",
			conformance.NodeBodyInput{Key: "node-a"}},
		{"node-typical", conformance.BodyNode, "",
			conformance.NodeBodyInput{Key: "node-a", DisplayName: "pnw-bbs", SysopContact: "sysop@pnw", Incarnation: 1}},
		{"node-max", conformance.BodyNode, "labels at MaxDisplayNameLen and MaxSysopContactLen",
			conformance.NodeBodyInput{
				Key:          "node-b",
				DisplayName:  strings.Repeat("n", record.MaxDisplayNameLen),
				SysopContact: strings.Repeat("c", record.MaxSysopContactLen),
				Incarnation:  0xFFFFFFFF,
			}},

		{"profile-listed", conformance.BodyProfile, "",
			conformance.ProfileBodyInput{Nick: "austin", DMKey: repeatHex(0x21, 32)}},
		{"profile-unlisted", conformance.BodyProfile, "FlagUnlisted set: was listed, has opted out (§6.7)",
			conformance.ProfileBodyInput{Nick: "austin", DMKey: repeatHex(0x21, 32), Flags: 1}},
		{"profile-nick-max", conformance.BodyProfile, "nick at MaxNickLen",
			conformance.ProfileBodyInput{Nick: strings.Repeat("n", record.MaxNickLen), DMKey: repeatHex(0x22, 32)}},

		{"succession-basic", conformance.BodySuccession, "fixed width: 8 + 32 + 4, no length field to disagree with",
			conformance.SuccessionBodyInput{SuccessorKey: "node-b", Effective: 1700003600}},

		{"file-min", conformance.BodyFile, "one-character name, no description, no tags",
			conformance.FileBodyInput{Name: "a", Size: 0, Hash: repeatHex(0x5a, record.FileHashLen)}},
		{"file-size-127", conformance.BodyFile, "size in one uvarint byte",
			conformance.FileBodyInput{Name: "small.txt", Size: 127, Hash: repeatHex(0x5b, record.FileHashLen)}},
		{"file-size-128", conformance.BodyFile, "size needing two uvarint bytes — the boundary the first fuzz run found an overlong encoding at",
			conformance.FileBodyInput{Name: "small.txt", Size: 128, Hash: repeatHex(0x5b, record.FileHashLen)}},
		{"file-size-max", conformance.BodyFile, "size at the uint64 ceiling",
			conformance.FileBodyInput{Name: "huge.bin", Size: ^uint64(0), Hash: repeatHex(0x5c, record.FileHashLen)}},
		{"file-tags-one", conformance.BodyFile, "",
			conformance.FileBodyInput{
				Name: "ansi-pack.zip", Size: 4096, Hash: repeatHex(0x5d, record.FileHashLen),
				Description: "art pack", Tags: []string{"art"},
			}},
		{"file-max", conformance.BodyFile, "every bounded field at its limit",
			conformance.FileBodyInput{
				Name:        strings.Repeat("f", record.MaxFileNameLen),
				Size:        1 << 40,
				Hash:        repeatHex(0x5e, record.FileHashLen),
				Description: strings.Repeat("d", record.MaxFileDescLen),
				Tags: []string{
					strings.Repeat("t", record.MaxFileTagLen),
					strings.Repeat("u", record.MaxFileTagLen),
					strings.Repeat("v", record.MaxFileTagLen),
				},
			}},

		{"door-event-single", conformance.BodyDoorEvent, "one event, no target, no payload — the minimum a league record can say",
			conformance.DoorEventBodyInput{
				Game:   "tradewars",
				Events: []conformance.DoorEventInput{{Kind: 1, Actor: "austin"}},
			}},
		{"door-event-target", conformance.BodyDoorEvent, "a target writes its nick AND its node; absent, neither is on the wire",
			conformance.DoorEventBodyInput{
				Game: "tradewars",
				Events: []conformance.DoorEventInput{
					{Kind: 3, Actor: "austin", Target: "kestrel", TargetKey: "node-b", Payload: []byte{0x01, 0x02}},
				},
			}},
		{"door-event-max", conformance.BodyDoorEvent, "MaxDoorEventsPerRecord events with every bounded field at its limit",
			maxDoorEventBody()},
	}

	var out []conformance.BodyVector
	for _, s := range specs {
		raw := mustJSON(s.in)
		encoded, err := conformance.DeriveBody(s.kind, raw, keys)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		out = append(out, conformance.BodyVector{
			Name: s.name, Kind: s.kind, Note: s.note, Input: raw, Encoded: encoded,
		})
	}
	return out, nil
}

func maxDoorEventBody() conformance.DoorEventBodyInput {
	events := make([]conformance.DoorEventInput, 0, record.MaxDoorEventsPerRecord)
	for i := 0; i < record.MaxDoorEventsPerRecord; i++ {
		events = append(events, conformance.DoorEventInput{
			Kind:      uint8(i),
			Actor:     strings.Repeat("a", record.MaxNickLen),
			Target:    strings.Repeat("t", record.MaxNickLen),
			TargetKey: "node-b",
			Payload:   repeatHex(byte(0x80+i), record.MaxDoorEventPayloadLen),
		})
	}
	return conformance.DoorEventBodyInput{
		Game:   strings.Repeat("g", record.MaxDoorGameLen),
		Events: events,
	}
}

// ---------------------------------------------------------------------------
// Bundles
// ---------------------------------------------------------------------------

func buildBundles(records map[string]*record.Record) ([]conformance.BundleVector, []conformance.BundleDecodeVector, error) {
	const baseTS = 1700000000

	specs := []struct {
		name, note string
		in         conformance.BundleInput
	}{
		{"bundle-single", "one record, one origin", conformance.BundleInput{
			Area: "general", BaseTS: baseTS, Records: []string{"post-toplevel"},
		}},
		{"bundle-multi-origin", "three origins, so the origin table is built in first-appearance order and each record costs one index byte", conformance.BundleInput{
			Area: "general", BaseTS: baseTS,
			Records: []string{"post-toplevel", "post-from-node-b", "post-threaded", "post-from-node-c"},
		}},
		{"bundle-negative-delta", "a record older than the bundle base, which is why the timestamp delta is a SIGNED varint", conformance.BundleInput{
			Area: "general", BaseTS: baseTS, Records: []string{"post-from-node-c", "post-toplevel"},
		}},
		{"bundle-repeated-origin", "the same origin twice: the table holds one entry and both records index it", conformance.BundleInput{
			Area: "general", BaseTS: baseTS, Records: []string{"post-toplevel", "post-threaded"},
		}},
	}

	var vectors []conformance.BundleVector
	for _, s := range specs {
		body, err := conformance.DeriveBundleBody(s.in, records)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", s.name, err)
		}
		vectors = append(vectors, conformance.BundleVector{
			Name: s.name, Note: s.note, Input: s.in, Body: body,
		})
	}

	// The decode direction. These blobs are frozen compressor output and are
	// never regenerated: the test only asserts they still DECODE, so a zstd
	// upgrade that emits different bytes is free while one that stops reading
	// its own old output is not.
	// Every shipped dictionary appears here. That is what makes editing one in
	// place a red build rather than a field report: §7.4 keeps old dictionaries
	// supported, and content that changed under an unchanged ID is exactly the
	// failure that promise exists to prevent.
	//
	// The dictionary-0 vectors keep the names they were frozen under. Their
	// suffix does not say "dict0" because at the time there was only one, and
	// renaming a frozen vector is not a thing this corpus permits — a third-party
	// implementation may already be testing against the name.
	d0, err := bundle.Dictionary0()
	if err != nil {
		return nil, nil, err
	}
	defer d0.Close()
	d1, err := bundle.Dictionary1()
	if err != nil {
		return nil, nil, err
	}
	defer d1.Close()

	packedFor := func(v conformance.BundleVector, d *bundle.Dictionary) ([]byte, error) {
		b := &bundle.Bundle{Area: record.AreaTagFor(v.Input.Area), BaseTS: v.Input.BaseTS, DictID: d.ID()}
		for _, rn := range v.Input.Records {
			b.Records = append(b.Records, records[rn])
		}
		return bundle.Pack(b, d)
	}
	find := func(name string) conformance.BundleVector {
		for _, c := range vectors {
			if c.Name == name {
				return c
			}
		}
		return conformance.BundleVector{}
	}

	var decode []conformance.BundleDecodeVector
	for _, spec := range []struct {
		dict   *bundle.Dictionary
		suffix string
	}{
		{d0, "-packed"},
		{d1, "-packed-dict1"},
	} {
		for _, name := range []string{"bundle-single", "bundle-multi-origin"} {
			v := find(name)
			packed, err := packedFor(v, spec.dict)
			if err != nil {
				return nil, nil, err
			}
			decode = append(decode, conformance.BundleDecodeVector{
				Name: name + spec.suffix,
				Note: fmt.Sprintf("frozen dictionary-%d output; asserted to DECODE, never to be re-emitted byte for byte",
					spec.dict.ID()),
				DictID:     spec.dict.ID(),
				Packed:     packed,
				ExpectBody: v.Body,
			})
		}
	}
	return vectors, decode, nil
}

// ---------------------------------------------------------------------------
// Control plane
// ---------------------------------------------------------------------------

func buildControl(keys conformance.KeySet) ([]conformance.ControlVector, error) {
	// A digest filled to MaxAreas, which is derived from the MTU rather than
	// chosen: this vector is what notices if that arithmetic moves.
	maxAreas := make([]conformance.DigestAreaInput, 0, gossip.MaxAreas)
	for i := 0; i < gossip.MaxAreas; i++ {
		maxAreas = append(maxAreas, conformance.DigestAreaInput{
			Area:  fmt.Sprintf("area-%02d", i),
			Hash:  repeatHex(byte(i), 4),
			Count: uint16(i * 100),
		})
	}

	entries := []conformance.VectorEntryInput{
		{Key: "node-a", Seq: 1},
		{Key: "node-b", Seq: 127},
		{Key: "node-c", Seq: 128},
		{Key: "node-d", Seq: ^uint64(0)},
	}

	type spec struct {
		name, kind, note string
		in               any
	}
	specs := []spec{
		{"digest-empty", conformance.CtrlDigest, "three bytes and no areas", conformance.DigestInput{}},
		{"digest-one", conformance.CtrlDigest, "", conformance.DigestInput{Areas: []conformance.DigestAreaInput{
			{Area: "general", Hash: repeatHex(0xA1, 4), Count: 42},
		}}},
		{"digest-unsorted", conformance.CtrlDigest, "areas supplied in a deliberately wrong order: Encode sorts by tag, and this is what pins that it does rather than trusting the caller",
			conformance.DigestInput{Areas: []conformance.DigestAreaInput{
				{Area: "zulu", Hash: repeatHex(0xC3, 4), Count: 3},
				{Area: "alpha", Hash: repeatHex(0xC1, 4), Count: 1},
				{Area: "mike", Hash: repeatHex(0xC2, 4), Count: 2},
			}}},
		{"digest-count-saturated", conformance.CtrlDigest, "count at the 16-bit ceiling", conformance.DigestInput{
			Areas: []conformance.DigestAreaInput{{Area: "general", Hash: repeatHex(0xA2, 4), Count: 0xFFFF}},
		}},
		{"digest-max-areas", conformance.CtrlDigest, "a full digest, which must still be one mesh packet", conformance.DigestInput{Areas: maxAreas}},

		{"vector-req-one", conformance.CtrlVectorReq, "", conformance.VectorReqInput{Areas: []string{"general"}}},
		{"vector-req-unsorted", conformance.CtrlVectorReq, "same sorting guarantee as the digest",
			conformance.VectorReqInput{Areas: []string{"zulu", "alpha", "mike"}}},

		{"vector-msg-empty", conformance.CtrlVectorMsg, "an empty vector still encodes its count",
			conformance.VectorMsgInput{Area: "general"}},
		{"vector-msg-four", conformance.CtrlVectorMsg, "origins ascend by ID, not by the order given",
			conformance.VectorMsgInput{Area: "general", Entries: entries}},

		{"range-req-one", conformance.CtrlRangeReq, "", conformance.RangeReqInput{
			Area: "general", Ranges: []conformance.RangeInput{{Key: "node-a", From: 5, To: 9}},
		}},
		{"range-req-multi", conformance.CtrlRangeReq, "ranges sort by origin then by From; the wire carries the SPAN, not the absolute To",
			conformance.RangeReqInput{Area: "general", Ranges: []conformance.RangeInput{
				{Key: "node-c", From: 10, To: 10},
				{Key: "node-a", From: 200, To: 400},
				{Key: "node-a", From: 1, To: 3},
				{Key: "node-b", From: 0, To: 127},
			}}},

		{"vv-encode-empty", conformance.CtrlVVEncode, "", conformance.VectorInput{}},
		{"vv-encode-one", conformance.CtrlVVEncode, "", conformance.VectorInput{
			Entries: []conformance.VectorEntryInput{{Key: "node-a", Seq: 1}},
		}},
		{"vv-encode-four", conformance.CtrlVVEncode, "sequence numbers across the uvarint width boundaries",
			conformance.VectorInput{Entries: entries}},

		{"vv-hash-empty", conformance.CtrlVVHash, "the fingerprint a digest carries for an area holding nothing",
			conformance.VectorInput{}},
		{"vv-hash-four", conformance.CtrlVVHash, "", conformance.VectorInput{Entries: entries}},
	}

	var out []conformance.ControlVector
	for _, s := range specs {
		raw := mustJSON(s.in)
		encoded, err := conformance.DeriveControl(s.kind, raw, keys)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		out = append(out, conformance.ControlVector{
			Name: s.name, Kind: s.kind, Note: s.note, Input: raw, Encoded: encoded,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fountain codec
// ---------------------------------------------------------------------------

func buildFountain(keys conformance.KeySet) ([]conformance.SymbolVector, []conformance.MaskVector, []conformance.RepairVector, error) {
	symbolSpecs := []struct {
		name, note string
		in         conformance.SymbolInput
	}{
		{"symbol-k1-index0", "K=1 needs no coding at all; most DMs land here", conformance.SymbolInput{
			BundleID: 0x01020304, Index: 0, K: 1, Data: []byte("hello"),
		}},
		{"symbol-index-255", "index still fits byte 5", conformance.SymbolInput{
			BundleID: 0xDEADBEEF, Index: 255, K: 10, Data: repeatHex(0x41, 16),
		}},
		{"symbol-index-256", "index 256: the low half is in byte 5 and the high half in byte 7, which is the one thing about this header a reader would not guess",
			conformance.SymbolInput{BundleID: 0xDEADBEEF, Index: 256, K: 10, Data: repeatHex(0x41, 16)}},
		{"symbol-index-max", "index at the uint16 ceiling", conformance.SymbolInput{
			BundleID: 0, Index: 0xFFFF, K: 10, Data: []byte{0x00},
		}},
		{"symbol-k-max", "K at MaxK", conformance.SymbolInput{
			BundleID: 0x11223344, Index: 64, K: 64, Data: repeatHex(0x7E, 225),
		}},
	}

	var symbols []conformance.SymbolVector
	for _, s := range symbolSpecs {
		symbols = append(symbols, conformance.SymbolVector{
			Name: s.name, Note: s.note, Input: s.in, Encoded: conformance.DeriveSymbol(s.in),
		})
	}

	// Masks. Two senders over the same (bundle, index) prove the sender is inside
	// the derivation — the property that stops two nodes colliding on a bundle ID
	// from corrupting each other's decodes.
	maskSpecs := []struct {
		name, note string
		in         conformance.MaskInput
	}{
		{"mask-k1", "K=1 is the special case: the sole source symbol, always", conformance.MaskInput{
			SenderKey: "node-a", BundleID: 1234, Index: 1, K: 1,
		}},
		{"mask-k2-i2", "", conformance.MaskInput{SenderKey: "node-a", BundleID: 1234, Index: 2, K: 2}},
		{"mask-k8-i8", "", conformance.MaskInput{SenderKey: "node-a", BundleID: 1234, Index: 8, K: 8}},
		{"mask-k8-i9", "same block, next index", conformance.MaskInput{SenderKey: "node-a", BundleID: 1234, Index: 9, K: 8}},
		{"mask-k8-i8-other-sender", "identical bundle and index, different sender: the mask MUST differ",
			conformance.MaskInput{SenderKey: "node-b", BundleID: 1234, Index: 8, K: 8}},
		{"mask-k8-i8-other-bundle", "identical sender and index, different bundle",
			conformance.MaskInput{SenderKey: "node-a", BundleID: 5678, Index: 8, K: 8}},
		{"mask-k32-i40", "K=32: exactly one 32-bit draw, no partial batch", conformance.MaskInput{
			SenderKey: "node-a", BundleID: 42, Index: 40, K: 32,
		}},
		{"mask-k33-i40", "K=33 needs a second draw of which only one bit is used, which is where an off-by-one in the batching would show",
			conformance.MaskInput{SenderKey: "node-a", BundleID: 42, Index: 40, K: 33}},
		{"mask-k64-i100", "K at MaxK", conformance.MaskInput{SenderKey: "node-a", BundleID: 42, Index: 100, K: 64}},
	}

	var masks []conformance.MaskVector
	for _, s := range maskSpecs {
		packed, degree, err := conformance.DeriveMask(s.in, keys)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.name, err)
		}
		masks = append(masks, conformance.MaskVector{
			Name: s.name, Note: s.note, Input: s.in, Mask: packed, Degree: degree,
		})
	}

	// Repair symbols off a real encoder: the same property as the masks, checked
	// through the path that actually ships.
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i)
	}
	repairSpecs := []struct {
		name, note string
		in         conformance.RepairInput
	}{
		{"repair-systematic-0", "a systematic symbol is the source fragment verbatim, at zero coding overhead",
			conformance.RepairInput{SenderKey: "node-a", BundleID: 99, Payload: payload, SymSize: 32, Index: 0}},
		{"repair-first", "the first repair symbol, index K", conformance.RepairInput{
			SenderKey: "node-a", BundleID: 99, Payload: payload, SymSize: 32, Index: 7,
		}},
		{"repair-second", "", conformance.RepairInput{
			SenderKey: "node-a", BundleID: 99, Payload: payload, SymSize: 32, Index: 8,
		}},
		{"repair-other-sender", "same block from a different sender: the repair bytes MUST differ",
			conformance.RepairInput{SenderKey: "node-b", BundleID: 99, Payload: payload, SymSize: 32, Index: 7}},
	}

	var repairs []conformance.RepairVector
	for _, s := range repairSpecs {
		encoded, k, err := conformance.DeriveRepair(s.in, keys)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.name, err)
		}
		repairs = append(repairs, conformance.RepairVector{
			Name: s.name, Note: s.note, Input: s.in, K: k, Encoded: encoded,
		})
	}
	return symbols, masks, repairs, nil
}

// ---------------------------------------------------------------------------
// Mesh link
// ---------------------------------------------------------------------------

func buildLink(keys conformance.KeySet) ([]conformance.LinkVector, error) {
	type spec struct {
		name, kind, note string
		in               any
	}
	specs := []spec{
		{"announce-basic", conformance.LinkAnnounce, "the radio number is INSIDE the signature, which is what makes a captured announcement unreplayable from another radio",
			conformance.AnnounceInput{Key: "node-a", Radio: 0x7A3B1C2D, At: 1700000000}},
		{"announce-radio-max", conformance.LinkAnnounce, "",
			conformance.AnnounceInput{Key: "node-b", Radio: 0xFFFFFFFF, At: 1700000000}},
		{"announce-epoch", conformance.LinkAnnounce, "timestamp zero",
			conformance.AnnounceInput{Key: "node-a", Radio: 1, At: 0}},

		{"whois-basic", conformance.LinkWhoIs, "five bytes: the frame type and the radio being asked about",
			conformance.WhoIsInput{Target: 0x7A3B1C2D}},
		{"whois-zero", conformance.LinkWhoIs, "", conformance.WhoIsInput{Target: 0}},
	}

	var out []conformance.LinkVector
	for _, s := range specs {
		raw := mustJSON(s.in)
		encoded, err := conformance.DeriveLink(s.kind, raw, keys)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		out = append(out, conformance.LinkVector{
			Name: s.name, Kind: s.kind, Note: s.note, Input: raw, Encoded: encoded,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Append-only merge
// ---------------------------------------------------------------------------

type section struct {
	key     string
	entries []any
}

type doc struct {
	file     string
	sections []section
}

// merge writes path, refusing to change any vector already in it.
//
// Returns how many vectors were appended.
func merge(path string, d doc) (int, error) {
	existing := map[string]map[string]json.RawMessage{}
	if raw, err := os.ReadFile(path); err == nil {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		for _, s := range d.sections {
			existing[s.key] = map[string]json.RawMessage{}
			arr, ok := top[s.key]
			if !ok {
				continue
			}
			var items []json.RawMessage
			if err := json.Unmarshal(arr, &items); err != nil {
				return 0, fmt.Errorf("%s.%s: %w", path, s.key, err)
			}
			for _, item := range items {
				name, err := entryName(item)
				if err != nil {
					return 0, fmt.Errorf("%s.%s: %w", path, s.key, err)
				}
				existing[s.key][name] = item
			}
		}
	} else if !os.IsNotExist(err) {
		return 0, err
	}

	added := 0
	var rendered []renderSection
	for _, s := range d.sections {
		out := renderSection{key: s.key}
		fresh := map[string]bool{}
		for _, e := range s.entries {
			raw, err := json.Marshal(e)
			if err != nil {
				return 0, err
			}
			name, err := entryName(raw)
			if err != nil {
				return 0, err
			}
			fresh[name] = true

			if old, ok := existing[s.key][name]; ok {
				same, err := sameJSON(old, raw)
				if err != nil {
					return 0, err
				}
				if !same {
					return 0, fmt.Errorf(
						"%s.%s: vector %q already exists and its bytes have CHANGED.\n"+
							"The corpus is append-only (§12.6): a frozen vector that no longer matches means\n"+
							"either the wire format moved or the generator's input for it was edited. Neither is\n"+
							"fixed by regenerating. Work out which encoder changed and why, and if the change is\n"+
							"intended, add a NEW vector under a new name and bump the format version.\n"+
							"  frozen: %s\n  now:    %s",
						path, s.key, name, old, raw)
				}
				// Keep the frozen bytes verbatim rather than the freshly rendered
				// ones, so formatting can never drift a file that has not changed.
				out.raws = append(out.raws, old)
				continue
			}
			out.raws = append(out.raws, raw)
			added++
		}
		for name := range existing[s.key] {
			if !fresh[name] {
				return 0, fmt.Errorf(
					"%s.%s: vector %q is in the corpus but not in the generator.\n"+
						"Vectors are never removed: a third-party implementation may already be testing\n"+
						"against this one. Put its input back.", path, s.key, name)
			}
		}
		rendered = append(rendered, out)
	}

	if added == 0 && fileExists(path) {
		return 0, nil
	}
	if err := os.WriteFile(path, render(rendered), 0o644); err != nil {
		return 0, err
	}
	fmt.Printf("wrote %s (+%d)\n", path, added)
	return added, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func entryName(raw json.RawMessage) (string, error) {
	var probe struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", err
	}
	if probe.Name == "" {
		return "", fmt.Errorf("vector has no name")
	}
	return probe.Name, nil
}

// sameJSON compares two entries by value rather than by formatting.
func sameJSON(a, b json.RawMessage) (bool, error) {
	var ca, cb bytes.Buffer
	if err := json.Compact(&ca, a); err != nil {
		return false, err
	}
	if err := json.Compact(&cb, b); err != nil {
		return false, err
	}
	return ca.String() == cb.String(), nil
}

type renderSection struct {
	key  string
	raws []json.RawMessage
}

// render emits the file. Key order is fixed by construction rather than by a
// map, so a regenerated file that gained nothing is byte-identical.
func render(sections []renderSection) []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	fmt.Fprintf(&b, "  \"format_version\": %d", conformance.FormatVersion)
	for _, s := range sections {
		fmt.Fprintf(&b, ",\n  %q: [", s.key)
		for i, raw := range s.raws {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString("\n    ")
			var pretty bytes.Buffer
			if err := json.Indent(&pretty, raw, "    ", "  "); err != nil {
				// Unreachable: raw came from json.Marshal or a parsed file.
				panic(err)
			}
			b.Write(pretty.Bytes())
		}
		if len(s.raws) > 0 {
			b.WriteString("\n  ")
		}
		b.WriteString("]")
	}
	b.WriteString("\n}\n")
	return b.Bytes()
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

func toAny[T any](in []T) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func repeatHex(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }
