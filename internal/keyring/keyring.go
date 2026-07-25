// Package keyring implements per-user DM key custody (design §8.2).
//
// # The custody boundary
//
// This package exists to enforce one rule, and the rule is the whole reason it
// is a separate package rather than a few functions in the store:
//
//	The server must NEVER require a user's private key for anything except
//	decrypting a message for display.
//
// Key discovery, DM addressing, encrypting *to* a user, signature
// verification, and delivery must all work from public keys alone. Getting
// that boundary wrong in Phase 1 is what would make tier 3 (client-held keys,
// §8.2) a rewrite rather than an addition — so the API is shaped so that the
// wrong thing is hard to write: Encrypt takes a PublicKey, and the only
// function that touches private material takes an explicit passphrase.
//
// # Three custody tiers
//
//	Tier 1  server-held        sysop can read DMs at rest        (not implemented)
//	Tier 2  passphrase-wrapped sysop holds ciphertext at rest    (the v1 default)
//	Tier 3  client-held        only the user can ever decrypt    (Phase 5)
//
// Tier 2 is what ships. At rest the sysop has ciphertext; during an
// authenticated session the plaintext key is necessarily in server memory,
// because the server renders the message. That is unavoidable at tier 2 and
// must be stated honestly at signup rather than buried in a man page.
package keyring

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
)

// KeySize is the length of an X25519 key in bytes.
const KeySize = 32

// PublicKey is a user's X25519 public key. It is not secret: it is what other
// users encrypt to, and what a PROFILE record publishes (§8.2).
type PublicKey [KeySize]byte

// PrivateKey is a user's X25519 private key in plaintext.
//
// A value of this type should exist only inside an authenticated session, and
// only for as long as it takes to decrypt something. Call Zero when finished.
type PrivateKey [KeySize]byte

// Zero overwrites the private key in memory. This is best-effort — Go may have
// copied the value — but it shortens the window and documents the intent.
func (k *PrivateKey) Zero() {
	for i := range k {
		k[i] = 0
	}
}

// Public derives the public half.
func (k *PrivateKey) Public() (PublicKey, error) {
	out, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		return PublicKey{}, fmt.Errorf("derive public key: %w", err)
	}
	var pub PublicKey
	copy(pub[:], out)
	return pub, nil
}

// String renders a public key for display and storage.
func (p PublicKey) String() string { return base64.RawStdEncoding.EncodeToString(p[:]) }

// ParsePublicKey parses the String form.
func ParsePublicKey(s string) (PublicKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return PublicKey{}, fmt.Errorf("not a valid public key: %w", err)
	}
	if len(raw) != KeySize {
		return PublicKey{}, fmt.Errorf("public key is %d bytes, want %d", len(raw), KeySize)
	}
	var p PublicKey
	copy(p[:], raw)
	return p, nil
}

// ---------------------------------------------------------------------------
// Wrapping (tier 2)
// ---------------------------------------------------------------------------

// Argon2id parameters for wrapping a DM key. Matched to internal/auth so a
// BBS on a Raspberry Pi (§10) stays usable. Encoded into the wrapped blob so
// raising them later does not strand existing users.
const (
	wrapTime    uint32 = 3
	wrapMemory  uint32 = 64 * 1024
	wrapThreads uint8  = 4
	wrapSaltLen        = 16
)

// wrapVersion prefixes the wrapped blob so the KDF parameters can change.
const wrapVersion = 1

// ErrWrongPassphrase is returned when unwrapping fails authentication.
//
// It is deliberately indistinguishable from a corrupt blob: telling a caller
// which of the two happened would let someone probe for valid passphrases.
var ErrWrongPassphrase = errors.New("wrong passphrase (or the wrapped key is corrupt)")

// WrappedKey is a user's private key encrypted under their passphrase. This is
// what the database stores — the sysop has ciphertext at rest.
type WrappedKey struct {
	Version uint8
	Memory  uint32
	Time    uint32
	Threads uint8
	Salt    []byte
	Sealed  []byte // ChaCha20-Poly1305 ciphertext + tag
}

// Generate creates a new X25519 keypair using cryptographic randomness.
func Generate(rnd io.Reader) (PrivateKey, PublicKey, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	var priv PrivateKey
	if _, err := io.ReadFull(rnd, priv[:]); err != nil {
		return PrivateKey{}, PublicKey{}, fmt.Errorf("generate DM key: %w", err)
	}
	// Clamp per RFC 7748.
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64

	pub, err := priv.Public()
	if err != nil {
		return PrivateKey{}, PublicKey{}, err
	}
	return priv, pub, nil
}

