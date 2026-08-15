package conformance

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/meshlink"
	"github.com/aghman/meshbbs/internal/record"
)

// breakNote is appended to every mismatch, because the person reading it is
// usually not the person who knew this file existed.
const breakNote = "\n" +
	"This is a WIRE FORMAT CHANGE (§12.6). The vector above was frozen from a build that\n" +
	"was believed correct; an encoder now produces different bytes for the same input.\n" +
	"Do NOT regenerate the corpus to make this pass — the vectors are append-only and\n" +
	"`go run ./tools/conformance` will refuse anyway. Find the encoder change. If it is\n" +
	"intended, it needs a format version bump and a compatibility story in both\n" +
	"directions, not an edited expectation.\n" +
	"See internal/conformance/testdata/v1/README.md."

func load(t *testing.T) (*Corpus, KeySet) {
	t.Helper()
	c, err := Load(Dir)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	keys, err := c.KeySet()
	if err != nil {
		t.Fatalf("build key set: %v", err)
	}
	return c, keys
}

func same(t *testing.T, vector, field string, want, got []byte) {
	t.Helper()
	if bytes.Equal(want, got) {
		return
	}
	t.Errorf("%s: %s does not match the frozen vector.\n  frozen: %x\n  got:    %x%s",
		vector, field, want, got, breakNote)
}

// TestCorpusMatchesFormatVersions guards the directory name.
//
// testdata/v1 holds vectors for version 1 of each format. If a FormatVersion is
// bumped, these bytes describe a format the code no longer speaks, and the
// vectors belong in a testdata/v2 beside them with cross-version tests in both
// directions (§12.6). Failing here is the reminder to do that rather than to
// quietly retire the old corpus.
func TestCorpusMatchesFormatVersions(t *testing.T) {
	if record.FormatVersion != 1 {
		t.Errorf("record.FormatVersion is %d; %s holds version 1 vectors", record.FormatVersion, Dir)
	}
	if bundle.FormatVersion != 1 {
		t.Errorf("bundle.FormatVersion is %d; %s holds version 1 vectors", bundle.FormatVersion, Dir)
	}
	if gossip.FormatVersion != 1 {
		t.Errorf("gossip.FormatVersion is %d; %s holds version 1 vectors", gossip.FormatVersion, Dir)
	}

	c, _ := load(t)
	docs := map[string]int{
		KeysFile:     c.Keys.FormatVersion,
		RecordsFile:  c.Records.FormatVersion,
		BodiesFile:   c.Bodies.FormatVersion,
		BundlesFile:  c.Bundles.FormatVersion,
		ControlFile:  c.Control.FormatVersion,
		FountainFile: c.Fountain.FormatVersion,
		LinkFile:     c.Link.FormatVersion,
	}
	for name, v := range docs {
		if v != FormatVersion {
			t.Errorf("%s declares format_version %d, want %d", name, v, FormatVersion)
		}
	}
}

