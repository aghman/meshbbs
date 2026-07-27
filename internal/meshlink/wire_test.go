package meshlink

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func testKey(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAnnounceRoundTrip(t *testing.T) {
	key := testKey(t, 1)
	at := time.Unix(1_800_000_000, 0).UTC()

	frame := EncodeAnnounce(key, 0x3368D998, at)
	if len(frame) != announceLen {
		t.Fatalf("frame is %d bytes, want %d", len(frame), announceLen)
	}
	if frame[0] != FrameAnnounce {
		t.Errorf("frame type = %d", frame[0])
	}
	// It has to fit one packet with room to spare, or the mechanism that makes
	// a node addressable would itself need fragmenting.
	if len(frame) > 233 {
		t.Fatalf("announcement does not fit the MTU: %d bytes", len(frame))
	}

	a, err := DecodeAnnounce(frame, 0x3368D998)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The ID is computed from the key, never read off the wire — the property
	// §6.1.1 calls self-certifying.
	if a.ID != key.ID() {
		t.Errorf("ID = %s, want %s", a.ID.Short(), key.ID().Short())
	}
	if !bytes.Equal(a.PublicKey, key.Public) {
		t.Error("public key did not survive the round trip")
	}
	if a.Radio != 0x3368D998 {
		t.Errorf("Radio = %#x", a.Radio)
	}
	if !a.At.Equal(at) {
		t.Errorf("At = %v, want %v", a.At, at)
	}
}

// The attack the radio number in the signed body prevents: capture a peer's
// announcement, rebroadcast it from your own radio, and inherit its traffic.
func TestReplayFromAnotherRadioIsRejected(t *testing.T) {
	key := testKey(t, 1)
	frame := EncodeAnnounce(key, 0x11111111, time.Unix(1_800_000_000, 0))

	if _, err := DecodeAnnounce(frame, 0x22222222); !errors.Is(err, ErrRadioMismatch) {
		t.Fatalf("err = %v, want ErrRadioMismatch", err)
	}
}

func TestTamperedAnnouncementIsRejected(t *testing.T) {
	key := testKey(t, 1)
	good := EncodeAnnounce(key, 0x11111111, time.Unix(1_800_000_000, 0))

	// Every single-byte change to a signed frame must fail verification. That
	// includes the key itself: substituting one produces a different node ID,
	// so an attacker cannot keep the signature and change who it is from.
	for _, i := range []int{1, 20, 33, 37, 50, len(good) - 1} {
		bad := append([]byte(nil), good...)
		bad[i] ^= 0x01
		if _, err := DecodeAnnounce(bad, 0x11111111); err == nil {
			t.Errorf("byte %d could be flipped without detection", i)
		}
	}
}

func TestMalformedAnnouncementsAreRejected(t *testing.T) {
	key := testKey(t, 1)
	good := EncodeAnnounce(key, 1, time.Unix(1_800_000_000, 0))

	cases := map[string][]byte{
		"empty":       {},
		"type only":   {FrameAnnounce},
		"truncated":   good[:len(good)-1],
		"overlong":    append(append([]byte(nil), good...), 0),
		"wrong type":  append([]byte{FrameSymbol}, good[1:]...),
		"other frame": {FrameControl, 1, 2, 3},
	}
	for name, in := range cases {
		if _, err := DecodeAnnounce(in, 1); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Anyone holding the channel PSK can put bytes in front of this parser, and it
// runs before any of the trust machinery above it.
func FuzzDecodeAnnounce(f *testing.F) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(EncodeAnnounce(key, 0x1234, time.Unix(1_800_000_000, 0)), uint32(0x1234))
	f.Add([]byte{FrameAnnounce}, uint32(0))
	f.Add([]byte{}, uint32(0))
	f.Add(EncodeWhoIs(), uint32(7))

	f.Fuzz(func(t *testing.T, data []byte, from uint32) {
		a, err := DecodeAnnounce(data, from)
		if err != nil {
			return
		}
		// Anything that verifies must be internally consistent: the ID is the
		// hash of the key it carries, and the radio is the one it arrived from.
		// A parser that could produce a mismatch here would let an attacker
		// bind someone else's ID to their own radio.
		if !a.ID.Matches(a.PublicKey) {
			t.Fatalf("accepted an announcement whose ID does not match its key")
		}
		if a.Radio != from {
			t.Fatalf("accepted an announcement claiming radio %#x from %#x", a.Radio, from)
		}
	})
}
