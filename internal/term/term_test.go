package term

import (
	"strings"
	"testing"
)

func TestASCIIPassesThrough(t *testing.T) {
	s := "Hello, BBS! 0123456789"
	if got := ToUnicode([]byte(s)); got != s {
		t.Fatalf("ASCII changed: %q", got)
	}
	if got := string(ToCP437(s)); got != s {
		t.Fatalf("ASCII changed on the way out: %q", got)
	}
}

// The box-drawing characters are what make BBS art look like BBS art.
func TestBoxDrawingRoundTrip(t *testing.T) {
	// CP437 0xC9 0xCD 0xBB is the top of a double-line box.
	raw := []byte{0xC9, 0xCD, 0xBB}
	unicode := ToUnicode(raw)
	if unicode != "╔═╗" {
		t.Fatalf("got %q, want ╔═╗", unicode)
	}
	back := ToCP437(unicode)
	if string(back) != string(raw) {
		t.Fatalf("round trip changed bytes: %x -> %q -> %x", raw, unicode, back)
	}
}

func TestFullHighRangeRoundTrips(t *testing.T) {
	for i := 128; i < 256; i++ {
		raw := []byte{byte(i)}
		u := ToUnicode(raw)
		back := ToCP437(u)
		if len(back) != 1 || back[0] != byte(i) {
			t.Fatalf("byte 0x%02X did not round-trip: %q -> %x", i, u, back)
		}
	}
}

// Characters with no CP437 equivalent must be visibly substituted rather than
// silently dropped.
func TestUnmappableBecomesQuestionMark(t *testing.T) {
	got := string(ToCP437("emoji: 🎉"))
	if !strings.HasSuffix(got, "?") {
		t.Fatalf("got %q, want a trailing substitution marker", got)
	}
	if strings.Contains(got, "🎉") {
		t.Fatal("emoji survived conversion to CP437")
	}
}

func TestDetectEncoding(t *testing.T) {
	cases := []struct {
		name string
		term string
		env  map[string]string
		want Encoding
	}{
		{"utf8 locale", "xterm-256color", map[string]string{"LANG": "en_US.UTF-8"}, EncodingUTF8},
		{"utf8 via LC_ALL", "xterm", map[string]string{"LC_ALL": "C.UTF-8"}, EncodingUTF8},
		{"non-utf8 locale", "xterm", map[string]string{"LANG": "en_US.ISO-8859-1"}, EncodingCP437},
		{"syncterm", "ansi-bbs", nil, EncodingCP437},
		{"plain ansi", "ansi", nil, EncodingCP437},
		{"modern default", "xterm-256color", nil, EncodingUTF8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectEncoding(tc.term, tc.env); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Text from other users reaches a terminal. An escape sequence in a subject
// line must not be able to drive it.
func TestSanitizeStripsEscapeSequences(t *testing.T) {
	attacks := []string{
		"normal\x1b[2Jcleared the screen",
		"\x1b]0;retitled the window\x07",
		"bell\x07bell",
		"back\x08space",
		"c1 controlhere",
	}
	for _, a := range attacks {
		got := SanitizeForDisplay(a)
		if strings.ContainsRune(got, 0x1b) {
			t.Errorf("ESC survived sanitisation of %q: %q", a, got)
		}
		for _, r := range got {
			if r < 0x20 && r != '\n' && r != '\t' {
				t.Errorf("control character %U survived in %q", r, got)
			}
			if r >= 0x80 && r <= 0x9f {
				t.Errorf("C1 control %U survived in %q", r, got)
			}
		}
	}
}

func TestSanitizeKeepsUsefulWhitespace(t *testing.T) {
	got := SanitizeForDisplay("line one\nline two\tcolumn")
	if got != "line one\nline two\tcolumn" {
		t.Fatalf("useful whitespace was stripped: %q", got)
	}
}

func TestSanitizeNormalisesCarriageReturn(t *testing.T) {
	got := SanitizeForDisplay("windows\r\nline")
	if strings.ContainsRune(got, '\r') {
		t.Fatalf("bare CR survived: %q", got)
	}
	if got != "windows\nline" {
		t.Fatalf("got %q", got)
	}
}

func TestSanitizeLineFlattensNewlines(t *testing.T) {
	got := SanitizeLine("subject\nwith\nnewlines")
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("newlines survived a single-line sanitisation: %q", got)
	}
}

func TestSanitizeHandlesInvalidUTF8(t *testing.T) {
	got := SanitizeForDisplay(string([]byte{0xff, 0xfe, 'o', 'k'}))
	if !strings.HasSuffix(got, "ok") {
		t.Fatalf("valid tail was lost: %q", got)
	}
}

func TestEncodeSelectsTheRightPath(t *testing.T) {
	if string(Encode("╔", EncodingUTF8)) != "╔" {
		t.Fatal("UTF-8 encoding altered the string")
	}
	if b := Encode("╔", EncodingCP437); len(b) != 1 || b[0] != 0xC9 {
		t.Fatalf("CP437 encoding produced %x, want C9", b)
	}
}
