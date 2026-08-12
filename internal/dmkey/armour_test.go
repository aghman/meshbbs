package dmkey

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestArmourRoundTrips(t *testing.T) {
	for name, sealed := range map[string][]byte{
		"one byte":            {0x01},
		"a sealed-sized blob": bytes.Repeat([]byte{0xAB}, 48+16),
		"a long message":      bytes.Repeat([]byte{0x5A}, 4096),
		"empty":               {},
	} {
		t.Run(name, func(t *testing.T) {
			block := Armour(sealed)
			if len(sealed) == 0 {
				// An empty seal has no block body, which Unarmour refuses. The
				// case is here to pin that it is refused rather than silently
				// producing empty plaintext.
				if _, err := Unarmour(block); err == nil {
					t.Error("an empty block decoded")
				}
				return
			}
			got, err := Unarmour(block)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, sealed) {
				t.Errorf("round trip changed %d bytes into %d", len(sealed), len(got))
			}
		})
	}
}

// Lines must not reach the width where a terminal takes over the wrapping.
func TestArmourWrapsNarrowEnoughForATerminal(t *testing.T) {
	block := Armour(bytes.Repeat([]byte{0x7F}, 1000))
	for i, line := range strings.Split(block, "\n") {
		if len(line) > armourWidth {
			t.Fatalf("line %d is %d characters, over the %d-column wrap", i, len(line), armourWidth)
		}
	}
}

// A user copies the whole screen. Everything outside the delimiters has to be
// ignored, or they will start trimming by hand and damage the block itself.
func TestUnarmourIgnoresEverythingAroundTheBlock(t *testing.T) {
	sealed := bytes.Repeat([]byte{0x11}, 64)
	block := Armour(sealed)

	pasted := "meshbbs:mail> read 3\r\n" +
		"From: alice@pnw   2 hours ago\r\n" +
		"\r\n" + block + "\r\n" +
		"[q] back  [r] reply\r\n" +
		"meshbbs:mail> \r\n"

	got, err := Unarmour(pasted)
	if err != nil {
		t.Fatalf("a realistic terminal paste was refused: %v", err)
	}
	if !bytes.Equal(got, sealed) {
		t.Error("the surrounding screen changed the payload")
	}
}

// Two messages pasted together must be refused, not silently resolved to the
// first — a user who gets one plaintext from two blocks cannot tell which
// message they are reading.
func TestUnarmourRefusesMoreThanOneBlock(t *testing.T) {
	two := Armour(bytes.Repeat([]byte{1}, 32)) + "\n" + Armour(bytes.Repeat([]byte{2}, 32))
	if _, err := Unarmour(two); !errors.Is(err, ErrManyArmour) {
		t.Errorf("got %v, want ErrManyArmour", err)
	}

	// The same refusal when the paste is damaged rather than doubled: a second
	// BEGIN arriving before the first END means the boundaries cannot be
	// trusted, and picking some of the lines would be guessing.
	damaged := armourBegin + "\nAAAA\n" + armourBegin + "\nBBBB\n" + armourEnd + "\n"
	if _, err := Unarmour(damaged); !errors.Is(err, ErrManyArmour) {
		t.Errorf("got %v, want ErrManyArmour", err)
	}
}

func TestUnarmourRejectsBadInput(t *testing.T) {
	good := Armour(bytes.Repeat([]byte{0x22}, 64))

	cases := map[string]string{
		"nothing at all":       "",
		"prose with no block":  "hello, this is not a message",
		"a begin with no end":  armourBegin + "\nQUJD\n",
		"an end with no begin": "QUJD\n" + armourEnd + "\n",
		"an empty block":       armourBegin + "\n" + armourEnd + "\n",
		"not base64":           armourBegin + "\nthis is not base64!!!\n" + armourEnd + "\n",
		"truncated payload":    good[:len(good)/2],
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Unarmour(in); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A stray character INSIDE the payload must be refused, not skipped.
//
// Written with a body that would decode perfectly if the stray were stripped,
// because the obvious version of this test does not test it: "not base64!!!"
// also fails on length, so a decoder that silently dropped junk would still
// error and the test would still pass. It took a mutation to notice.
//
// Skipping matters because the failure it produces is misdirected: a damaged
// paste would decode to plausible bytes and then fail AUTHENTICATION, telling
// the user their key is wrong when their copy was short.
func TestUnarmourRefusesStrayCharactersInsideThePayload(t *testing.T) {
	// "QUJDRA==" is "ABCD"; the block below is that with one character pushed
	// into the middle of it.
	stray := armourBegin + "\nQUJ!DRA==\n" + armourEnd + "\n"
	if _, err := Unarmour(stray); err == nil {
		t.Fatal("a stray character inside the payload was skipped rather than refused")
	}

	// The control: the same body without the stray decodes.
	clean := armourBegin + "\nQUJDRA==\n" + armourEnd + "\n"
	got, err := Unarmour(clean)
	if err != nil {
		t.Fatalf("the control block was refused: %v", err)
	}
	if string(got) != "ABCD" {
		t.Errorf("control decoded to %q", got)
	}
}

// One payload, one rendering. The encoder always pads, so the decoder must not
// accept the unpadded form — two spellings of one message is the same defect
// the record codecs re-encode-and-compare to prevent, and here it would mean a
// message that verifies from one paste and not from another.
func TestUnarmourAcceptsOnlyTheCanonicalPadding(t *testing.T) {
	padded := armourBegin + "\nQUJDRA==\n" + armourEnd + "\n"
	if _, err := Unarmour(padded); err != nil {
		t.Fatalf("the padded form was refused: %v", err)
	}
	unpadded := armourBegin + "\nQUJDRA\n" + armourEnd + "\n"
	if _, err := Unarmour(unpadded); err == nil {
		t.Error("the unpadded form decoded; the encoder never emits it")
	}
}

// The input is a paste buffer, which is the least trustworthy input in the
// system: the user was told to paste something they did not write.
func TestUnarmourIsBounded(t *testing.T) {
	huge := strings.Repeat("A", MaxArmourBytes+1)
	if _, err := Unarmour(huge); err == nil {
		t.Error("accepted an input past the ceiling")
	}
}

// A paste that survives a Windows terminal has CRs in it.
func TestUnarmourToleratesCarriageReturns(t *testing.T) {
	sealed := bytes.Repeat([]byte{0x33}, 96)
	crlf := strings.ReplaceAll(Armour(sealed), "\n", "\r\n")
	got, err := Unarmour(crlf)
	if err != nil {
		t.Fatalf("a CRLF paste was refused: %v", err)
	}
	if !bytes.Equal(got, sealed) {
		t.Error("CRLF changed the payload")
	}
}

// Anything Unarmour accepts must survive being re-armoured, which is the same
// canonical-form discipline the record codecs use: one payload, one rendering.
func FuzzUnarmour(f *testing.F) {
	f.Add(Armour([]byte{1, 2, 3}))
	f.Add(armourBegin + "\n" + armourEnd)
	f.Add("")
	f.Add("garbage")

	f.Fuzz(func(t *testing.T, s string) {
		sealed, err := Unarmour(s)
		if err != nil {
			return
		}
		again, err := Unarmour(Armour(sealed))
		if err != nil {
			t.Fatalf("a decoded payload would not survive re-armouring: %v", err)
		}
		if !bytes.Equal(again, sealed) {
			t.Fatalf("re-armouring changed %x into %x", sealed, again)
		}
	})
}
