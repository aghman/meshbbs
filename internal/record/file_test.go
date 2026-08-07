package record

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func testHash(b byte) [FileHashLen]byte {
	var h [FileHashLen]byte
	for i := range h {
		h[i] = b
	}
	return h
}

func sampleFile() FileBody {
	return FileBody{
		Name:        "ARCHIVE.ZIP",
		Size:        4096,
		Hash:        testHash(0x11),
		Description: "A compressed archive",
		Tags:        []string{"utils", "dos"},
	}
}

func TestFileBodyRoundTrip(t *testing.T) {
	want := sampleFile()
	b, err := MarshalFileBody(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalFileBody(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.Size != want.Size || got.Hash != want.Hash ||
		got.Description != want.Description || len(got.Tags) != len(want.Tags) {
		t.Fatalf("round trip changed the body:\n got %+v\nwant %+v", got, want)
	}
	for i := range want.Tags {
		if got.Tags[i] != want.Tags[i] {
			t.Errorf("tag %d is %q, want %q", i, got.Tags[i], want.Tags[i])
		}
	}
}

// The common case: a file with no description and no tags. Both absences cost
// one byte each, which is what the no-flags-byte layout buys.
func TestFileBodyMinimal(t *testing.T) {
	f := FileBody{Name: "A.TXT", Size: 1, Hash: testHash(0x22)}
	b, err := MarshalFileBody(f)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalFileBody(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "" || len(got.Tags) != 0 {
		t.Errorf("absent fields came back as %+v", got)
	}

	// 1 + 5 (name) + 1 (size) + 16 (hash) + 1 (no desc) + 1 (no tags)
	if len(b) != 25 {
		t.Errorf("minimal body is %d bytes, want 25", len(b))
	}
}

func TestFileBodyLenMatchesTheEncoding(t *testing.T) {
	for _, f := range []FileBody{
		sampleFile(),
		{Name: "A", Size: 0, Hash: testHash(1)},
		{Name: strings.Repeat("n", MaxFileNameLen), Size: 1 << 40, Hash: testHash(2),
			Description: strings.Repeat("d", MaxFileDescLen),
			Tags:        []string{"a", "bb", "ccc"}},
	} {
		b, err := MarshalFileBody(f)
		if err != nil {
			t.Fatalf("%q: %v", f.Name, err)
		}
		if FileBodyLen(f) != len(b) {
			t.Errorf("FileBodyLen said %d, encoding is %d bytes", FileBodyLen(f), len(b))
		}
	}
}

// §6.5 claims a catalog entry is roughly 120-200 bytes. That is an arithmetic
// claim about the airtime budget, so it gets asserted rather than trusted
// (§12.7).
//
// Note what is NOT asserted: that a record fits one 233-byte packet. Records
// are packed into bundles and fountain-coded across symbols (§7.2), which is
// why MaxBodyLen is 8 KiB and an ordinary POST already exceeds a packet. An
// earlier version of this test required it, which is a constraint the design
// does not make and which no useful description can satisfy — 89 bytes of
// framing and a 16-byte hash leave about 128 for text.
func TestFileRecordFitsTheBudget(t *testing.T) {
	typical := FileBody{
		Name:        "MESHBBS-1.0.TAR.GZ",
		Size:        2_400_000,
		Hash:        testHash(0x33),
		Description: "Source release, builds cgo-free on all five targets",
	}
	if n := FileSize(typical); n > 200 {
		t.Errorf("a typical catalog entry is %d bytes, above §6.5's 200", n)
	}

	// The worst case is bounded so the limits above cannot drift upward
	// unnoticed. The ceiling is the current maximum (300) plus a little
	// headroom, not a target to bend the limits toward: raising one past it
	// should be a decision someone makes on purpose, because every byte here is
	// airtime the whole network shares.
	tag := strings.Repeat("t", MaxFileTagLen)
	tags := make([]string, MaxFileTags)
	for i := range tags {
		tags[i] = tag
	}
	worst := FileBody{
		Name:        strings.Repeat("n", MaxFileNameLen),
		Size:        1 << 62,
		Hash:        testHash(0x44),
		Description: strings.Repeat("d", MaxFileDescLen),
		Tags:        tags,
	}
	if n := FileSize(worst); n > 320 {
		t.Errorf("a maximal catalog entry is %d bytes, above the 320-byte ceiling", n)
	}
	t.Logf("catalog entry sizes: typical %d bytes, maximal %d bytes",
		FileSize(typical), FileSize(worst))
}

func TestFileBodyRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		body FileBody
	}{
		{"empty name", FileBody{Size: 1, Hash: testHash(1)}},
		{"name too long", FileBody{Name: strings.Repeat("x", MaxFileNameLen+1), Hash: testHash(1)}},
		{"name with a slash", FileBody{Name: "dir/file", Hash: testHash(1)}},
		{"name with a backslash", FileBody{Name: `dir\file`, Hash: testHash(1)}},
		{"dot", FileBody{Name: ".", Hash: testHash(1)}},
		{"dotdot", FileBody{Name: "..", Hash: testHash(1)}},
		{"control character in name", FileBody{Name: "bad\x01name", Hash: testHash(1)}},
		{"invalid UTF-8 name", FileBody{Name: "bad\xffname", Hash: testHash(1)}},
		{"zero hash", FileBody{Name: "A.TXT"}},
		{"description too long", FileBody{Name: "A.TXT", Hash: testHash(1),
			Description: strings.Repeat("d", MaxFileDescLen+1)}},
		{"control character in description", FileBody{Name: "A.TXT", Hash: testHash(1),
			Description: "line\nbreak"}},
		{"too many tags", FileBody{Name: "A.TXT", Hash: testHash(1),
			Tags: []string{"a", "b", "c", "d", "e"}}},
		{"description at the old, looser limit", FileBody{Name: "A.TXT", Hash: testHash(1),
			Description: strings.Repeat("d", 120)}},
		{"empty tag", FileBody{Name: "A.TXT", Hash: testHash(1), Tags: []string{""}}},
		{"tag too long", FileBody{Name: "A.TXT", Hash: testHash(1),
			Tags: []string{strings.Repeat("t", MaxFileTagLen+1)}}},
	}
	for _, c := range cases {
		if _, err := MarshalFileBody(c.body); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// A hostile body must be rejected by the same rules that governed writing it.
// Marshal cannot produce any of these, so the decoder has to enforce them
// itself rather than assuming its input came from us.
func TestUnmarshalRejectsHostileBodies(t *testing.T) {
	valid, err := MarshalFileBody(sampleFile())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("truncated at every length", func(t *testing.T) {
		for i := 0; i < len(valid); i++ {
			if _, err := UnmarshalFileBody(valid[:i]); err == nil {
				t.Errorf("a body truncated to %d bytes was accepted", i)
			}
		}
	})

	t.Run("trailing bytes", func(t *testing.T) {
		// Two encodings of one logical file would be two content addresses,
		// because the ID derives from bytes rather than fields.
		if _, err := UnmarshalFileBody(append(append([]byte{}, valid...), 0x00)); err == nil {
			t.Error("a body with a trailing byte was accepted")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if _, err := UnmarshalFileBody(nil); !errors.Is(err, ErrTruncated) {
			t.Errorf("empty input gave %v, want ErrTruncated", err)
		}
	})

	t.Run("name claims more than is there", func(t *testing.T) {
		b := append([]byte{}, valid...)
		b[0] = 0xFF
		if _, err := UnmarshalFileBody(b); err == nil {
			t.Error("a body whose name length overruns was accepted")
		}
	})

	t.Run("tag count over the limit", func(t *testing.T) {
		f := sampleFile()
		f.Tags = nil
		b, err := MarshalFileBody(f)
		if err != nil {
			t.Fatal(err)
		}
		b[len(b)-1] = MaxFileTags + 1
		if _, err := UnmarshalFileBody(b); err == nil {
			t.Error("a body claiming more tags than the limit was accepted")
		}
	})

	t.Run("zero hash on the wire", func(t *testing.T) {
		f := sampleFile()
		f.Tags = nil
		f.Description = ""
		b, err := MarshalFileBody(f)
		if err != nil {
			t.Fatal(err)
		}
		// Zero the hash in place. The size is a uvarint, so its width has to be
		// computed rather than assumed — assuming one byte zeroed the wrong
		// range, left the last hash byte intact, and the decoder correctly
		// accepted a hash that was not all zero.
		start := 1 + len(f.Name) + uvarintLen(f.Size)
		for i := start; i < start+FileHashLen; i++ {
			b[i] = 0
		}
		if _, err := UnmarshalFileBody(b); err == nil {
			t.Error("a body with an all-zero hash was accepted")
		}
	})
}

// The encoding is deterministic: the same body must produce the same bytes
// every time, or the derived record ID is not a content address (§6.2.1 rule 2).
func TestFileBodyEncodingIsDeterministic(t *testing.T) {
	f := sampleFile()
	first, err := MarshalFileBody(f)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := MarshalFileBody(f)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("encoding varied between runs:\n%x\n%x", first, again)
		}
	}
}

func TestNewAndVerifyFileRecord(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(9))
	if err != nil {
		t.Fatal(err)
	}
	area := AreaTagFor("utils")
	want := sampleFile()

	rec, err := NewFileRecord(key, 1, 1_700_000_000, area, want)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Type != TypeFile {
		t.Errorf("record type is %s", rec.Type)
	}
	if rec.Area != area {
		t.Error("the record did not land in its file area")
	}
	if rec.Origin != key.ID() {
		t.Error("the record's origin is not the signing node")
	}

	got, err := VerifyFileRecord(rec, key.Public)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != want.Name || got.Hash != want.Hash {
		t.Errorf("verified body is %+v", got)
	}

	// The origin IS the holding node. That is why the body has no such field.
	if rec.Origin != key.ID() {
		t.Error("held-by cannot be read from the origin")
	}
}

func TestVerifyFileRecordRejectsWrongType(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(10))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := New(key, Record{
		Origin: key.ID(), Seq: 1, TS: 1, Type: TypePost,
		Area: AreaTagFor("general"), Body: []byte("not a file"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFileRecord(rec, key.Public); err == nil {
		t.Error("a POST verified as a FILE")
	}
}

func TestVerifyFileRecordRejectsAForeignKey(t *testing.T) {
	mine, err := identity.GenerateNodeKey(rng.TestSecret(11))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := identity.GenerateNodeKey(rng.TestSecret(12))
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewFileRecord(mine, 1, 1, AreaTagFor("utils"), sampleFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyFileRecord(rec, theirs.Public); err == nil {
		t.Error("a FILE verified against a key that is not its origin's")
	}
}

func TestTruncateFileHash(t *testing.T) {
	full := make([]byte, 32)
	for i := range full {
		full[i] = byte(i)
	}
	h, err := TruncateFileHash(full)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < FileHashLen; i++ {
		if h[i] != byte(i) {
			t.Fatalf("truncation took the wrong bytes: %x", h)
		}
	}

	if _, err := TruncateFileHash(full[:8]); err == nil {
		t.Error("a hash too short to truncate was accepted")
	}
}

// Sizes must survive the uvarint round trip at the boundaries, since a file
// size crossing a varint width is exactly where an encoder bug hides.
func TestFileSizeRoundTrips(t *testing.T) {
	for _, size := range []uint64{
		0, 1, 127, 128, 16383, 16384, 1 << 20, 1<<32 - 1, 1 << 32, 1 << 62,
	} {
		f := FileBody{Name: "A.BIN", Size: size, Hash: testHash(0x55)}
		b, err := MarshalFileBody(f)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnmarshalFileBody(b)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if got.Size != size {
			t.Errorf("size %d came back as %d", size, got.Size)
		}
	}
}

// FuzzUnmarshalFileBody — §12.5.
//
// A FILE body arrives from anyone holding the channel PSK, and what it carries
// is rendered into a terminal and turned into a filename. The round-trip
// assertion is the important one: the record ID derives from these bytes, so
// two encodings of one logical file would be two content addresses for the same
// thing, and anti-entropy would carry both forever.
func FuzzUnmarshalFileBody(f *testing.F) {
	good, _ := MarshalFileBody(sampleFile())
	f.Add(good)
	minimal, _ := MarshalFileBody(FileBody{Name: "A", Size: 0, Hash: testHash(1)})
	f.Add(minimal)
	f.Add([]byte{0})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		body, err := UnmarshalFileBody(data)
		if err != nil {
			return
		}
		again, err := MarshalFileBody(body)
		if err != nil {
			t.Fatalf("a parsed FILE body failed to re-encode: %v", err)
		}
		if string(again) != string(data) {
			t.Fatalf("FILE encoding is not canonical:\n input % x\nre-enc % x", data, again)
		}

		// Anything that survives parsing gets drawn on a screen and used as a
		// filename, so neither may carry what a terminal or a path would act on.
		for _, s := range append([]string{body.Name, body.Description}, body.Tags...) {
			for _, r := range s {
				if r < 0x20 || r == 0x7f {
					t.Fatalf("a control character survived into %q", s)
				}
			}
		}
		for _, r := range body.Name {
			if r == '/' || r == '\\' {
				t.Fatalf("a path separator survived into a file name: %q", body.Name)
			}
		}
		if body.Name == "." || body.Name == ".." {
			t.Fatalf("%q survived as a file name", body.Name)
		}
	})
}
