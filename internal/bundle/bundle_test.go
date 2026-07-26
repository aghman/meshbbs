package bundle

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

func testKey(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func dicts(t *testing.T) *DictionarySet {
	t.Helper()
	s, err := NewDictionarySet()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func makeBundle(t *testing.T, k identity.NodeKey, area string, n int, baseTS uint32) *Bundle {
	t.Helper()
	b := &Bundle{Area: record.AreaTagFor(area), BaseTS: baseTS}
	for i := 0; i < n; i++ {
		body, err := marshalPost(fmt.Sprintf("user%d", i%3), "subject", strings.Repeat("body text ", 5))
		if err != nil {
			t.Fatal(err)
		}
		r, err := record.New(k, record.Record{
			Seq: uint64(i + 1), TS: baseTS + uint32(i), Type: record.TypePost,
			Area: b.Area, Body: body,
		})
		if err != nil {
			t.Fatal(err)
		}
		b.Records = append(b.Records, r)
	}
	return b
}

// marshalPost mirrors the store's post body without importing it, keeping this
// package's tests free of a dependency cycle.
func marshalPost(author, subject, text string) ([]byte, error) {
	out := []byte{byte(len(author))}
	out = append(out, author...)
	out = append(out, byte(len(subject)))
	out = append(out, subject...)
	return append(out, text...), nil
}

func TestPackUnpackRoundTrip(t *testing.T) {
	k := testKey(t, 1)
	ds := dicts(t)
	d, _ := ds.Get(0)

	for _, n := range []int{1, 2, 5, 20, 64} {
		orig := makeBundle(t, k, "general", n, 1_700_000_000)
		packed, err := Pack(orig, d)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		got, err := Unpack(packed, ds)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}

		if got.Area != orig.Area || got.BaseTS != orig.BaseTS {
			t.Fatalf("n=%d: header mismatch", n)
		}
		if len(got.Records) != n {
			t.Fatalf("n=%d: got %d records", n, len(got.Records))
		}
		for i := range got.Records {
			if got.Records[i].ID() != orig.Records[i].ID() {
				t.Fatalf("n=%d: record %d ID changed", n, i)
			}
			// §6.2.1 rule 1: verification must still work off the bytes that
			// were signed, so the bundle has to carry them verbatim.
			if err := got.Records[i].Verify(k.Public); err != nil {
				t.Fatalf("n=%d: record %d failed to verify after bundling: %v", n, i, err)
			}
		}
	}
}

// §7.4: compressing the BUNDLE rather than each record is what lets zstd find
// redundancy between records. That difference is the whole argument.
func TestBundleCompressionBeatsPerRecord(t *testing.T) {
	k := testKey(t, 2)
	ds := dicts(t)
	d, _ := ds.Get(0)

	b := makeBundle(t, k, "general", 10, 1_700_000_000)

	packed, err := Pack(b, d)
	if err != nil {
		t.Fatal(err)
	}

	// Compress each record on its own for comparison.
	perRecord := 0
	for _, r := range b.Records {
		c, err := d.Compress(r.Marshal())
		if err != nil {
			t.Fatal(err)
		}
		perRecord += len(c)
	}

	t.Logf("10 records: bundled %d bytes, separately %d bytes (%.1f%% saved)",
		len(packed), perRecord, 100*(1-float64(len(packed))/float64(perRecord)))

	if len(packed) >= perRecord {
		t.Errorf("bundling saved nothing: %d bytes bundled vs %d separately", len(packed), perRecord)
	}
}

// §6.1.3: the origin ID is paid once per bundle, not once per record. That is
// what makes 8-byte self-certifying IDs cheaper on the wire than the 4-byte
// numeric addresses they replaced, at bundle sizes of three or more.
func TestOriginTableIsPaidOncePerBundle(t *testing.T) {
	k := testKey(t, 3)
	ds := dicts(t)
	d, _ := ds.Get(0)

	// Uncompressed body sizes isolate the framing from zstd's behaviour.
	one := makeBundle(t, k, "general", 1, 1000)
	ten := makeBundle(t, k, "general", 10, 1000)

	bodyOne, err := encodeBody(one)
	if err != nil {
		t.Fatal(err)
	}
	bodyTen, err := encodeBody(ten)
	if err != nil {
		t.Fatal(err)
	}

	// Per-record marginal cost should not include another 8-byte origin.
	marginal := float64(len(bodyTen)-len(bodyOne)) / 9
	perRecordRaw := 0
	for _, r := range ten.Records {
		perRecordRaw += len(r.Marshal())
	}
	avgRecord := float64(perRecordRaw) / 10

	// The framing overhead per additional record is the origin index, the
	// timestamp delta and a length prefix — a handful of bytes, not eight.
	framing := marginal - avgRecord
	t.Logf("marginal cost per extra record: %.1f bytes, of which %.1f is framing", marginal, framing)
	if framing > 6 {
		t.Errorf("per-record framing is %.1f bytes; the origin should be hoisted, not repeated", framing)
	}

	_ = d
}

// §7.4's decompression-bomb defence. A 233-byte packet can expand to gigabytes,
// and §8.1 says to assume everyone on the channel is hostile.
func TestDecompressionBombIsRefused(t *testing.T) {
	ds := dicts(t)
	d, _ := ds.Get(0)

	// A highly compressible payload far larger than the cap.
	huge := make([]byte, MaxDecompressed*4)
	compressed, err := d.Compress(huge)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d bytes of zeros compress to %d bytes (%.0fx)",
		len(huge), len(compressed), float64(len(huge))/float64(len(compressed)))

	// Frame it as a bundle declaring its true, over-limit size.
	packet := []byte{FormatVersion, 0}
	packet = appendUvarint(packet, uint64(len(huge)))
	packet = append(packet, compressed...)

	if _, err := Unpack(packet, ds); err == nil {
		t.Fatal("accepted a bundle declaring a decompressed size over the limit")
	}

	// And a packet that LIES about its size must also fail, rather than being
	// allowed to expand past the cap during decoding.
	lying := []byte{FormatVersion, 0}
	lying = appendUvarint(lying, 100)
	lying = append(lying, compressed...)
	if _, err := Unpack(lying, ds); err == nil {
		t.Fatal("accepted a bundle whose payload expanded far beyond its declared size")
	}
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// The hoisted fields are redundant with what each record carries. The record is
// the authority — its bytes are what the signature covers — so a mismatch means
// the framing is lying and the batch is suspect.
func TestFramingMustAgreeWithRecords(t *testing.T) {
	k := testKey(t, 4)
	ds := dicts(t)
	d, _ := ds.Get(0)

	b := makeBundle(t, k, "general", 3, 1_700_000_000)
	body, err := encodeBody(b)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the hoisted origin table so it disagrees with the records.
	corrupted := append([]byte(nil), body...)
	originOff := 4 + 4 + 1 // area + baseTS + originCount
	corrupted[originOff] ^= 0xff

	if _, err := decodeBody(corrupted); err == nil {
		t.Fatal("accepted a bundle whose origin table disagreed with its records")
	}

	// Corrupt the base timestamp, which the per-record deltas are relative to.
	corrupted = append([]byte(nil), body...)
	corrupted[4] ^= 0xff
	if _, err := decodeBody(corrupted); err == nil {
		t.Fatal("accepted a bundle whose base timestamp disagreed with its records")
	}

	_ = d
}

func TestUnpackRejectsGarbage(t *testing.T) {
	ds := dicts(t)
	for _, bad := range [][]byte{
		nil, {1}, {1, 0},
		{FormatVersion + 1, 0, 1, 0x00}, // future format version
		{FormatVersion, 200, 1, 0x00},   // unknown dictionary
	} {
		if _, err := Unpack(bad, ds); err == nil {
			t.Errorf("accepted malformed bundle % x", bad)
		}
	}
}

func TestEmptyBundleRejected(t *testing.T) {
	ds := dicts(t)
	d, _ := ds.Get(0)
	if _, err := Pack(&Bundle{}, d); err == nil {
		t.Fatal("packed an empty bundle")
	}
}

func TestUnknownDictionaryIsNamed(t *testing.T) {
	ds := dicts(t)
	_, err := ds.Get(42)
	if err == nil {
		t.Fatal("expected an error for an unknown dictionary")
	}
	// A sysop reading this should understand it is a version skew, not corruption.
	if !strings.Contains(err.Error(), "does not hold") {
		t.Errorf("error does not explain the situation: %v", err)
	}
}

// Old dictionaries stay supported when new ones ship (§7.4), so a peer that has
// not upgraded still gets read.
func TestDictionarySetKeepsOldDictionaries(t *testing.T) {
	trained, err := NewRawDictionary(1, bytes.Repeat([]byte("the quick brown fox "), 100))
	if err != nil {
		t.Fatal(err)
	}
	ds, err := NewDictionarySet(trained)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	// Both the trained dictionary and plain zstd must be available.
	for _, id := range []uint8{0, 1} {
		if _, err := ds.Get(id); err != nil {
			t.Errorf("dictionary %d unavailable: %v", id, err)
		}
	}
	ids := ds.IDs()
	if len(ids) != 2 || ids[0] != 0 || ids[1] != 1 {
		t.Errorf("IDs() returned %v, want sorted [0 1]", ids)
	}
}

// A bundle packed with a trained dictionary must round-trip through a set that
// holds it — this is the cross-version path that matters when a dictionary
// ships.
func TestTrainedDictionaryRoundTrip(t *testing.T) {
	k := testKey(t, 5)
	corpus := bytes.Repeat([]byte("> quoted reply\nRe: antenna\n73 de "), 200)

	trained, err := NewRawDictionary(1, corpus)
	if err != nil {
		t.Fatal(err)
	}
	ds, err := NewDictionarySet(trained)
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	b := makeBundle(t, k, "general", 5, 1000)
	packed, err := Pack(b, trained)
	if err != nil {
		t.Fatal(err)
	}
	if packed[1] != 1 {
		t.Fatalf("bundle header records dictionary %d, want 1", packed[1])
	}
	got, err := Unpack(packed, ds)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 5 {
		t.Fatalf("got %d records", len(got.Records))
	}
}

func FuzzUnpack(f *testing.F) {
	ds, err := NewDictionarySet()
	if err != nil {
		f.Fatal(err)
	}
	d, _ := ds.Get(0)
	k, err := identity.GenerateNodeKey(rng.TestSecret(9))
	if err != nil {
		f.Fatal(err)
	}
	b := &Bundle{Area: record.AreaTagFor("general"), BaseTS: 1000}
	r, err := record.New(k, record.Record{Seq: 1, TS: 1000, Type: record.TypePost,
		Area: b.Area, Body: []byte("seed")})
	if err != nil {
		f.Fatal(err)
	}
	b.Records = append(b.Records, r)
	if packed, err := Pack(b, d); err == nil {
		f.Add(packed)
	}
	f.Add([]byte{})
	f.Add([]byte{FormatVersion, 0, 0})

	// §12.5: bundles arrive from anyone holding the channel PSK.
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Unpack(data, ds)
		if err != nil {
			return
		}
		if len(got.Records) == 0 || len(got.Records) > MaxRecords {
			t.Fatalf("accepted a bundle with %d records", len(got.Records))
		}
	})
}

