package sneakernet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/vv"
)

func testVector(t *testing.T, seeds ...uint64) *vv.Vector {
	t.Helper()
	v := vv.New()
	for i, seed := range seeds {
		key, err := identity.GenerateNodeKey(rng.TestSecret(seed))
		if err != nil {
			t.Fatal(err)
		}
		v.Set(key.ID(), uint64(i+1)*7)
	}
	return v
}

func testCarrier(t *testing.T) *Carrier {
	t.Helper()
	key, err := identity.GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	return &Carrier{
		Origin:    key.ID(),
		CreatedAt: 1_765_000_000,
		Vectors: map[record.AreaTag]*vv.Vector{
			record.AreaTagFor("general"): testVector(t, 2, 3),
			record.AreaTagFor("tech"):    testVector(t, 2),
		},
		Bundles: [][]byte{
			bytes.Repeat([]byte{0xAA}, 120),
			bytes.Repeat([]byte{0xBB}, 40),
		},
	}
}

func TestCarrierRoundTrips(t *testing.T) {
	c := testCarrier(t)
	enc, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}

	if got.Origin != c.Origin || got.CreatedAt != c.CreatedAt {
		t.Errorf("header round-tripped as %x/%d", got.Origin[:], got.CreatedAt)
	}
	if len(got.Vectors) != len(c.Vectors) {
		t.Fatalf("got %d vectors, want %d", len(got.Vectors), len(c.Vectors))
	}
	for area, want := range c.Vectors {
		gotVec, ok := got.Vectors[area]
		if !ok {
			t.Fatalf("area %x is missing", area[:])
		}
		if !gotVec.Equal(want) {
			t.Errorf("area %x round-tripped to a different vector", area[:])
		}
	}
	if len(got.Bundles) != len(c.Bundles) {
		t.Fatalf("got %d bundles, want %d", len(got.Bundles), len(c.Bundles))
	}
	for i := range c.Bundles {
		if !bytes.Equal(got.Bundles[i], c.Bundles[i]) {
			t.Errorf("bundle %d changed", i)
		}
	}
}

// Vectors come from a map, and Go randomises map iteration (§6.2.1 rule 2).
// Without a sort the same carrier would have a different form on every write,
// which breaks the canonical check and would make two identical sticks compare
// unequal.
func TestEncodeIsDeterministic(t *testing.T) {
	c := testCarrier(t)
	first, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Encode(c)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(again, first) {
			t.Fatal("encoding the same carrier twice produced different bytes")
		}
	}
}

// The first question about a file found on a stick is whether it is one of ours.
func TestDecodeRejectsForeignFiles(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":        {},
		"short":        {'M', 'B'},
		"wrong magic":  append([]byte("NOPE"), 1, 2, 3),
		"a text file":  []byte("Dear sysop,\n\nthanks for the files.\n"),
		"a zip":        {0x50, 0x4B, 0x03, 0x04, 0, 0, 0, 0},
		"wrong versio": append(append(Magic[:], FormatVersion+1), bytes.Repeat([]byte{0}, 12)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(in); !errors.Is(err, ErrNotACarrier) && !errors.Is(err, ErrTruncated) {
				t.Errorf("got %v, want ErrNotACarrier or ErrTruncated", err)
			}
		})
	}
}

// The input is a file an attacker chose the bytes AND the size of, with no link
// authentication and no rate limit in front of it. Every count is bounded
// before it is allocated against.
func TestDecodeIsBoundedAgainstHostileCounts(t *testing.T) {
	header := func() []byte {
		b := append([]byte{}, Magic[:]...)
		b = append(b, FormatVersion)
		b = append(b, bytes.Repeat([]byte{0}, identity.NodeIDLen)...)
		b = binary.BigEndian.AppendUint32(b, 0)
		return b
	}

	t.Run("a vector count past the limit", func(t *testing.T) {
		b := binary.AppendUvarint(header(), uint64(MaxVectors+1))
		if _, err := Decode(b); !errors.Is(err, ErrTooMany) {
			t.Errorf("got %v, want ErrTooMany", err)
		}
	})

	t.Run("an enormous vector count", func(t *testing.T) {
		b := binary.AppendUvarint(header(), 1<<40)
		if _, err := Decode(b); !errors.Is(err, ErrTooMany) {
			t.Errorf("got %v, want ErrTooMany", err)
		}
	})

	t.Run("a bundle count past the limit", func(t *testing.T) {
		b := binary.AppendUvarint(header(), 0)
		b = binary.AppendUvarint(b, uint64(MaxBundles+1))
		if _, err := Decode(b); !errors.Is(err, ErrTooMany) {
			t.Errorf("got %v, want ErrTooMany", err)
		}
	})

	t.Run("a length longer than the file", func(t *testing.T) {
		b := binary.AppendUvarint(header(), 0)
		b = binary.AppendUvarint(b, 1)
		b = binary.AppendUvarint(b, 1<<30) // one bundle, a gigabyte long
		if _, err := Decode(b); !errors.Is(err, ErrTruncated) {
			t.Errorf("got %v, want ErrTruncated", err)
		}
	})

	t.Run("a file past the ceiling is refused before parsing", func(t *testing.T) {
		huge := make([]byte, MaxCarrierBytes+1)
		copy(huge, Magic[:])
		if _, err := Decode(huge); !errors.Is(err, ErrTooLarge) {
			t.Errorf("got %v, want ErrTooLarge", err)
		}
	})
}

