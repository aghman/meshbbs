package identity

import (
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/aghman/meshbbs/internal/rng"
)

// NodeKeyFile is the filename of the node's identity key within the keys dir.
const NodeKeyFile = "node.ed25519"

// keyFileMode is the only permission a private key may have on disk. §11.2
// requires the process to refuse to start on anything looser.
const keyFileMode os.FileMode = 0o600

// NodeKey is a BBS instance's identity: the Ed25519 keypair whose public half
// determines the node's ID (§6.1.1).
type NodeKey struct {
	Public  ed25519.PublicKey
	Private ed25519.PrivateKey
}

// ID returns the node ID derived from the public key.
func (k NodeKey) ID() NodeID { return NodeIDFromPublicKey(k.Public) }

// Sign signs msg with the node key.
func (k NodeKey) Sign(msg []byte) []byte { return ed25519.Sign(k.Private, msg) }

// GenerateNodeKey creates a new node keypair using cryptographic randomness.
//
// It takes an rng.Secret rather than reading crypto/rand directly so that tests
// can produce reproducible identities — but note the production caller must
// always pass rng.NewSecret().
func GenerateNodeKey(secret rng.Secret) (NodeKey, error) {
	pub, priv, err := ed25519.GenerateKey(secret.Reader())
	if err != nil {
		return NodeKey{}, fmt.Errorf("generate node key: %w", err)
	}
	return NodeKey{Public: pub, Private: priv}, nil
}

// SaveNodeKey writes the private key to dir/node.ed25519 with 0600
// permissions. It refuses to overwrite an existing key: losing a node key means
// losing the node's identity permanently (§6.1.6), so clobbering one must take
// a deliberate act outside this program.
func SaveNodeKey(dir string, k NodeKey) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	path := filepath.Join(dir, NodeKeyFile)

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("node key already exists at %s; refusing to overwrite "+
			"(a lost node key cannot be recovered — see design §6.1.6)", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	block := &pem.Block{
		Type: "MESHBBS NODE PRIVATE KEY",
		Headers: map[string]string{
			// Recorded for human inspection only. The ID is always re-derived
			// from the key on load; this header is never trusted.
			"node-id": k.ID().Compact(),
		},
		Bytes: k.Private.Seed(),
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFileMode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, block); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// LoadNodeKey reads the node key from dir, verifying its permissions.
func LoadNodeKey(dir string) (NodeKey, error) {
	path := filepath.Join(dir, NodeKeyFile)

	info, err := os.Stat(path)
	if err != nil {
		return NodeKey{}, fmt.Errorf("no node key at %s (run `meshbbs init`): %w", path, err)
	}
	if err := checkKeyPerms(path, info); err != nil {
		return NodeKey{}, err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return NodeKey{}, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return NodeKey{}, fmt.Errorf("%s is not a valid PEM file", path)
	}
	if len(block.Bytes) != ed25519.SeedSize {
		return NodeKey{}, fmt.Errorf("%s: expected a %d-byte seed, got %d",
			path, ed25519.SeedSize, len(block.Bytes))
	}

	priv := ed25519.NewKeyFromSeed(block.Bytes)
	return NodeKey{
		Public:  priv.Public().(ed25519.PublicKey),
		Private: priv,
	}, nil
}

// checkKeyPerms enforces 0600 on private key material. Windows does not carry
// Unix permission bits in a way this check can read meaningfully, so it is
// skipped there — the design's threat model for local file security is
// multi-user Unix hosts.
func checkKeyPerms(path string, info os.FileInfo) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := info.Mode().Perm(); perm != keyFileMode {
		return fmt.Errorf("%s has permissions %04o, want %04o; "+
			"refusing to load a private key readable by other users (run: chmod 600 %s)",
			path, perm, keyFileMode, path)
	}
	return nil
}
