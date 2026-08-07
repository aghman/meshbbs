// Package blobstore holds file contents on disk, addressed by their hash
// (design §6.5).
//
// # Why content addressing
//
// §6.5 asks for it so identical files across areas dedup, which on a Raspberry
// Pi with an SD card (§10) is worth having. But the more important property is
// the one the mesh needs: a FILE record replicates a *hash*, and every BBS that
// receives it must be able to say whether it holds that content without asking
// anyone. A path-addressed store cannot answer that — two BBSes name the same
// file differently — and "held by" is the whole point of a catalog that
// replicates without its bytes (§7.5).
//
// # What this package deliberately does not do
//
// It does not know about areas, users, or records. It maps hashes to bytes. The
// catalog — which area a file is in, who uploaded it, what it is called — lives
// in the database, because those are the parts that differ between two nodes
// holding identical content.
package blobstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"lukechampine.com/blake3"
)

// HashLen is the width of a blob address: BLAKE3-256.
//
// The full 32 bytes, not the 16-byte truncation a FILE record carries on the
// wire. Local integrity checking pays no airtime, so it has no reason to
// economize; the wire truncation is a separate decision made against a
// 233-byte MTU.
const HashLen = 32

// Hash is the content address of a blob.
type Hash [HashLen]byte

func (h Hash) String() string { return hex.EncodeToString(h[:]) }
func (h Hash) IsZero() bool   { return h == Hash{} }

// ParseHash decodes a hex-encoded blob hash.
func ParseHash(s string) (Hash, error) {
	var h Hash
	b, err := hex.DecodeString(s)
	if err != nil {
		return h, fmt.Errorf("blobstore: %q is not a hash: %w", s, err)
	}
	if len(b) != HashLen {
		return h, fmt.Errorf("blobstore: hash is %d bytes, expected %d", len(b), HashLen)
	}
	copy(h[:], b)
	return h, nil
}

// ErrNotFound is returned when a blob is not held locally.
//
// This is an ordinary condition, not a failure: the network catalog names files
// held by other BBSes, and asking for one we do not have is how the UI learns
// to say "held by pnw" rather than offering a download (§6.5).
var ErrNotFound = errors.New("blob not held locally")

// Store is a content-addressed store rooted at a directory.
type Store struct{ root string }

// Open prepares a blob store under root, creating it if needed.
func Open(root string) (*Store, error) {
	if root == "" {
		return nil, errors.New("blobstore: no root directory")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("blobstore: create %s: %w", abs, err)
	}
	return &Store{root: abs}, nil
}

// Root reports the directory the store writes under.
func (s *Store) Root() string { return s.root }

// path returns the on-disk location for a hash: <root>/ab/cd/<full hex>.
//
// Two levels of fan-out, one byte each. A flat directory of a few thousand
// files is where ext4 and HFS+ start to slow down and where `ls` in a support
// session stops being useful; 65536 buckets pushes both problems past any
// plausible BBS. The full hex is repeated in the filename so a bucket listing
// is still self-describing.
func (s *Store) path(h Hash) string {
	hexed := h.String()
	return filepath.Join(s.root, hexed[0:2], hexed[2:4], hexed)
}

// Put streams r into the store and returns its hash and size.
//
// The content is hashed as it is written, so the caller does not have to buffer
// a whole file in memory to learn its address — an upload may be far larger
// than a BBS host's RAM.
//
// Writing goes to a temporary file that is renamed into place only once the
// content is complete. A crash mid-upload therefore leaves a stray temp file
// rather than a blob whose name lies about its contents, which is the one
// corruption a content-addressed store must never have: every later reader
// trusts the name instead of re-hashing.
func (s *Store) Put(r io.Reader) (Hash, int64, error) {
	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: create %s: %w", tmpDir, err)
	}
	tmp, err := os.CreateTemp(tmpDir, "incoming-*")
	if err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Both are no-ops on the success path, which closes and renames first.
	defer os.Remove(tmpName)
	defer tmp.Close()

	hasher := blake3.New(HashLen, nil)
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: write blob: %w", err)
	}
	// Sync before rename: the rename is what publishes the blob under a name
	// that asserts its contents, and on a BBS host that loses power (§10, a Pi
	// on a shelf) an unsynced rename can land ahead of the bytes it names.
	if err := tmp.Sync(); err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: sync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: close blob: %w", err)
	}

	var h Hash
	copy(h[:], hasher.Sum(nil))

	dest := s.path(h)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: create bucket: %w", err)
	}
	// An existing blob is the dedup case, not a conflict: the name IS the
	// content, so what is already there is byte-identical to what we just
	// wrote. Keep the original and drop the copy.
	if _, err := os.Stat(dest); err == nil {
		return h, size, nil
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return Hash{}, 0, fmt.Errorf("blobstore: place blob: %w", err)
	}
	return h, size, nil
}

// Open returns a reader for a blob's contents.
func (s *Store) Open(h Hash) (*os.File, error) {
	f, err := os.Open(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
	}
	if err != nil {
		return nil, fmt.Errorf("blobstore: open %s: %w", h, err)
	}
	return f, nil
}

// Has reports whether the blob is held locally.
func (s *Store) Has(h Hash) bool {
	_, err := os.Stat(s.path(h))
	return err == nil
}

// Size reports a held blob's size in bytes.
func (s *Store) Size(h Hash) (int64, error) {
	info, err := os.Stat(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, h)
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Remove deletes a blob's bytes.
//
// The caller is responsible for having established that no catalog row still
// references the hash — this package does not know what a catalog is. Removing
// a blob that is not held is not an error, so a half-finished delete can be
// retried.
func (s *Store) Remove(h Hash) error {
	err := os.Remove(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("blobstore: remove %s: %w", h, err)
	}
	return nil
}

// Verify re-hashes a held blob and reports whether it still matches its name.
//
// Nothing calls this on the read path: doing so would make every download cost
// a full re-hash to defend against a fault the filesystem is supposed to
// prevent. It exists for a sysop maintenance command, where the cost is
// acceptable and the answer is the one thing a content-addressed store cannot
// otherwise be asked.
func (s *Store) Verify(h Hash) error {
	f, err := s.Open(h)
	if err != nil {
		return err
	}
	defer f.Close()

	hasher := blake3.New(HashLen, nil)
	if _, err := io.Copy(hasher, f); err != nil {
		return fmt.Errorf("blobstore: read %s: %w", h, err)
	}
	var got Hash
	copy(got[:], hasher.Sum(nil))
	if got != h {
		return fmt.Errorf("blobstore: %s hashes to %s — the stored bytes have changed", h, got)
	}
	return nil
}
