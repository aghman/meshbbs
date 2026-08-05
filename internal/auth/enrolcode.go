package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/identity"
	"lukechampine.com/blake3"
)

// Enrolment codes bootstrap a passkey onto an account that predates the web
// (webui.md §8, [D18]).
//
// # The one property that matters
//
// A code registers a credential. It CANNOT mint a session. That is not an
// implementation detail to be relaxed later for convenience — a code that logs
// someone in is a password with worse ergonomics, and every other property here
// (single use, minutes-long expiry, one live code per account) is chosen on the
// assumption that its authority is narrow. Widening the authority invalidates
// the rest of the design, not just this file.
//
// # Why hashed
//
// Same reason password_hash is: a leaked database must not yield live codes.
// Unlike a password, a code is 64 bits of crypto/rand output, so it needs no
// key stretching — Argon2 here would cost a second per redemption to defend
// against a dictionary attack on a value that is not in any dictionary. A fast
// hash over a high-entropy secret is the right trade; a fast hash over a
// human-chosen secret would not be.

// codeBytes is the entropy behind a code. Eight bytes encode to the same
// thirteen Crockford characters as a node ID, so the alphabet and the 4-4-5
// grouping are already familiar to anyone who has read one aloud.
const codeBytes = 8

// ErrBadCode is returned when a code is malformed.
var ErrBadCode = errors.New("not a valid enrolment code")

// NewEnrolmentCode returns a fresh code and its hash. The plaintext is shown to
// the user once and never stored.
func NewEnrolmentCode() (code, hash string, err error) {
	b := make([]byte, codeBytes)
	// Credentials require cryptographic randomness, never the seeded
	// rng.Source used by domain logic (§12.1). A seeded enrolment code would
	// be predictable to anyone who could guess the seed, which is the whole
	// point of the deterministic simulator.
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate enrolment code: %w", err)
	}
	raw := identity.EncodeBase32(b)
	return GroupEnrolmentCode(raw), HashEnrolmentCode(raw), nil
}

// GroupEnrolmentCode formats a code for display as 4-4-5, matching how node IDs
// are rendered.
func GroupEnrolmentCode(raw string) string { return identity.Group(raw) }

// NormaliseEnrolmentCode strips grouping and case so a user may type a code
// however it is easiest.
//
// Crockford base32 already folds case and treats I/L as 1 and O as 0, which is
// most of the transcription-error budget. Removing hyphens and spaces covers
// the rest: somebody reading "K3M9-P2QR-7TVWX" aloud gets it right whether the
// listener types the hyphens or not.
func NormaliseEnrolmentCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		if r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

// HashEnrolmentCode hashes a normalised code for storage and lookup.
func HashEnrolmentCode(raw string) string {
	sum := blake3.Sum256([]byte(NormaliseEnrolmentCode(raw)))
	return fmt.Sprintf("%x", sum)
}

// CheckEnrolmentCode reports whether a typed code matches a stored hash.
//
// The comparison is constant-time. That is close to theatre for a value looked
// up by hash rather than compared in a loop, but it costs nothing and it means
// the answer does not change if a future caller compares candidates directly.
func CheckEnrolmentCode(typed, hash string) error {
	normalised := NormaliseEnrolmentCode(typed)
	if len(normalised) != len(identity.EncodeBase32(make([]byte, codeBytes))) {
		return ErrBadCode
	}
	if _, err := identity.DecodeBase32(normalised); err != nil {
		return ErrBadCode
	}
	if subtle.ConstantTimeCompare([]byte(HashEnrolmentCode(normalised)), []byte(hash)) != 1 {
		return ErrMismatch
	}
	return nil
}
