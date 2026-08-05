package theme

import (
	"fmt"
	"strconv"
	"strings"
)

// Resolving a theme's colours to concrete hex, for renderers that cannot use
// ANSI indices (webui.md §11).
//
// A terminal looks up index 12 in whatever palette the user configured, which
// is the point — a BBS inherits the look of the terminal it is displayed in. A
// browser has no such palette, so somebody has to choose. Using the widely
// implemented xterm defaults keeps the browser recognisably the same theme as
// the terminal, which is the whole reason themes/*.toml reaches the web at all
// ([N5]).

// Palette is a theme's colours resolved to `#rrggbb`.
type Palette struct {
	Primary   string
	Secondary string
	Accent    string
	Muted     string
	Danger    string
	Success   string
	Text      string
	Highlight string
}

// Palette resolves every colour on the theme.
func (t Theme) Palette() Palette {
	return Palette{
		Primary:   Hex(t.Primary),
		Secondary: Hex(t.Secondary),
		Accent:    Hex(t.Accent),
		Muted:     Hex(t.Muted),
		Danger:    Hex(t.Danger),
		Success:   Hex(t.Success),
		Text:      Hex(t.Text),
		Highlight: Hex(t.Highlight),
	}
}

// ansi16 is the standard low sixteen, in xterm's rendering.
//
// These are the ones a terminal is most likely to have re-mapped to taste, so
// they are the least faithful part of this conversion — and still far closer
// than picking arbitrary colours would be.
var ansi16 = [16]string{
	"#000000", "#cd0000", "#00cd00", "#cdcd00",
	"#0000ee", "#cd00cd", "#00cdcd", "#e5e5e5",
	"#7f7f7f", "#ff0000", "#00ff00", "#ffff00",
	"#5c5cff", "#ff00ff", "#00ffff", "#ffffff",
}

// Hex converts a theme colour spec to `#rrggbb`.
//
// Accepts what the theme format accepts: an ANSI index 0-255, or a literal hex
// value passed through. An unrecognised value yields empty rather than a
// guess — a renderer can then fall back to its own default instead of showing
// a colour nobody chose.
func Hex(spec string) string {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return ""
	}
	if strings.HasPrefix(spec, "#") {
		if len(spec) == 7 || len(spec) == 4 {
			return strings.ToLower(spec)
		}
		return ""
	}

	n, err := strconv.Atoi(spec)
	if err != nil || n < 0 || n > 255 {
		return ""
	}
	switch {
	case n < 16:
		return ansi16[n]
	case n < 232:
		// The 6x6x6 cube. Levels are not evenly spaced: the first step is to
		// 0x5f, and the rest are 40 apart.
		n -= 16
		return fmt.Sprintf("#%02x%02x%02x",
			cubeLevel(n/36), cubeLevel((n/6)%6), cubeLevel(n%6))
	default:
		// The 24-step grey ramp, from #080808 to #eeeeee.
		v := 8 + (n-232)*10
		return fmt.Sprintf("#%02x%02x%02x", v, v, v)
	}
}

func cubeLevel(i int) int {
	if i == 0 {
		return 0
	}
	return 55 + i*40
}

// BorderCSS renders the theme's box-drawing choice as a CSS border style, so a
// theme stays visually distinct on the web rather than collapsing to one line
// everywhere.
func (t Theme) BorderCSS() string {
	switch t.Border {
	case "double":
		return "3px double"
	case "ascii":
		return "1px dashed"
	default:
		return "1px solid"
	}
}