// Wrap encrypts a private key under a passphrase.
func Wrap(priv PrivateKey, passphrase string) (*WrappedKey, error) {
	if passphrase == "" {
		return nil, errors.New("passphrase must not be empty")
	}
	salt := make([]byte, wrapSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}

	aead, err := aeadFor(passphrase, salt, wrapMemory, wrapTime, wrapThreads)
	if err != nil {
		return nil, err
	}
	// A fresh random salt per wrap means the derived key is never reused, so a
	// zero nonce is safe here and saves storing one.
	nonce := make([]byte, aead.NonceSize())
	sealed := aead.Seal(nil, nonce, priv[:], nil)

	return &WrappedKey{
		Version: wrapVersion,
		Memory:  wrapMemory,
		Time:    wrapTime,
		Threads: wrapThreads,
		Salt:    salt,
		Sealed:  sealed,
	}, nil
}

// Unwrap decrypts a private key with a passphrase.
//
// This is the ONLY function in the system that yields plaintext private key
// material, and it requires the passphrase explicitly. There is deliberately
// no variant that reads the passphrase from a session, a cache, or the
// database — that would be the shape of tier 1, and it would quietly make the
// tier-3 boundary unenforceable.
func Unwrap(w *WrappedKey, passphrase string) (PrivateKey, error) {
	if w == nil {
		return PrivateKey{}, errors.New("no wrapped key")
	}
	if w.Version != wrapVersion {
		return PrivateKey{}, fmt.Errorf("unsupported wrapped key version %d", w.Version)
	}
	aead, err := aeadFor(passphrase, w.Salt, w.Memory, w.Time, w.Threads)
	if err != nil {
		return PrivateKey{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	out, err := aead.Open(nil, nonce, w.Sealed, nil)
	if err != nil {
		return PrivateKey{}, ErrWrongPassphrase
	}
	if len(out) != KeySize {
		return PrivateKey{}, fmt.Errorf("unwrapped key is %d bytes, want %d", len(out), KeySize)
	}
	var priv PrivateKey
	copy(priv[:], out)
	// The decrypted buffer is a second copy of the secret; clear it.
	for i := range out {
		out[i] = 0
	}
	return priv, nil
}

// Rewrap changes the passphrase protecting a key.
//
// Note what this signature requires: the CURRENT passphrase. A sysop-forced
// password reset cannot call this, because the sysop does not have it — which
// is exactly the §6.7 constraint that a reset either preserves the DM
// passphrase separately or destroys DM history. Callers must surface that
// rather than silently discarding the key.
func Rewrap(w *WrappedKey, oldPassphrase, newPassphrase string) (*WrappedKey, error) {
	priv, err := Unwrap(w, oldPassphrase)
	if err != nil {
		return nil, err
	}
	defer priv.Zero()
	return Wrap(priv, newPassphrase)
}

func aeadFor(passphrase string, salt []byte, memory, time uint32, threads uint8) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}, error) {
	if len(salt) == 0 {
		return nil, errors.New("wrapped key has no salt")
	}
	key := argon2.IDKey([]byte(passphrase), salt, time, memory, threads, chacha20poly1305.KeySize)
	aead, err := chacha20poly1305.New(key)
	// The derived key has been copied into the cipher state; clear our copy.
	for i := range key {
		key[i] = 0
	}
	if err != nil {
		return nil, fmt.Errorf("initialise cipher: %w", err)
	}
	return aead, nil
}

// Encode serialises a wrapped key for storage.
//
// Layout: version(1) | memory(4 BE) | time(4 BE) | threads(1) |
// saltLen(1) | salt | sealed
func (w *WrappedKey) Encode() []byte {
	out := make([]byte, 0, 11+len(w.Salt)+len(w.Sealed))
	out = append(out, w.Version)
	out = append(out, byte(w.Memory>>24), byte(w.Memory>>16), byte(w.Memory>>8), byte(w.Memory))
	out = append(out, byte(w.Time>>24), byte(w.Time>>16), byte(w.Time>>8), byte(w.Time))
	out = append(out, w.Threads, byte(len(w.Salt)))
	out = append(out, w.Salt...)
	out = append(out, w.Sealed...)
	return out
}

