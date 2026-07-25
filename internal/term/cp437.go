// Package term handles terminal capability detection and CP437 translation
// (design §5.4).
//
// BBS aesthetics mean CP437 and ANSI art, but modern terminals are UTF-8. The
// two cannot be served by the same bytes: a CP437 0xC9 is a double box corner
// to SyncTERM and an invalid UTF-8 sequence to anything else.
package term

import (
	"strings"
	"unicode/utf8"
)

// cp437 maps the high half of code page 437 to Unicode. The low 128 are ASCII
// and pass through unchanged.
//
// This table is a display contract: changing an entry changes how existing art
// renders, so entries are only ever added, never edited.
var cp437 = [128]rune{
	0x00C7, 0x00FC, 0x00E9, 0x00E2, 0x00E4, 0x00E0, 0x00E5, 0x00E7,
	0x00EA, 0x00EB, 0x00E8, 0x00EF, 0x00EE, 0x00EC, 0x00C4, 0x00C5,
	0x00C9, 0x00E6, 0x00C6, 0x00F4, 0x00F6, 0x00F2, 0x00FB, 0x00F9,
	0x00FF, 0x00D6, 0x00DC, 0x00A2, 0x00A3, 0x00A5, 0x20A7, 0x0192,
	0x00E1, 0x00ED, 0x00F3, 0x00FA, 0x00F1, 0x00D1, 0x00AA, 0x00BA,
	0x00BF, 0x2310, 0x00AC, 0x00BD, 0x00BC, 0x00A1, 0x00AB, 0x00BB,
	0x2591, 0x2592, 0x2593, 0x2502, 0x2524, 0x2561, 0x2562, 0x2556,
	0x2555, 0x2563, 0x2551, 0x2557, 0x255D, 0x255C, 0x255B, 0x2510,
	0x2514, 0x2534, 0x252C, 0x251C, 0x2500, 0x253C, 0x255E, 0x255F,
	0x255A, 0x2554, 0x2569, 0x2566, 0x2560, 0x2550, 0x256C, 0x2567,
	0x2568, 0x2564, 0x2565, 0x2559, 0x2558, 0x2552, 0x2553, 0x256B,
	0x256A, 0x2518, 0x250C, 0x2588, 0x2584, 0x258C, 0x2590, 0x2580,
	0x03B1, 0x00DF, 0x0393, 0x03C0, 0x03A3, 0x03C3, 0x00B5, 0x03C4,
	0x03A6, 0x0398, 0x03A9, 0x03B4, 0x221E, 0x03C6, 0x03B5, 0x2229,
	0x2261, 0x00B1, 0x2265, 0x2264, 0x2320, 0x2321, 0x00F7, 0x2248,
	0x00B0, 0x2219, 0x00B7, 0x221A, 0x207F, 0x00B2, 0x25A0, 0x00A0,
}

// reverse maps Unicode back to CP437 bytes, built once.
var reverse = func() map[rune]byte {
	m := make(map[rune]byte, 128)
	for i, r := range cp437 {
		m[r] = byte(i + 128)
	}
	return m
}()

// Encoding is how a client wants its bytes.
type Encoding int

const (
	// EncodingUTF8 is a modern terminal.
	EncodingUTF8 Encoding = iota
	// EncodingCP437 is a legacy BBS terminal (SyncTERM, NetRunner, MagiTerm).
	EncodingCP437
)

func (e Encoding) String() string {
	if e == EncodingCP437 {
		return "cp437"
	}
	return "utf8"
}

// ToUnicode converts CP437 bytes to a UTF-8 string, for display on a modern
// terminal. This is the path stored ANSI art takes on its way to a UTF-8
// client.
func ToUnicode(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b) * 2)
	for _, c := range b {
		if c < 128 {
			sb.WriteByte(c)
			continue
		}
		sb.WriteRune(cp437[c-128])
	}
	return sb.String()
}

// ToCP437 converts a UTF-8 string to CP437 bytes, for a legacy client.
//
// Characters with no CP437 equivalent become '?' rather than being dropped:
// silently losing characters makes text subtly wrong in ways users cannot
// diagnose, whereas a '?' is visibly a substitution.
func ToCP437(s string) []byte {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r < 128:
			out = append(out, byte(r))
		default:
			if b, ok := reverse[r]; ok {
				out = append(out, b)
			} else {
				out = append(out, '?')
			}
		}
	}
	return out
}

// Encode renders a UTF-8 string for the target encoding.
func Encode(s string, enc Encoding) []byte {
	if enc == EncodingCP437 {
		return ToCP437(s)
	}
	return []byte(s)
}

// DetectEncoding guesses what a client wants from its environment (§5.4).
//
// This is a guess, and it is wrong often enough that the result must be
// overridable by a per-user preference — which is why signup offers the
// choice rather than trusting this.
func DetectEncoding(termType string, env map[string]string) Encoding {
	// An explicit UTF-8 locale is the strongest signal available.
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v, ok := env[key]; ok && v != "" {
			if strings.Contains(strings.ToUpper(v), "UTF-8") ||
				strings.Contains(strings.ToUpper(v), "UTF8") {
				return EncodingUTF8
			}
			// A locale that is explicitly not UTF-8 suggests a legacy client.
			return EncodingCP437
		}
	}

	// Terminal types that only legacy BBS clients announce.
	switch strings.ToLower(termType) {
	case "ansi", "ansi-bbs", "syncterm", "pcansi", "avatar":
		return EncodingCP437
	}

	// SSH clients from modern systems are overwhelmingly UTF-8.
	return EncodingUTF8
}

// SanitizeForDisplay strips control characters from untrusted text before it
// reaches a terminal.
//
// Display names, subjects and post bodies all arrive from other people, and on
// a federated BBS from other people's BBSes. Without this, an escape sequence
// in a subject line can reposition the cursor, recolour the screen, or in some
// terminals trigger a response the client sends back as input.
//
// Newlines and tabs survive because message bodies need them.
func SanitizeForDisplay(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "?")
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			sb.WriteRune(r)
		case r == '\r':
			// Normalise CRLF to LF; a bare CR would overwrite the line.
			continue
		case r < 0x20 || r == 0x7f:
			// Includes ESC (0x1b), which is the actual attack.
			continue
		case r >= 0x80 && r <= 0x9f:
			// C1 controls: some terminals treat these as escape equivalents.
			continue
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// SanitizeLine is SanitizeForDisplay for single-line fields, which must not
// contain newlines either.
func SanitizeLine(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(SanitizeForDisplay(s), "\n", " "), "\t", " ")
}
