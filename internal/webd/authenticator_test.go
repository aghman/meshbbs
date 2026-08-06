package webd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/fxamacker/cbor/v2"
)

// A software WebAuthn authenticator, so the passkey path can be tested at all.
//
// # Why this exists
//
// Everything else in this package can be driven with an HTTP request. A passkey
// cannot: registration and assertion require a device holding a private key,
// and without one the single most important path through the web front end —
// the only way anybody signs in — has no coverage. Mocking the library would
// test the mock; this produces the bytes a real authenticator produces and lets
// go-webauthn verify them the way it verifies a browser's.
//
// It implements the narrow slice the BBS actually asks for: ES256, attestation
// "none" (webui.md §7 — a BBS has no use for knowing an authenticator's make),
// and discoverable credentials.

type authenticator struct {
	key       *ecdsa.PrivateKey
	credID    []byte
	handle    []byte
	rpID      string
	origin    string
	signCount uint32
}

func newAuthenticator(t *testing.T, rpID, origin string) *authenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		t.Fatal(err)
	}
	return &authenticator{key: key, credID: credID, rpID: rpID, origin: origin}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// clientData builds the JSON the browser signs over.
func (a *authenticator) clientData(typ, challenge string) []byte {
	// Field order is not significant — the server hashes whatever bytes arrive
	// and the signature covers those same bytes.
	raw, _ := json.Marshal(map[string]any{
		"type":        typ,
		"challenge":   challenge,
		"origin":      a.origin,
		"crossOrigin": false,
	})
	return raw
}

// authData assembles the authenticator data structure (WebAuthn §6.1).
func (a *authenticator) authData(includeCredential bool) []byte {
	rpIDHash := sha256.Sum256([]byte(a.rpID))

	// UP (user present) and UV (user verified) are what a passkey prompt
	// produces; AT marks attested credential data, present only at registration.
	flags := byte(0x01 | 0x04)
	if includeCredential {
		flags |= 0x40
	}

	out := make([]byte, 0, 128)
	out = append(out, rpIDHash[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.signCount)

	if includeCredential {
		out = append(out, make([]byte, 16)...) // AAGUID: all zeroes, as a
		//                                        privacy-preserving
		//                                        authenticator reports.
		out = binary.BigEndian.AppendUint16(out, uint16(len(a.credID)))
		out = append(out, a.credID...)
		out = append(out, a.coseKey()...)
	}
	return out
}

// coseKey encodes the public key as COSE_Key for ES256.
func (a *authenticator) coseKey() []byte {
	x := make([]byte, 32)
	y := make([]byte, 32)
	a.key.PublicKey.X.FillBytes(x)
	a.key.PublicKey.Y.FillBytes(y)

	// Integer keys, per RFC 8152: 1=kty (2 = EC2), 3=alg (-7 = ES256),
	// -1=crv (1 = P-256), -2=x, -3=y.
	raw, _ := cbor.Marshal(map[int]any{
		1: 2, 3: -7, -1: 1, -2: x, -3: y,
	})
	return raw
}

// Register produces the response to navigator.credentials.create().
func (a *authenticator) Register(t *testing.T, challenge string) map[string]any {
	t.Helper()

	attestation, err := cbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": a.authData(true),
	})
	if err != nil {
		t.Fatal(err)
	}

	return map[string]any{
		"id":    b64(a.credID),
		"rawId": b64(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": b64(attestation),
			"clientDataJSON":    b64(a.clientData("webauthn.create", challenge)),
		},
	}
}

// Assert produces the response to navigator.credentials.get().
//
// The counter advances on every assertion, as a real authenticator's does —
// which also exercises the clone check on the way through.
func (a *authenticator) Assert(t *testing.T, challenge string) map[string]any {
	t.Helper()
	a.signCount++

	clientData := a.clientData("webauthn.get", challenge)
	authData := a.authData(false)

	// The signature covers authenticatorData concatenated with the HASH of the
	// client data, not the client data itself.
	clientHash := sha256.Sum256(clientData)
	signed := append(append([]byte{}, authData...), clientHash[:]...)
	digest := sha256.Sum256(signed)

	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return map[string]any{
		"id":    b64(a.credID),
		"rawId": b64(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": b64(authData),
			"clientDataJSON":    b64(clientData),
			"signature":         b64(sig),
			"userHandle":        b64(a.handle),
		},
	}
}