// DecodeWrapped parses the Encode form.
func DecodeWrapped(b []byte) (*WrappedKey, error) {
	if len(b) < 11 {
		return nil, errors.New("wrapped key is truncated")
	}
	w := &WrappedKey{
		Version: b[0],
		Memory:  uint32(b[1])<<24 | uint32(b[2])<<16 | uint32(b[3])<<8 | uint32(b[4]),
		Time:    uint32(b[5])<<24 | uint32(b[6])<<16 | uint32(b[7])<<8 | uint32(b[8]),
		Threads: b[9],
	}
	saltLen := int(b[10])
	if len(b) < 11+saltLen {
		return nil, errors.New("wrapped key is truncated in the salt")
	}
	w.Salt = append([]byte(nil), b[11:11+saltLen]...)
	w.Sealed = append([]byte(nil), b[11+saltLen:]...)
	if len(w.Sealed) == 0 {
		return nil, errors.New("wrapped key has no ciphertext")
	}
	return w, nil
}

// ---------------------------------------------------------------------------
// Sealing DMs (§8.2)
// ---------------------------------------------------------------------------

// SealOverhead is the byte cost of sealing: an ephemeral public key plus the
// Poly1305 tag. 48 bytes is 21% of a mesh packet — real, but the floor for
// anything with forward-secrecy properties (§8.2).
const SealOverhead = KeySize + chacha20poly1305.Overhead

// Seal encrypts plaintext to a recipient's public key.
//
// Note the signature: this needs only a PUBLIC key. Sending someone a DM never
// requires the sender's stored private key, which is what lets tier 3 work
// without redesigning delivery.
//
// Layout: ephemeral_pub(32) || ChaCha20-Poly1305(body) || tag(16)
func Seal(recipient PublicKey, plaintext []byte) ([]byte, error) {
	ephPriv, ephPub, err := Generate(rand.Reader)
	if err != nil {
		return nil, err
	}
	defer ephPriv.Zero()

	shared, err := curve25519.X25519(ephPriv[:], recipient[:])
	if err != nil {
		return nil, fmt.Errorf("key agreement: %w", err)
	}
	aead, err := sealAEAD(shared, ephPub, recipient)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, aead.NonceSize())
	out := make([]byte, 0, KeySize+len(plaintext)+chacha20poly1305.Overhead)
	out = append(out, ephPub[:]...)
	return aead.Seal(out, nonce, plaintext, ephPub[:]), nil
}

// Open decrypts a sealed message with the recipient's private key.
//
// This is the one operation that needs private key material, which is why it
// is the only thing tier 3 has to move off the server.
func Open(priv PrivateKey, sealed []byte) ([]byte, error) {
	if len(sealed) < SealOverhead {
		return nil, errors.New("sealed message is too short")
	}
	var ephPub PublicKey
	copy(ephPub[:], sealed[:KeySize])

	recipientPub, err := priv.Public()
	if err != nil {
		return nil, err
	}
	shared, err := curve25519.X25519(priv[:], ephPub[:])
	if err != nil {
		return nil, fmt.Errorf("key agreement: %w", err)
	}
	aead, err := sealAEAD(shared, ephPub, recipientPub)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, aead.NonceSize())
	out, err := aead.Open(nil, nonce, sealed[KeySize:], ephPub[:])
	if err != nil {
		return nil, errors.New("cannot decrypt: wrong key, or the message was tampered with")
	}
	return out, nil
}

// sealAEAD derives the message key from the shared secret.
//
// Both public keys go into the derivation so a ciphertext is bound to the pair
// it was created for — without that, a sealed message could be replayed at a
// different recipient by an attacker who could substitute keys.
func sealAEAD(shared []byte, ephPub, recipientPub PublicKey) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}, error) {
	if allZero(shared) {
		// An all-zero shared secret means a small-order point was supplied.
		return nil, errors.New("invalid key agreement result")
	}
	h, err := blake3DeriveKey(shared, ephPub, recipientPub)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.New(h)
	for i := range h {
		h[i] = 0
	}
	if err != nil {
		return nil, fmt.Errorf("initialise cipher: %w", err)
	}
	return aead, nil
}

func allZero(b []byte) bool {
	var acc byte
	for _, c := range b {
		acc |= c
	}
	return subtle.ConstantTimeByteEq(acc, 0) == 1
}
