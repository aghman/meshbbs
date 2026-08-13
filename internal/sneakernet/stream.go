package sneakernet

import (
	"errors"
	"fmt"
	"io"

	"github.com/aghman/meshbbs/internal/blobstore"
)

// ErrBlobMismatch is returned when a carried blob is not what it claimed.
//
// Content addressing is what makes an unsigned container safe to take file
// bytes from: the hash in the manifest is the identity, so a carrier that
// substitutes different content produces a different hash and is caught. It
// cannot lie about what a blob IS — only about which blobs it has, which is a
// wasted trip rather than a compromise.
var ErrBlobMismatch = errors.New("a carried file is not what the carrier said it was")

// ErrShortBlob is returned when the stream ends inside a declared body.
var ErrShortBlob = errors.New("carrier ended inside a file body")

// Write streams a carrier: the manifest, then each declared body in order.
//
// open supplies bodies by hash. It is a callback rather than a slice of readers
// because the caller is the blob store, which opens files lazily — materialising
// every body first would defeat the point of streaming.
//
// The manifest goes out first and completely. A reader can therefore refuse
// everything that follows on the strength of the declarations alone, without
// reading a byte of it, which is the streaming form of bounding a length before
// allocating against it.
func Write(w io.Writer, c *Carrier, open func(blobstore.Hash) (io.ReadCloser, error)) error {
	manifest, err := Encode(c)
	if err != nil {
		return err
	}
	if _, err := w.Write(manifest); err != nil {
		return fmt.Errorf("write the carrier manifest: %w", err)
	}

	for _, ref := range c.Blobs {
		if open == nil {
			return fmt.Errorf("carrier declares %d files but no source was given", len(c.Blobs))
		}
		body, err := open(ref.Hash)
		if err != nil {
			return fmt.Errorf("open %s: %w", ref.Hash, err)
		}
		// LimitReader, not trust: a body that grew since the manifest was built
		// would desynchronise the stream from its declarations, and every
		// subsequent blob would be read from the wrong offset.
		n, err := io.Copy(w, io.LimitReader(body, int64(ref.Size)))
		body.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w", ref.Hash, err)
		}
		if uint64(n) != ref.Size {
			return fmt.Errorf("%s: wrote %d bytes, the manifest declared %d", ref.Hash, n, ref.Size)
		}
	}
	return nil
}

// Read parses a carrier and streams its file bodies into a blob store.
//
// The manifest is read first and read whole, because it is small and canonical
// and everything after it is described by it. Bodies then go straight to disk
// through blobstore.Put, which hashes as it writes — so a 200 MB file crosses
// without ever being a 200 MB allocation, on hardware §4 assumes might be a
// Raspberry Pi.
//
// into may be nil, which reads the manifest and refuses to go further if bodies
// were declared. That is the request-only leg: a carrier saying "here is what I
// hold, send me the difference" has no bodies and needs no store.
func Read(r io.Reader, into *blobstore.Store) (*Carrier, error) {
	// The manifest is length-bounded but not length-prefixed, so it is read up
	// to the ceiling and Decode finds its own end. Reading one byte past the
	// limit is what distinguishes "a large manifest" from "a manifest claiming
	// to be larger than we will ever accept".
	head, err := io.ReadAll(io.LimitReader(r, MaxCarrierBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read the carrier: %w", err)
	}
	// allowTrailing, because the file bodies follow the manifest. The canonical
	// check still applies to the manifest prefix; see decode.
	c, consumed, err := decode(head, true)
	if err != nil {
		return nil, err
	}
	if len(c.Blobs) == 0 {
		if consumed != len(head) {
			return nil, fmt.Errorf("%w: %d bytes follow a carrier that declares no files",
				ErrNotCanonical, len(head)-consumed)
		}
		return c, nil
	}
	if into == nil {
		return nil, fmt.Errorf("this carrier holds %d file(s) and no blob store was given",
			len(c.Blobs))
	}

	// Whatever of the bodies came in with the first read, followed by the rest
	// of the stream.
	bodies := io.MultiReader(byteReader(head[consumed:]), r)
	for i, ref := range c.Blobs {
		got, n, err := into.Put(io.LimitReader(bodies, int64(ref.Size)))
		if err != nil {
			return nil, fmt.Errorf("store file %d of %d: %w", i+1, len(c.Blobs), err)
		}
		if uint64(n) != ref.Size {
			// Short read: the stream ended early. The blob just written is
			// content-addressed under whatever did arrive, so it is not
			// masquerading as the truncated file — it is simply a blob nobody
			// references, which the store's own maintenance collects.
			return nil, fmt.Errorf("%w: file %d ended after %d of %d bytes",
				ErrShortBlob, i+1, n, ref.Size)
		}
		if got != ref.Hash {
			return nil, fmt.Errorf("%w: declared %s, carried %s", ErrBlobMismatch, ref.Hash, got)
		}
	}

	// Anything after the last declared body is unaccounted for. Refused rather
	// than ignored: a carrier is a description of its own contents, and bytes
	// it does not describe mean it is not the thing it claims to be.
	var tail [1]byte
	if n, _ := io.ReadFull(bodies, tail[:]); n != 0 {
		return nil, fmt.Errorf("%w: bytes follow the last declared file", ErrNotCanonical)
	}
	return c, nil
}

// byteReader is io.MultiReader's first half without importing bytes for one use.
func byteReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}