// TestCorpusIsPopulated fails if a corpus file is present but empty.
//
// Every check below is a range over a slice, so an empty corpus passes all of
// them in silence — the one failure mode a test made of loops has by default.
func TestCorpusIsPopulated(t *testing.T) {
	c, _ := load(t)
	counts := map[string]int{
		"keys":             len(c.Keys.Vectors),
		"records":          len(c.Records.Vectors),
		"bodies":           len(c.Bodies.Vectors),
		"bundles":          len(c.Bundles.Vectors),
		"bundles/decode":   len(c.Bundles.DecodeOnly),
		"control":          len(c.Control.Vectors),
		"fountain/symbols": len(c.Fountain.Symbols),
		"fountain/masks":   len(c.Fountain.Masks),
		"fountain/repairs": len(c.Fountain.Repairs),
		"meshlink":         len(c.Link.Vectors),
	}
	for name, n := range counts {
		if n == 0 {
			t.Errorf("%s section is empty", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Layer 0 — identity
// ---------------------------------------------------------------------------

func TestKeyVectors(t *testing.T) {
	c, _ := load(t)
	for _, v := range c.Keys.Vectors {
		got, err := DeriveKey(v.Name, v.Seed)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "public_key", v.PublicKey, got.PublicKey)
		same(t, v.Name, "node_id", v.NodeID, got.NodeID)
		for _, f := range []struct{ field, want, got string }{
			{"compact", v.Compact, got.Compact},
			{"grouped", v.Grouped, got.Grouped},
			{"short", v.Short, got.Short},
			{"words", v.Words, got.Words},
		} {
			if f.want != f.got {
				t.Errorf("%s: %s does not match the frozen vector.\n  frozen: %s\n  got:    %s%s",
					v.Name, f.field, f.want, f.got, breakNote)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Layer 1 — records
// ---------------------------------------------------------------------------

func TestRecordVectors(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Records.Vectors {
		r, got, err := DeriveRecord(v.Input, keys)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "area_tag", v.AreaTag, got.AreaTag)
		same(t, v.Name, "canonical", v.Canonical, got.Canonical)
		same(t, v.Name, "id", v.ID, got.ID)
		same(t, v.Name, "signature", v.Signature, got.Signature)
		same(t, v.Name, "marshal", v.Marshal, got.Marshal)

		// The signature must VERIFY, not merely match. A vector frozen from a
		// broken signer would otherwise be reproduced faithfully forever.
		key := keys[v.Input.Key]
		if err := r.Verify(key.Public); err != nil {
			t.Errorf("%s: frozen record does not verify: %v", v.Name, err)
		}
	}
}

// TestRecordVectorsDecode reads the corpus in the direction a peer does.
//
// Deriving proves this build writes the frozen bytes; this proves it still reads
// them. They are different failures: an encoder and a decoder can drift together
// and every round-trip test in the tree would stay green.
func TestRecordVectorsDecode(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Records.Vectors {
		r, err := record.Unmarshal(v.Marshal)
		if err != nil {
			t.Errorf("%s: frozen bytes no longer parse: %v%s", v.Name, err, breakNote)
			continue
		}
		id := r.ID()
		same(t, v.Name, "id (decoded)", v.ID, id[:])
		same(t, v.Name, "canonical (decoded)", v.Canonical, r.SignedBytes())
		same(t, v.Name, "body (decoded)", v.Input.Body, r.Body)

		if r.Seq != v.Input.Seq {
			t.Errorf("%s: decoded seq %d, frozen input says %d%s", v.Name, r.Seq, v.Input.Seq, breakNote)
		}
		if r.TS != v.Input.TS {
			t.Errorf("%s: decoded ts %d, frozen input says %d%s", v.Name, r.TS, v.Input.TS, breakNote)
		}
		if uint8(r.Type) != v.Input.Type {
			t.Errorf("%s: decoded type %d, frozen input says %d%s", v.Name, uint8(r.Type), v.Input.Type, breakNote)
		}
		if err := r.Verify(keys[v.Input.Key].Public); err != nil {
			t.Errorf("%s: decoded record does not verify: %v", v.Name, err)
		}
	}
}

func TestBodyVectors(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Bodies.Vectors {
		got, err := DeriveBody(v.Kind, v.Input, keys)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "encoded", v.Encoded, got)
	}
}

// ---------------------------------------------------------------------------
// Layer 2 — bundles
// ---------------------------------------------------------------------------

func TestBundleVectors(t *testing.T) {
	c, keys := load(t)
	records := map[string]*record.Record{}
	for _, rv := range c.Records.Vectors {
		r, _, err := DeriveRecord(rv.Input, keys)
		if err != nil {
			t.Fatalf("%s: %v", rv.Name, err)
		}
		records[rv.Name] = r
	}

	for _, v := range c.Bundles.Vectors {
		got, err := DeriveBundleBody(v.Input, records)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "body", v.Body, got)

		// And the framing must still parse back.
		b, err := bundle.DecodeBody(v.Body)
		if err != nil {
			t.Errorf("%s: frozen body no longer parses: %v%s", v.Name, err, breakNote)
			continue
		}
		if len(b.Records) != len(v.Input.Records) {
			t.Errorf("%s: decoded %d records, frozen input names %d",
				v.Name, len(b.Records), len(v.Input.Records))
		}
	}
}

// TestBundleDecodeVectors checks compression without freezing a compressor.
//
// The packed blobs were produced once and are never re-emitted: zstd output
// moves with the library and will move again when §7.4's real dictionary is
// trained. What must not move is the ability to READ what was written, and this
// is also what stops dictionary 0's corpus being edited in place — a retrain
// belongs in a new dictionary ID, leaving old peers able to decode.
func TestBundleDecodeVectors(t *testing.T) {
	c, _ := load(t)
	dict, err := bundle.Dictionary0()
	if err != nil {
		t.Fatalf("dictionary 0: %v", err)
	}
	defer dict.Close()
	set, err := bundle.NewDictionarySet(dict)
	if err != nil {
		t.Fatalf("dictionary set: %v", err)
	}
	defer set.Close()

	for _, v := range c.Bundles.DecodeOnly {
		b, err := bundle.Unpack(v.Packed, set)
		if err != nil {
			t.Errorf("%s: frozen packed bundle no longer unpacks: %v%s", v.Name, err, breakNote)
			continue
		}
		body, err := bundle.EncodeBody(b)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "decoded body", v.ExpectBody, body)
	}
}

// ---------------------------------------------------------------------------
// Layer 3 — control plane
// ---------------------------------------------------------------------------

func TestControlVectors(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Control.Vectors {
		got, err := DeriveControl(v.Kind, v.Input, keys)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "encoded", v.Encoded, got)
	}
}

// TestControlVectorsFitOnePacket restates §7.3's rule against the frozen bytes.
//
// The limits are derived from the MTU rather than chosen, so a change to the
// derivation shows up as vectors that no longer fit rather than as fragmentation
// discovered on a radio.
func TestControlVectorsFitOnePacket(t *testing.T) {
	c, _ := load(t)
	for _, v := range c.Control.Vectors {
		if v.Kind == CtrlVVEncode || v.Kind == CtrlVVHash {
			// Not control messages in their own right; they ride inside one.
			continue
		}
		if len(v.Encoded) > gossip.MaxControlMessage {
			t.Errorf("%s: %d bytes, over the %d-byte control message limit (§7.3)",
				v.Name, len(v.Encoded), gossip.MaxControlMessage)
		}
	}
}

// ---------------------------------------------------------------------------
// Layer 3b — fountain codec
// ---------------------------------------------------------------------------

func TestSymbolVectors(t *testing.T) {
	c, _ := load(t)
	for _, v := range c.Fountain.Symbols {
		same(t, v.Name, "encoded", v.Encoded, DeriveSymbol(v.Input))

		got, err := fountain.DecodeSymbol(v.Encoded)
		if err != nil {
			t.Errorf("%s: frozen symbol no longer parses: %v%s", v.Name, err, breakNote)
			continue
		}
		if got.Index != v.Input.Index {
			t.Errorf("%s: decoded index %d, frozen input says %d%s",
				v.Name, got.Index, v.Input.Index, breakNote)
		}
		if got.K != v.Input.K || got.BundleID != v.Input.BundleID {
			t.Errorf("%s: decoded K=%d bundle=%d, frozen input says K=%d bundle=%d%s",
				v.Name, got.K, got.BundleID, v.Input.K, v.Input.BundleID, breakNote)
		}
	}
}

// TestMaskVectors is the one that matters most to a third-party implementation.
//
// The mask never travels on the wire, so an implementation that derives it
// differently produces symbols that decode to garbage with no error anywhere in
// the stack. See fountain.Mask.
func TestMaskVectors(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Fountain.Masks {
		got, degree, err := DeriveMask(v.Input, keys)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "mask", v.Mask, got)
		if degree != v.Degree {
			t.Errorf("%s: degree is %d, frozen vector says %d%s", v.Name, degree, v.Degree, breakNote)
		}
	}
}

func TestRepairVectors(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Fountain.Repairs {
		got, k, err := DeriveRepair(v.Input, keys)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "encoded", v.Encoded, got)
		if k != v.K {
			t.Errorf("%s: K is %d, frozen vector says %d%s", v.Name, k, v.K, breakNote)
		}
	}
}

