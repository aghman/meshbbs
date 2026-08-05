package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestEnrolmentCodeRoundTrip(t *testing.T) {
	code, hash, err := NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckEnrolmentCode(code, hash); err != nil {
		t.Errorf("a freshly minted code should verify: %v", err)
	}
	if strings.Contains(hash, code) {
		t.Error("the hash contains the plaintext code")
	}
}

// TestEnrolmentCodesAreUnpredictable is a smoke test, not a statistical one:
// it catches the failure that actually happens, which is a generator that
// forgets to read randomness at all and returns a constant.
func TestEnrolmentCodesAreUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		code, _, err := NewEnrolmentCode()
		if err != nil {
			t.Fatal(err)
		}
		if seen[code] {
			t.Fatalf("duplicate code %q in 200 draws", code)
		}
		seen[code] = true
	}
}

// TestEnrolmentCodeToleratesTranscription is the whole reason for reusing the
// node ID's alphabet: a code is read off a terminal and typed into a browser,
// usually by the same person, occasionally over the radio.
func TestEnrolmentCodeToleratesTranscription(t *testing.T) {
	code, hash, err := NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}

	variants := map[string]string{
		"as displayed":  code,
		"no hyphens":    strings.ReplaceAll(code, "-", ""),
		"lower case":    strings.ToLower(code),
		"spaces":        strings.ReplaceAll(code, "-", " "),
		"lower, no sep": strings.ToLower(strings.ReplaceAll(code, "-", "")),
	}
	for name, v := range variants {
		if err := CheckEnrolmentCode(v, hash); err != nil {
			t.Errorf("%s (%q) did not verify: %v", name, v, err)
		}
	}
}

func TestEnrolmentCodeRejectsRubbish(t *testing.T) {
	_, hash, err := NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}

	// Wrong length and undecodable input are malformed; a well-formed code
	// that is simply not this one is a mismatch. The distinction matters
	// because only the second is worth rate-limiting as a guess.
	for _, bad := range []string{"", "SHORT", strings.Repeat("Z", 20)} {
		if err := CheckEnrolmentCode(bad, hash); !errors.Is(err, ErrBadCode) {
			t.Errorf("CheckEnrolmentCode(%q) = %v, want ErrBadCode", bad, err)
		}
	}

	other, _, err := NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckEnrolmentCode(other, hash); !errors.Is(err, ErrMismatch) {
		t.Errorf("a different valid code = %v, want ErrMismatch", err)
	}
}

// TestEnrolmentCodeHashIsStable guards the property the store depends on:
// redemption looks a code up BY ITS HASH, so the same code must always hash to
// the same value regardless of how it was typed.
func TestEnrolmentCodeHashIsStable(t *testing.T) {
	code, hash, err := NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	if got := HashEnrolmentCode(code); got != hash {
		t.Errorf("re-hashing the displayed code gave %s, want %s", got, hash)
	}
	if got := HashEnrolmentCode(strings.ToLower(strings.ReplaceAll(code, "-", ""))); got != hash {
		t.Errorf("normalisation is not applied consistently: %s vs %s", got, hash)
	}
}
