package sneakernet

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/vv"
)

func testStore(t *testing.T) *blobstore.Store {
	t.Helper()
	bs, err := blobstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return bs
}

// putBlob writes content into a store and returns its reference.
func putBlob(t *testing.T, bs *blobstore.Store, content []byte) BlobRef {
	t.Helper()
	h, n, err := bs.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return BlobRef{Hash: h, Size: uint64(n)}
}

func opener(bs *blobstore.Store) func(blobstore.Hash) (io.ReadCloser, error) {
	return func(h blobstore.Hash) (io.ReadCloser, error) { return bs.Open(h) }
}

// The whole point: file bytes cross on a stick, which is the one path §7.5
// permits them on — and they arrive in the receiver's blob store verified
// against the hash the manifest declared.
func TestFilesCrossOnAStick(t *testing.T) {
	sender, receiver := testStore(t), testStore(t)

	small := []byte("a readme nobody will read")
	big := bytes.Repeat([]byte{0x5A}, 3<<20) // 3 MiB: past any in-memory bundle
	refs := []BlobRef{putBlob(t, sender, small), putBlob(t, sender, big)}

	c := testCarrier(t)
	c.Blobs = refs

	var stick bytes.Buffer
	if err := Write(&stick, c, opener(sender)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := Read(&stick, receiver)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Blobs) != 2 {
		t.Fatalf("carrier declared %d files", len(got.Blobs))
	}

	for i, ref := range refs {
		if !receiver.Has(ref.Hash) {
			t.Fatalf("file %d did not arrive", i)
		}
		f, err := receiver.Open(ref.Hash)
		if err != nil {
			t.Fatal(err)
		}
		arrived, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := small
		if i == 1 {
			want = big
		}
		if !bytes.Equal(arrived, want) {
			t.Errorf("file %d arrived with %d bytes, want %d", i, len(arrived), len(want))
		}
	}

	// And the records came too — a carrier is one exchange, not two.
	if len(got.Bundles) != len(c.Bundles) {
		t.Errorf("got %d bundles, want %d", len(got.Bundles), len(c.Bundles))
	}
}

// Content addressing is what makes an unsigned container safe to take bytes
// from. A carrier that substitutes different content produces a different hash,
// and the receiver must refuse rather than store it under the claimed name.
func TestASubstitutedFileIsRefused(t *testing.T) {
	sender, receiver := testStore(t), testStore(t)

	honest := putBlob(t, sender, []byte("the real thing"))
	c := testCarrier(t)
	c.Blobs = []BlobRef{honest}

	var stick bytes.Buffer
	if err := Write(&stick, c, opener(sender)); err != nil {
		t.Fatal(err)
	}

	// Swap the body for something else of the same length, as a tampered stick
	// would carry.
	tampered := stick.Bytes()
	forged := []byte("the FAKE thing")
	if len(forged) != len("the real thing") {
		t.Fatal("the test's forgery must be the same length")
	}
	copy(tampered[len(tampered)-len(forged):], forged)

	_, err := Read(bytes.NewReader(tampered), receiver)
	if !errors.Is(err, ErrBlobMismatch) {
		t.Fatalf("got %v, want ErrBlobMismatch", err)
	}
}

// A stick that ends mid-file must not leave a truncated blob masquerading as
// the whole one.
func TestATruncatedStickIsRefused(t *testing.T) {
	sender, receiver := testStore(t), testStore(t)
	ref := putBlob(t, sender, bytes.Repeat([]byte{7}, 4096))

	c := testCarrier(t)
	c.Blobs = []BlobRef{ref}
	var stick bytes.Buffer
	if err := Write(&stick, c, opener(sender)); err != nil {
		t.Fatal(err)
	}

	cut := stick.Bytes()[:stick.Len()-1000]
	_, err := Read(bytes.NewReader(cut), receiver)
	if !errors.Is(err, ErrShortBlob) {
		t.Fatalf("got %v, want ErrShortBlob", err)
	}
	// Whatever landed is addressed by its own content, so it cannot be mistaken
	// for the file that was promised.
	if receiver.Has(ref.Hash) {
		t.Error("a truncated file was stored under the full file's hash")
	}
}