// ---------------------------------------------------------------------------
// Layer 3c — mesh link
// ---------------------------------------------------------------------------

func TestLinkVectors(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Link.Vectors {
		got, err := DeriveLink(v.Kind, v.Input, keys)
		if err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}
		same(t, v.Name, "encoded", v.Encoded, got)
	}
}

// TestAnnounceVectorsVerify checks the self-certifying property against frozen
// bytes rather than against bytes this build just produced.
//
// An announcement is what lets a node be trusted with no registry and nothing
// known about it beforehand (§7.1.2), and the radio number is inside the
// signature so a captured frame cannot be replayed from somewhere else. Both
// halves are asserted here: the frame verifies when it arrives from the radio it
// claims, and is refused when it does not.
func TestAnnounceVectorsVerify(t *testing.T) {
	c, keys := load(t)
	for _, v := range c.Link.Vectors {
		if v.Kind != LinkAnnounce {
			continue
		}
		var in AnnounceInput
		if err := json.Unmarshal(v.Input, &in); err != nil {
			t.Errorf("%s: %v", v.Name, err)
			continue
		}

		ann, err := meshlink.DecodeAnnounce(v.Encoded, in.Radio)
		if err != nil {
			t.Errorf("%s: frozen announcement no longer verifies: %v%s", v.Name, err, breakNote)
			continue
		}
		want := keys[in.Key].ID()
		if ann.ID != want {
			t.Errorf("%s: announcement resolves to %s, frozen input names %s%s",
				v.Name, ann.ID, want, breakNote)
		}
		if ann.Radio != in.Radio {
			t.Errorf("%s: announcement claims radio 0x%08x, frozen input says 0x%08x",
				v.Name, ann.Radio, in.Radio)
		}

		// Replayed from any other radio, it must be refused.
		if _, err := meshlink.DecodeAnnounce(v.Encoded, in.Radio+1); err == nil {
			t.Errorf("%s: announcement accepted from a radio it does not claim; "+
				"the binding in §7.1.2 is what makes a captured frame unreplayable", v.Name)
		}
	}
}
