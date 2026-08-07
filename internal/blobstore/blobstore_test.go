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

// The dedup §6.5 asks for: the same bytes uploaded to two areas are one blob.
func TestIdenticalContentDedups(t *testing.T) {
	s := testStore(t)
	first := put(t, s, "identical bytes")
	second := put(t, s, "identical bytes")
	if first != second {
		t.Fatalf("same content produced two hashes: %s and %s", first, second)
	}

	var files int
	err := filepath.WalkDir(s.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip the temp directory: it holds no blobs, only work in progress.
		if !d.IsDir() && !strings.Contains(path, string(filepath.Separator)+"tmp"+string(filepath.Separator)) {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Errorf("two uploads of identical content left %d files on disk, want 1", files)
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

	var blobs int
	_ = filepath.WalkDir(s.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.Contains(path, string(filepath.Separator)+"tmp"+string(filepath.Separator)) {
			blobs++
		}
		return nil
	})
	if blobs != 0 {
		t.Errorf("a failed upload left %d blob(s) on disk", blobs)
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