// The request-only leg: vectors and nothing else, which needs no blob store.
func TestARequestOnlyCarrierNeedsNoStore(t *testing.T) {
	c := testCarrier(t)
	c.Blobs = nil

	var stick bytes.Buffer
	if err := Write(&stick, c, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := Read(bytes.NewReader(stick.Bytes()), nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Vectors) != len(c.Vectors) {
		t.Errorf("got %d vectors, want %d", len(got.Vectors), len(c.Vectors))
	}
}

// A carrier declaring files, handed to a reader with nowhere to put them, must
// say so rather than silently dropping the bodies.
func TestFilesWithoutAStoreAreRefused(t *testing.T) {
	sender := testStore(t)
	c := testCarrier(t)
	c.Blobs = []BlobRef{putBlob(t, sender, []byte("content"))}

	var stick bytes.Buffer
	if err := Write(&stick, c, opener(sender)); err != nil {
		t.Fatal(err)
	}
	_, err := Read(bytes.NewReader(stick.Bytes()), nil)
	if err == nil {
		t.Fatal("a carrier with files was read without a blob store")
	}
	if !strings.Contains(err.Error(), "blob store") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// Bytes nobody declared mean the file is not what it says it is.
func TestUndeclaredTrailingBytesAreRefused(t *testing.T) {
	sender, receiver := testStore(t), testStore(t)
	c := testCarrier(t)
	c.Blobs = []BlobRef{putBlob(t, sender, []byte("declared"))}

	var stick bytes.Buffer
	if err := Write(&stick, c, opener(sender)); err != nil {
		t.Fatal(err)
	}
	extended := append(stick.Bytes(), []byte("and something extra")...)
	if _, err := Read(bytes.NewReader(extended), receiver); !errors.Is(err, ErrNotCanonical) {
		t.Errorf("got %v, want ErrNotCanonical", err)
	}

	// The same rule for a carrier with no files at all.
	bare := testCarrier(t)
	bare.Blobs = nil
	var plain bytes.Buffer
	if err := Write(&plain, bare, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(bytes.NewReader(append(plain.Bytes(), 0)), receiver); !errors.Is(err, ErrNotCanonical) {
		t.Errorf("got %v, want ErrNotCanonical for a bare carrier", err)
	}
}

// A manifest declaring a blob past the ceiling is refused before a byte of the
// body is read — the streaming form of bounding a length before allocating.
func TestAnOversizedDeclarationIsRefusedBeforeReading(t *testing.T) {
	c := testCarrier(t)
	c.Blobs = []BlobRef{{Hash: blobstore.Hash{1}, Size: MaxBlobBytes + 1}}
	if _, err := Encode(c); !errors.Is(err, ErrTooMany) {
		t.Errorf("encode: got %v, want ErrTooMany", err)
	}

	// And from the decoding side, where the declaration is hostile rather than
	// a caller's mistake. Reading must not block waiting for the body.
	c.Blobs = []BlobRef{{Hash: blobstore.Hash{1}, Size: 8}}
	enc, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the declared size to something enormous. It is the last uvarint
	// in the manifest, so appending a larger one after truncating the old is
	// the simplest surgery.
	forged := append(enc[:len(enc)-1], 0xFF, 0xFF, 0xFF, 0xFF, 0x7F)
	if _, err := Read(bytes.NewReader(forged), testStore(t)); err == nil {
		t.Error("a blob declaring a gigantic size was accepted")
	}
}

// Every carrier that goes out must come back in. Asserted over the shapes an
// exchange actually produces rather than one happy path.
func TestWriteReadRoundTripsEveryShape(t *testing.T) {
	shapes := map[string]func(*Carrier, *blobstore.Store){
		"records only": func(c *Carrier, _ *blobstore.Store) { c.Blobs = nil },
		"vectors only": func(c *Carrier, _ *blobstore.Store) { c.Blobs = nil; c.Bundles = nil },
		"files only": func(c *Carrier, bs *blobstore.Store) {
			c.Bundles = nil
			c.Blobs = []BlobRef{putBlob(t, bs, []byte("x"))}
		},
		"records and files": func(c *Carrier, bs *blobstore.Store) { c.Blobs = []BlobRef{putBlob(t, bs, []byte("y"))} },
		"nothing at all": func(c *Carrier, _ *blobstore.Store) {
			c.Bundles = nil
			c.Blobs = nil
			c.Vectors = map[record.AreaTag]*vv.Vector{}
		},
		"an empty-ish file": func(c *Carrier, bs *blobstore.Store) { c.Blobs = []BlobRef{putBlob(t, bs, []byte{0})} },
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			sender, receiver := testStore(t), testStore(t)
			c := testCarrier(t)
			shape(c, sender)

			var stick bytes.Buffer
			if err := Write(&stick, c, opener(sender)); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := Read(bytes.NewReader(stick.Bytes()), receiver)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got.Bundles) != len(c.Bundles) || len(got.Blobs) != len(c.Blobs) ||
				len(got.Vectors) != len(c.Vectors) {
				t.Errorf("round trip gave %d bundles, %d blobs, %d vectors; want %d, %d, %d",
					len(got.Bundles), len(got.Blobs), len(got.Vectors),
					len(c.Bundles), len(c.Blobs), len(c.Vectors))
			}
			for _, ref := range c.Blobs {
				if !receiver.Has(ref.Hash) {
					t.Errorf("file %s did not arrive", ref.Hash)
				}
			}
		})
	}
}
