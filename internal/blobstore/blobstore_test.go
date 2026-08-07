package blobstore

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func put(t *testing.T, s *Store, content string) Hash {
	t.Helper()
	h, size, err := s.Put(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Fatalf("Put reported %d bytes for %d bytes of content", size, len(content))
	}
	return h
}

func read(t *testing.T, s *Store, h Hash) string {
	t.Helper()
	f, err := s.Open(h)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRoundTrip(t *testing.T) {
	s := testStore(t)
	h := put(t, s, "SYSOP.TXT\r\nWelcome to the board.\r\n")
	if got := read(t, s, h); got != "SYSOP.TXT\r\nWelcome to the board.\r\n" {
		t.Errorf("read back %q", got)
	}
	if !s.Has(h) {
		t.Error("Has says a blob we just wrote is not held")
	}
}

// countBlobs counts the files under the store root, ignoring staging.
//
// The staging check is RELATIVE to the root, not a substring of the absolute
// path. Matching "/tmp/" anywhere in the path made every blob invisible on
// Linux, where t.TempDir() itself lives under /tmp — so the dedup assertion
// below passed on macOS and reported zero files on CI.
func countBlobs(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	err := filepath.WalkDir(s.Root(), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.Root(), p)
		if err != nil {
			return err
		}
		if strings.SplitN(rel, string(filepath.Separator), 2)[0] == "tmp" {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The dedup §6.5 asks for: the same bytes uploaded to two areas are one blob.
func TestIdenticalContentDedups(t *testing.T) {
	s := testStore(t)
	first := put(t, s, "identical bytes")
	second := put(t, s, "identical bytes")
	if first != second {
		t.Fatalf("same content produced two hashes: %s and %s", first, second)
	}

	if n := countBlobs(t, s); n != 1 {
		t.Errorf("two uploads of identical content left %d files on disk, want 1", n)
	}
}

// The counter itself has to be able to see a blob, or every assertion built on
// it passes vacuously — which is how the bug above survived local runs.
func TestCountBlobsSeesABlob(t *testing.T) {
	s := testStore(t)
	if n := countBlobs(t, s); n != 0 {
		t.Fatalf("a fresh store already has %d blobs", n)
	}
	put(t, s, "one blob")
	if n := countBlobs(t, s); n != 1 {
		t.Fatalf("countBlobs found %d files after one upload, want 1", n)
	}
}

func TestDifferentContentDiffers(t *testing.T) {
	s := testStore(t)
	if put(t, s, "one") == put(t, s, "two") {
		t.Error("different content produced the same hash")
	}
}

// A blob nobody holds is an ordinary answer, not a failure — the network
// catalog is full of files held elsewhere (§6.5).
func TestMissingBlobIsNotFound(t *testing.T) {
	s := testStore(t)
	var absent Hash
	absent[0] = 0xAB

	if _, err := s.Open(absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open of an absent blob returned %v, want ErrNotFound", err)
	}
	if _, err := s.Size(absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Size of an absent blob returned %v, want ErrNotFound", err)
	}
	if s.Has(absent) {
		t.Error("Has claims we hold a blob we never wrote")
	}
}

// The fan-out is part of the on-disk contract: <root>/ab/cd/<full hex>.
func TestLayoutFansOut(t *testing.T) {
	s := testStore(t)
	h := put(t, s, "layout")
	hexed := h.String()

	want := filepath.Join(s.Root(), hexed[0:2], hexed[2:4], hexed)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("blob is not at %s: %v", want, err)
	}
}

// Nothing may be left under a name that asserts contents it does not have, so a
// read that fails partway must not publish a blob.
func TestFailedWriteLeavesNoBlob(t *testing.T) {
	s := testStore(t)
	boom := errors.New("disk went away")
	r := io.MultiReader(strings.NewReader("partial"), &failingReader{err: boom})

	if _, _, err := s.Put(r); !errors.Is(err, boom) {
		t.Fatalf("Put returned %v, want the reader's error", err)
	}

	if n := countBlobs(t, s); n != 0 {
		t.Errorf("a failed upload left %d blob(s) on disk", n)
	}
}

func TestVerifyDetectsCorruption(t *testing.T) {
	s := testStore(t)
	h := put(t, s, "the bytes this hash names")
	if err := s.Verify(h); err != nil {
		t.Fatalf("a freshly written blob failed verification: %v", err)
	}

	hexed := h.String()
	p := filepath.Join(s.Root(), hexed[0:2], hexed[2:4], hexed)
	if err := os.WriteFile(p, []byte("different bytes entirely"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(h); err == nil {
		t.Error("Verify accepted a blob whose bytes had been replaced")
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	s := testStore(t)
	h := put(t, s, "temporary")
	if err := s.Remove(h); err != nil {
		t.Fatal(err)
	}
	if s.Has(h) {
		t.Error("blob survived Remove")
	}
	// Retrying a half-finished delete must not fail.
	if err := s.Remove(h); err != nil {
		t.Errorf("removing an absent blob returned %v, want nil", err)
	}
}

func TestTempAndAdopt(t *testing.T) {
	s := testStore(t)
	f, err := s.Temp()
	if err != nil {
		t.Fatal(err)
	}
	// Written out of order, the way an SFTP client is allowed to.
	if _, err := f.WriteAt([]byte("WORLD"), 6); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("HELLO "), 0); err != nil {
		t.Fatal(err)
	}

	h, size, err := s.Adopt(f)
	if err != nil {
		t.Fatal(err)
	}
	if size != 11 {
		t.Errorf("Adopt reported %d bytes, want 11", size)
	}
	if got := read(t, s, h); got != "HELLO WORLD" {
		t.Errorf("adopted blob reads back as %q", got)
	}
	if countBlobs(t, s) != 1 {
		t.Error("Adopt did not leave exactly one blob")
	}
	// The staging file is gone, not left to accumulate.
	if _, err := os.Stat(f.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the staging file survived Adopt: %v", err)
	}
}

// Adopt leaves no open handle behind.
//
// This is a NECESSARY condition for the Windows correctness Adopt documents,
// not a sufficient one: it passes whether the close happens before the rename
// or after it, because a deferred close still runs before Adopt returns. Only
// the Windows runner can tell those two apart, and it did. Kept because
// leaking a handle per upload is worth catching on its own.
func TestAdoptLeavesNoOpenHandle(t *testing.T) {
	s := testStore(t)
	f, err := s.Temp()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("content"), 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Adopt(f); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err == nil {
		t.Error("Adopt returned with the staging file still open")
	}
}

func TestAdoptDedups(t *testing.T) {
	s := testStore(t)
	var first Hash
	for i := 0; i < 2; i++ {
		f, err := s.Temp()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt([]byte("same bytes"), 0); err != nil {
			t.Fatal(err)
		}
		h, _, err := s.Adopt(f)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = h
		} else if h != first {
			t.Fatalf("identical content adopted to two hashes: %s and %s", first, h)
		}
	}
	if n := countBlobs(t, s); n != 1 {
		t.Errorf("two adoptions of identical content left %d blobs, want 1", n)
	}
}

// Put and Adopt must agree: the same bytes get the same address whichever way
// they arrived.
func TestPutAndAdoptAgree(t *testing.T) {
	s := testStore(t)
	viaPut := put(t, s, "one set of bytes")

	f, err := s.Temp()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("one set of bytes"), 0); err != nil {
		t.Fatal(err)
	}
	viaAdopt, _, err := s.Adopt(f)
	if err != nil {
		t.Fatal(err)
	}
	if viaPut != viaAdopt {
		t.Errorf("Put gave %s and Adopt gave %s for the same content", viaPut, viaAdopt)
	}
}

func TestParseHash(t *testing.T) {
	s := testStore(t)
	h := put(t, s, "parse me")

	got, err := ParseHash(h.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Errorf("round trip changed the hash: %s -> %s", h, got)
	}
	if _, err := ParseHash("not hex"); err == nil {
		t.Error("ParseHash accepted a non-hex string")
	}
	if _, err := ParseHash("abcd"); err == nil {
		t.Error("ParseHash accepted a hash of the wrong width")
	}
}

// Large content must not be buffered in memory to be addressed.
func TestPutStreams(t *testing.T) {
	s := testStore(t)
	content := bytes.Repeat([]byte("meshbbs"), 200_000) // ~1.4 MB
	h, size, err := s.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(content)) {
		t.Errorf("size %d, want %d", size, len(content))
	}
	if err := s.Verify(h); err != nil {
		t.Error(err)
	}
}

type failingReader struct{ err error }

func (f *failingReader) Read([]byte) (int, error) { return 0, f.err }