// FuzzDecodeBody fuzzes the bundle body parser DIRECTLY, without compression.
//
// FuzzUnpack cannot reach this code in any useful way: to get past Unpack, the
// fuzzer must first synthesize a valid zstd frame, so essentially every input
// dies at decompression and the body parser — the part that actually walks
// attacker-controlled structure — goes unexercised. Fuzzing the layer where the
// parsing lives is the only way to test it.
//
// The assertion is canonicality, not merely "does not panic": a body that
// parses must re-encode from its parsed fields to exactly the input. Anything
// else means one logical bundle has several wire forms, which now matters
// directly — bundle IDs are derived from the packed bytes (§7.2), so a second
// wire form is a second bundle ID for identical content, and the resumable
// transmission that depends on stable IDs silently stops resuming.
func FuzzDecodeBody(f *testing.F) {
	k, err := identity.GenerateNodeKey(rng.TestSecret(11))
	if err != nil {
		f.Fatal(err)
	}
	area := record.AreaTagFor("general")

	seed := &Bundle{Area: area, BaseTS: 1000}
	for i := 1; i <= 3; i++ {
		r, err := record.New(k, record.Record{Seq: uint64(i), TS: uint32(1000 + i),
			Type: record.TypePost, Area: area, Body: []byte("seed")})
		if err != nil {
			f.Fatal(err)
		}
		seed.Records = append(seed.Records, r)
	}
	if body, err := encodeBody(seed); err == nil {
		f.Add(body)
	}
	if body, err := encodeBody(&Bundle{Area: area, BaseTS: 0, Records: seed.Records[:1]}); err == nil {
		f.Add(body)
	}
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := decodeBody(data)
		if err != nil {
			return
		}
		if len(got.Records) == 0 || len(got.Records) > MaxRecords {
			t.Fatalf("accepted a bundle with %d records", len(got.Records))
		}
		reencoded, err := encodeBody(got)
		if err != nil {
			t.Fatalf("a parsed body failed to re-encode: %v", err)
		}
		if !bytes.Equal(reencoded, data) {
			t.Fatalf("bundle body encoding is not canonical:\n input % x\nre-enc % x", data, reencoded)
		}
	})
}

