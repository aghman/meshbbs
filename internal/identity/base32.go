package identity

import (
	"fmt"
	"strings"
)

// Crockford base32 alphabet: no I, L, O or U. Excluding I/L/O removes the
// classic 1/l/I and 0/O confusions; excluding U makes accidental profanity in
// a random 13-character string far less likely (§6.1.4.2).
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var crockfordDecode = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	for i, c := range crockfordAlphabet {
		t[c] = int8(i)
		t[lowerOf(byte(c))] = int8(i)
	}
	// Crockford's forgiving decode: these are accepted as their visual
	// equivalents so that a human transcribing an ID by ear or by eye is not
	// punished for the obvious substitutions.
	t['I'], t['i'], t['L'], t['l'] = 1, 1, 1, 1
	t['O'], t['o'] = 0, 0
	return t
}()

func lowerOf(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

// base32Len returns the number of characters needed to encode n bytes.
func base32Len(n int) int { return (n*8 + 4) / 5 }

// EncodeBase32 renders b in Crockford base32, most-significant bit first, with
// no padding. 8 bytes produces 13 characters.
func EncodeBase32(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	out := make([]byte, 0, base32Len(len(b)))
	var acc uint16 // holds up to 13 pending bits
	var bits uint
	for _, c := range b {
		acc = acc<<8 | uint16(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out = append(out, crockfordAlphabet[(acc>>bits)&0x1f])
		}
	}
	if bits > 0 {
		// Left-align the remaining bits in the final symbol.
		out = append(out, crockfordAlphabet[(acc<<(5-bits))&0x1f])
	}
	return string(out)
}

// DecodeBase32 parses a Crockford base32 string. Hyphens and spaces are
// ignored, so both grouped and ungrouped forms are accepted, and decoding is
// case-insensitive.
func DecodeBase32(s string) ([]byte, error) {
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == ' ' || c == '\t' {
			continue
		}
		if crockfordDecode[c] < 0 {
			return nil, fmt.Errorf("invalid base32 character %q", string(rune(c)))
		}
		clean = append(clean, c)
	}
	if len(clean) == 0 {
		return nil, fmt.Errorf("no base32 characters")
	}

	n := len(clean) * 5 / 8
	out := make([]byte, 0, n)
	var acc uint16
	var bits uint
	for _, c := range clean {
		acc = acc<<5 | uint16(crockfordDecode[c])
		bits += 5
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	// Any leftover bits are the left-aligned padding of the final symbol and
	// must be zero; a non-zero remainder means the string is not a canonical
	// encoding of a whole number of bytes.
	if bits > 0 && acc&((1<<bits)-1) != 0 {
		return nil, fmt.Errorf("non-canonical base32: %d stray bits set", bits)
	}
	return out, nil
}

// Group inserts hyphens for readability: 13 characters become 4-4-5.
func Group(s string) string {
	if len(s) != 13 {
		return s
	}
	var b strings.Builder
	b.Grow(15)
	b.WriteString(s[0:4])
	b.WriteByte('-')
	b.WriteString(s[4:8])
	b.WriteByte('-')
	b.WriteString(s[8:13])
	return b.String()
}
