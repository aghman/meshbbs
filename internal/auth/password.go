// Package auth handles credential hashing (design §6.7, §11.5).
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. These are the moderate defaults from RFC 9106's second
// recommendation: 64 MiB, 3 passes, 4 lanes. A BBS may well run on a Raspberry
// Pi (§10), so memory-hardness is traded down from the aggressive profile.
//
// They are encoded into every hash, so raising them later does not invalidate
// existing passwords — old hashes keep verifying with their own parameters.
const (
	argonTime    uint32 = 3
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	saltLen             = 16
)

// ErrMismatch is returned when a password does not match its hash.
var ErrMismatch = errors.New("password does not match")

// HashPassword returns an encoded Argon2id hash in the standard PHC string
// format, which carries the parameters and salt alongside the digest.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password must not be empty")
	}
	salt := make([]byte, saltLen)
	// Password hashing salts require cryptographic randomness, never the
	// seeded rng.Source used by domain logic (§12.1).
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks a password against an encoded hash.
func VerifyPassword(password, encoded string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return fmt.Errorf("not an argon2id hash")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return fmt.Errorf("unreadable argon2 version: %w", err)
	}
	if version != argon2.Version {
		return fmt.Errorf("unsupported argon2 version %d", version)
	}

	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return fmt.Errorf("unreadable argon2 parameters: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return fmt.Errorf("unreadable salt: %w", err)
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return fmt.Errorf("unreadable digest: %w", err)
	}

	// Use the stored parameters, not the current constants, so raising the
	// defaults does not lock existing users out.
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}
