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
