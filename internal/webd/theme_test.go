package webd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/theme"
)

// TestThemeCSSCarriesTheConfiguredTheme is the [N5] promise reaching the
// browser: the same eight fields the terminal reads, as CSS variables.
func TestThemeCSSCarriesTheConfiguredTheme(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/theme.css", nil, "")

	if w.Code != http.StatusOK {
		t.Fatalf("GET /theme.css = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q", ct)
	}

	css := w.Body.String()
	for _, want := range []string{"--accent:", "--heading:", "--muted:", "--rule-style:"} {
		if !strings.Contains(css, want) {
			t.Errorf("theme CSS is missing %s:\n%s", want, css)
		}
	}
	// "classic" is the default, and its accent is ANSI 11 — bright yellow.
	if !strings.Contains(css, theme.Hex("11")) {
		t.Errorf("theme CSS does not carry the classic accent %s:\n%s", theme.Hex("11"), css)
	}
}

// TestThemeCSSSkipsUnresolvableColours — emitting an empty variable would beat
// the stylesheet's own fallback and leave text with no colour at all.
func TestThemeCSSSkipsUnresolvableColours(t *testing.T) {
	css := themeCSS(theme.Theme{
		Name: "broken", Primary: "12", Accent: "not-a-colour", Text: "7",
	})
	if strings.Contains(css, "--accent:") {
		t.Errorf("an unresolvable colour was emitted:\n%s", css)
	}
	if !strings.Contains(css, "--heading:") {
		t.Errorf("a resolvable colour was dropped:\n%s", css)
	}
	for _, line := range strings.Split(css, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), ": ;") {
			t.Errorf("empty variable emitted: %q", line)
		}
	}
}

// TestEveryBuiltinThemeServes — a sysop selecting any shipped theme should get
// a browser that looks like it.
func TestEveryBuiltinThemeServes(t *testing.T) {
	for _, th := range theme.Builtins() {
		css := themeCSS(th)
		if !strings.Contains(css, ":root {") {
			t.Errorf("theme %q produced no root block", th.Name)
		}
		if !strings.Contains(css, "--accent:") {
			t.Errorf("theme %q produced no accent", th.Name)
		}
	}
}

// TestThemeCSSNeverSetsForegroundOrBackground.
//
// Foreground and background are a pair, and the stylesheet owns both. This file
// once emitted the theme's `text` — a near-white, because a terminal is dark —
// unconditionally, and since theme.css loads AFTER app.css it beat the
// stylesheet's light-mode value and left body text invisible on white. A theme
// contributes hues, not the contrast pair.
func TestThemeCSSNeverSetsForegroundOrBackground(t *testing.T) {
	css := themeCSS(theme.Theme{
		Name: "pale", Primary: "#ff8c42", Text: "#f2e9e4", Highlight: "#ffffff",
		Muted: "#8d7b68", Border: "single",
	})

	for _, forbidden := range []string{"--fg:", "--bg:", "--highlight:"} {
		if strings.Contains(css, forbidden) {
			t.Errorf("theme CSS sets %s, which the stylesheet owns:\n%s", forbidden, css)
		}
	}

	_, light, ok := strings.Cut(css, "@media (prefers-color-scheme: light)")
	if !ok {
		t.Fatalf("no light-mode block:\n%s", css)
	}
	// Hued colours do cross over, corrected for contrast.
	if !strings.Contains(light, "--heading:") {
		t.Error("light mode dropped the theme's heading colour")
	}
	if strings.Contains(light, "#ff8c42") {
		t.Error("light mode used the uncorrected heading colour")
	}
}