// One logical bundle must have exactly one wire form.
//
// All three cases below were ACCEPTED until FuzzDecodeBody was written. They
// were invisible because the only bundle fuzz target went through Unpack, which
// requires a valid zstd frame before the body parser is reached — so the parser
// that actually walks attacker-controlled structure was never exercised.
//
// This matters more than it would have a week ago: §7.2 now derives the bundle
// ID from the packed bytes, so a second wire form is a second bundle ID for
// identical content, and the resumable transmission that depends on stable IDs
// stops resuming with no visible error.
func TestBundleBodyEncodingIsCanonical(t *testing.T) {
	k, err := identity.GenerateNodeKey(rng.TestSecret(11))
	if err != nil {
		t.Fatal(err)
	}
	area := record.AreaTagFor("general")
	b := &Bundle{Area: area, BaseTS: 1000}
	r, err := record.New(k, record.Record{Seq: 1, TS: 1000, Type: record.TypePost,
		Area: area, Body: []byte("x")})
	if err != nil {
		t.Fatal(err)
	}
	b.Records = append(b.Records, r)
	body, err := encodeBody(b)
	if err != nil {
		t.Fatal(err)
	}

	// area(4) + baseTS(4) + originCount(1) + one origin(8)
	const countOffset = 4 + 4 + 1 + identity.NodeIDLen
	cnt, n := binary.Uvarint(body[countOffset:])

	t.Run("overlong record count", func(t *testing.T) {
		bad := append([]byte{}, body[:countOffset]...)
		bad = append(bad, byte(cnt)|0x80, 0x00) // same value, longer encoding
		bad = append(bad, body[countOffset+n:]...)
		if _, err := decodeBody(bad); err == nil {
			t.Error("accepted an overlong record count; one bundle now has two wire forms")
		}
	})

	t.Run("duplicate origin", func(t *testing.T) {
		bad := append([]byte{}, body[:8]...)
		bad = append(bad, 2)
		bad = append(bad, body[9:9+identity.NodeIDLen]...)
		bad = append(bad, body[9:9+identity.NodeIDLen]...)
		bad = append(bad, body[9+identity.NodeIDLen:]...)
		if _, err := decodeBody(bad); err == nil {
			t.Error("accepted a repeated origin; records could reference either index for one node")
		}
	})

	t.Run("unused origin", func(t *testing.T) {
		var ghost identity.NodeID
		ghost[0] = 0xFF
		bad := append([]byte{}, body[:8]...)
		bad = append(bad, 2)
		bad = append(bad, body[9:9+identity.NodeIDLen]...)
		bad = append(bad, ghost[:]...)
		bad = append(bad, body[9+identity.NodeIDLen:]...)
		if _, err := decodeBody(bad); err == nil {
			t.Error("accepted an origin table entry no record references")
		}
	})

	// The honest baseline: the real thing still round-trips.
	got, err := decodeBody(body)
	if err != nil {
		t.Fatalf("a well-formed body was rejected: %v", err)
	}
	again, err := encodeBody(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, body) {
		t.Error("a well-formed body did not survive the round trip")
	}
}
