package theme

import "testing"

func TestHexConvertsAnsiIndices(t *testing.T) {
	cases := map[string]string{
		"0":   "#000000", // black
		"7":   "#e5e5e5", // light grey
		"12":  "#5c5cff", // bright blue, "classic" primary
		"15":  "#ffffff", // white
		"16":  "#000000", // first cube entry
		"68":  "#5f87d7", // "muted" primary
		"231": "#ffffff", // last cube entry
		"232": "#080808", // darkest grey
		"255": "#eeeeee", // lightest grey
	}
	for spec, want := range cases {
		if got := Hex(spec); got != want {
			t.Errorf("Hex(%q) = %q, want %q", spec, got, want)
		}
	}
}

func TestHexPassesLiteralsThrough(t *testing.T) {
	for _, spec := range []string{"#5f87ff", "#ABC"} {
		if got := Hex(spec); got == "" {
			t.Errorf("Hex(%q) returned empty", spec)
		}
	}
}

// TestHexRefusesNonsense — an unresolvable colour must yield empty so the
// renderer keeps its own default. Guessing would paint text a colour nobody
// chose, and black-on-black is a real way for that to end.
func TestHexRefusesNonsense(t *testing.T) {
	for _, spec := range []string{"", "256", "-1", "blue", "#12345", "12.5"} {
		if got := Hex(spec); got != "" {
			t.Errorf("Hex(%q) = %q, want empty", spec, got)
		}
	}
}

// TestEveryBuiltinResolves — a built-in theme that cannot be rendered in a
// browser would be a theme the web silently ignores.
func TestEveryBuiltinResolves(t *testing.T) {
	for _, th := range Builtins() {
		p := th.Palette()
		for name, v := range map[string]string{
			"primary": p.Primary, "secondary": p.Secondary, "accent": p.Accent,
			"muted": p.Muted, "danger": p.Danger, "success": p.Success,
			"text": p.Text, "highlight": p.Highlight,
		} {
			if v == "" {
				t.Errorf("theme %q: %s did not resolve", th.Name, name)
			}
		}
		if th.BorderCSS() == "" {
			t.Errorf("theme %q: no border style", th.Name)
		}
	}
}

// TestForLightBackgroundFixesTerminalBrights is the problem this exists for:
// ANSI 11 is bright yellow, unreadable on white at about 1.07:1.
func TestForLightBackgroundFixesTerminalBrights(t *testing.T) {
	for _, spec := range []string{"11", "10", "14", "15"} {
		hex := Hex(spec)
		fixed := ForLightBackground(hex)
		if fixed == hex {
			t.Errorf("ANSI %s (%s) was left unreadable on white", spec, hex)
		}
		if got := contrastOnWhite(fixed); got < 4.5 {
			t.Errorf("ANSI %s corrected to %s, contrast %.2f, want >= 4.5", spec, fixed, got)
		}
	}
}

// TestForLightBackgroundLeavesReadableColours — correcting a colour that is
// already fine would darken every theme for no reason.
func TestForLightBackgroundLeavesReadableColours(t *testing.T) {
	for _, hex := range []string{"#000000", "#23262e", "#b5303a", Hex("4")} {
		if got := ForLightBackground(hex); got != hex {
			t.Errorf("ForLightBackground(%s) = %s, want it unchanged", hex, got)
		}
	}
}

// TestForLightBackgroundKeepsTheHue — a theme should stay recognisably itself
// rather than sliding toward grey.
func TestForLightBackgroundKeepsTheHue(t *testing.T) {
	// A saturated orange, the sort a sysop would pick.
	fixed := ForLightBackground("#ff8c42")
	r, g, b, ok := rgb(fixed)
	if !ok {
		t.Fatalf("unparseable result %q", fixed)
	}
	if !(r > g && g > b) {
		t.Errorf("orange became %s (r=%v g=%v b=%v); channel order should survive", fixed, r, g, b)
	}
}

// TestEveryBuiltinIsReadableInLightMode — a shipped theme that is illegible in
// a browser's light mode is a theme the sysop cannot safely select.
func TestEveryBuiltinIsReadableInLightMode(t *testing.T) {
	for _, th := range Builtins() {
		p := th.Palette()
		for name, hex := range map[string]string{
			"primary": p.Primary, "accent": p.Accent, "text": p.Text,
			"danger": p.Danger, "success": p.Success, "muted": p.Muted,
		} {
			fixed := ForLightBackground(hex)
			if got := contrastOnWhite(fixed); got < 4.5 {
				t.Errorf("theme %q %s: %s -> %s, contrast %.2f", th.Name, name, hex, fixed, got)
			}
		}
	}
}

func contrastOnWhite(hex string) float64 {
	return 1.05 / (luminance(hex) + 0.05)
}
