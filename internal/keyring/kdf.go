package keyring

import (
	"golang.org/x/crypto/chacha20poly1305"
	"lukechampine.com/blake3"
)

// dmKeyContext domain-separates the DM message key derivation. A fixed,
// descriptive context string means this KDF output can never collide with any
// other use of BLAKE3 in the system.
const dmKeyContext = "meshbbs DM v1 message key"

// blake3DeriveKey derives the ChaCha20-Poly1305 message key from the X25519
// shared secret and both public keys.
//
// Binding both keys into the derivation is what stops a sealed message being
// re-pointed at a different recipient: the key only reproduces for the exact
// (ephemeral, recipient) pair that created it.
func blake3DeriveKey(shared []byte, ephPub, recipientPub PublicKey) ([]byte, error) {
	h := blake3.New(chacha20poly1305.KeySize, nil)
	if _, err := h.Write([]byte(dmKeyContext)); err != nil {
		return nil, err
	}
	if _, err := h.Write(shared); err != nil {
		return nil, err
	}
	if _, err := h.Write(ephPub[:]); err != nil {
		return nil, err
	}
	if _, err := h.Write(recipientPub[:]); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
