package keyring

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func mustGenerate(t *testing.T) (PrivateKey, PublicKey) {
	t.Helper()
	priv, pub, err := Generate(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pub
}

func TestGenerateProducesMatchingPair(t *testing.T) {
	priv, pub := mustGenerate(t)
	derived, err := priv.Public()
	if err != nil {
		t.Fatal(err)
	}
	if derived != pub {
		t.Fatal("Generate returned a public key that does not match its private key")
	}

	_, other := mustGenerate(t)
	if pub == other {
		t.Fatal("two generated keypairs collided")
	}
}

func TestWrapUnwrapRoundTrip(t *testing.T) {
	priv, _ := mustGenerate(t)

	w, err := Wrap(priv, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unwrap(w, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if got != priv {
		t.Fatal("unwrapped key differs from the original")
	}
}

// §8.2: at rest the sysop holds ciphertext. The wrapped blob must not contain
// the plaintext key anywhere.
func TestWrappedBlobDoesNotLeakTheKey(t *testing.T) {
	priv, _ := mustGenerate(t)
	w, err := Wrap(priv, "passphrase")
	if err != nil {
		t.Fatal(err)
	}
	blob := w.Encode()
	if bytes.Contains(blob, priv[:]) {
		t.Fatal("the wrapped blob contains the plaintext private key")
	}
	// Also check the sealed section specifically, in case Encode changes.
	if bytes.Contains(w.Sealed, priv[:]) {
		t.Fatal("the ciphertext contains the plaintext private key")
	}
}

func TestUnwrapRejectsWrongPassphrase(t *testing.T) {
	priv, _ := mustGenerate(t)
	w, err := Wrap(priv, "right")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unwrap(w, "wrong"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("expected ErrWrongPassphrase, got %v", err)
	}
}

// A corrupt blob and a wrong passphrase must be indistinguishable, so the
// error cannot be used to probe for valid passphrases.
func TestCorruptBlobLooksLikeAWrongPassphrase(t *testing.T) {
	priv, _ := mustGenerate(t)
	w, err := Wrap(priv, "pw")
	if err != nil {
		t.Fatal(err)
	}
	w.Sealed[0] ^= 0xff
	if _, err := Unwrap(w, "pw"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("corrupt ciphertext produced a distinguishable error: %v", err)
	}
}

func TestWrappedKeyEncodeRoundTrip(t *testing.T) {
	priv, _ := mustGenerate(t)
	w, err := Wrap(priv, "pw")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeWrapped(w.Encode())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unwrap(decoded, "pw")
	if err != nil {
		t.Fatal(err)
	}
	if got != priv {
		t.Fatal("key did not survive an encode/decode round trip")
	}
}

func TestDecodeWrappedRejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{
		nil, {}, {1}, make([]byte, 10),
		append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0, 4, 200}, 1, 2, 3), // salt longer than input
	} {
		if _, err := DecodeWrapped(bad); err == nil {
			t.Errorf("accepted malformed wrapped key %v", bad)
		}
	}
}

// §6.7: changing a passphrase requires the CURRENT one. This is the property
// that makes a sysop-forced password reset unable to silently re-wrap the DM
// key — which is why the reset path must warn instead.
func TestRewrapRequiresTheCurrentPassphrase(t *testing.T) {
	priv, _ := mustGenerate(t)
	w, err := Wrap(priv, "old")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Rewrap(w, "not-the-old-one", "new"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("Rewrap succeeded without the current passphrase: %v", err)
	}

	w2, err := Rewrap(w, "old", "new")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Unwrap(w2, "new")
	if err != nil {
		t.Fatal(err)
	}
	if got != priv {
		t.Fatal("rewrapped key differs from the original")
	}
	// The old passphrase must no longer work.
	if _, err := Unwrap(w2, "old"); err == nil {
		t.Fatal("the old passphrase still unwraps the rewrapped key")
	}
}

// §8.2: sealing needs only a PUBLIC key. This is the boundary that keeps tier 3
// an addition rather than a rewrite — if sending required stored private
// material, moving keys to the client would mean redesigning delivery.
func TestSealNeedsOnlyThePublicKey(t *testing.T) {
	recipientPriv, recipientPub := mustGenerate(t)
	defer recipientPriv.Zero()

	msg := []byte("meet me on the repeater at 19:00")
	sealed, err := Seal(recipientPub, msg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(recipientPriv, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("decrypted %q, want %q", got, msg)
	}
}

func TestOpenRejectsTheWrongKey(t *testing.T) {
	_, recipientPub := mustGenerate(t)
	wrongPriv, _ := mustGenerate(t)
	defer wrongPriv.Zero()

	sealed, err := Seal(recipientPub, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(wrongPriv, sealed); err == nil {
		t.Fatal("a message decrypted with the wrong private key")
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	_, pub := mustGenerate(t)
	a, err := Seal(pub, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Seal(pub, []byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("sealing the same plaintext twice produced identical ciphertext; " +
			"the ephemeral key is not fresh")
	}
}

func TestSealDetectsTampering(t *testing.T) {
	priv, pub := mustGenerate(t)
	defer priv.Zero()

	sealed, err := Seal(pub, []byte("transfer 100 credits"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range sealed {
		tampered := append([]byte(nil), sealed...)
		tampered[i] ^= 0x01
		if _, err := Open(priv, tampered); err == nil {
			t.Fatalf("tampering at byte %d went undetected", i)
		}
	}
}

// §8.2 states 48 bytes of overhead. Assert it, since it is a load-bearing
// number in the airtime budget (21% of a mesh packet).
func TestSealOverheadMatchesTheDesign(t *testing.T) {
	if SealOverhead != 48 {
		t.Fatalf("SealOverhead is %d, design §8.2 says 48", SealOverhead)
	}
	_, pub := mustGenerate(t)
	msg := []byte("0123456789")
	sealed, err := Seal(pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(sealed) - len(msg); got != SealOverhead {
		t.Fatalf("actual overhead is %d bytes, want %d", got, SealOverhead)
	}
}

func TestOpenRejectsShortInput(t *testing.T) {
	priv, _ := mustGenerate(t)
	defer priv.Zero()
	for n := 0; n < SealOverhead; n++ {
		if _, err := Open(priv, make([]byte, n)); err == nil {
			t.Fatalf("Open accepted a %d-byte input", n)
		}
	}
}

func TestPublicKeyStringRoundTrip(t *testing.T) {
	_, pub := mustGenerate(t)
	got, err := ParsePublicKey(pub.String())
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Fatal("public key did not survive a string round trip")
	}
	for _, bad := range []string{"", "!!!", "c2hvcnQ"} {
		if _, err := ParsePublicKey(bad); err == nil {
			t.Errorf("accepted invalid public key %q", bad)
		}
	}
}

func TestZeroClearsTheKey(t *testing.T) {
	priv, _ := mustGenerate(t)
	priv.Zero()
	if priv != (PrivateKey{}) {
		t.Fatal("Zero did not clear the private key")
	}
}

func TestWrapRejectsEmptyPassphrase(t *testing.T) {
	priv, _ := mustGenerate(t)
	if _, err := Wrap(priv, ""); err == nil {
		t.Fatal("accepted an empty passphrase")
	}
}