// One carrier, one wire form — the same rule the record and bundle codecs
// enforce, and for the same reason: two spellings of one thing is how a
// deduplicating system ends up holding both.
func TestDecodeRefusesNonCanonicalInput(t *testing.T) {
	good, err := Encode(testCarrier(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("trailing bytes", func(t *testing.T) {
		if _, err := Decode(append(append([]byte(nil), good...), 0)); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("got %v, want ErrNotCanonical", err)
		}
	})

	t.Run("an overlong uvarint", func(t *testing.T) {
		// The bundle count, re-spelled in two bytes instead of one. binary
		// .Uvarint accepts it; the re-encode check is what does not.
		b := append([]byte{}, Magic[:]...)
		b = append(b, FormatVersion)
		b = append(b, bytes.Repeat([]byte{0}, identity.NodeIDLen)...)
		b = binary.BigEndian.AppendUint32(b, 0)
		b = append(b, 0x80, 0x00) // vector count 0, overlong
		b = append(b, 0x00)       // bundle count 0
		if _, err := Decode(b); !errors.Is(err, ErrNotCanonical) && !errors.Is(err, ErrTruncated) {
			t.Errorf("got %v, want a refusal", err)
		}
	})

	t.Run("one area twice", func(t *testing.T) {
		area := record.AreaTagFor("general")
		vec := testVector(t, 2).Encode()
		b := append([]byte{}, Magic[:]...)
		b = append(b, FormatVersion)
		b = append(b, bytes.Repeat([]byte{0}, identity.NodeIDLen)...)
		b = binary.BigEndian.AppendUint32(b, 0)
		b = binary.AppendUvarint(b, 2)
		for i := 0; i < 2; i++ {
			b = append(b, area[:]...)
			b = binary.AppendUvarint(b, uint64(len(vec)))
			b = append(b, vec...)
		}
		b = binary.AppendUvarint(b, 0)
		if _, err := Decode(b); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("got %v, want ErrNotCanonical", err)
		}
	})
}

// An empty bundle is not a thing a writer produces, and accepting one would put
// a zero-length entry into the log path where it means nothing.
func TestEmptyBundlesAreRefused(t *testing.T) {
	c := testCarrier(t)
	c.Bundles = [][]byte{{}}
	if _, err := Encode(c); err == nil {
		t.Error("encoded a carrier holding an empty bundle")
	}
}

// A carrier with no records is legitimate: it is how a node says "here is what
// I have, send me the difference" on the outward leg of a two-trip exchange.
func TestAVectorOnlyCarrierIsValid(t *testing.T) {
	c := testCarrier(t)
	c.Bundles = nil
	enc, err := Encode(c)
	if err != nil {
		t.Fatalf("a request-only carrier was refused: %v", err)
	}
	got, err := Decode(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bundles) != 0 {
		t.Errorf("got %d bundles", len(got.Bundles))
	}
	if len(got.Vectors) != len(c.Vectors) {
		t.Errorf("the vectors did not survive: %d", len(got.Vectors))
	}
}

// The decoder must not alias its input, or a caller holding bundles after the
// buffer is reused reads whatever replaced them.
func TestDecodeCopiesBundles(t *testing.T) {
	enc, err := Encode(testCarrier(t))
	if err != nil {
		t.Fatal(err)
	}
	buf := append([]byte(nil), enc...)
	c, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), c.Bundles[0]...)

	for i := range buf {
		buf[i] = 0xFF
	}
	if !bytes.Equal(c.Bundles[0], before) {
		t.Error("a decoded bundle aliased the input buffer")
	}
}

func FuzzDecode(f *testing.F) {
	if enc, err := Encode(&Carrier{Vectors: map[record.AreaTag]*vv.Vector{}}); err == nil {
		f.Add(enc)
	}
	f.Add([]byte{})
	f.Add(Magic[:])
	f.Add(append(Magic[:], FormatVersion))

	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := Decode(data)
		if err != nil {
			return
		}
		// Anything that parses must be the canonical encoding of itself.
		again, err := Encode(c)
		if err != nil {
			t.Fatalf("a decoded carrier would not re-encode: %v", err)
		}
		if !bytes.Equal(again, data) {
			t.Fatalf("re-encoding changed %x into %x", data, again)
		}
		// And the bounds hold on anything accepted.
		if len(c.Vectors) > MaxVectors || len(c.Bundles) > MaxBundles {
			t.Fatalf("accepted %d vectors and %d bundles", len(c.Vectors), len(c.Bundles))
		}
	})
}
