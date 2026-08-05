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
	for _, want := range []string{"--accent:", "--heading:", "--muted:", "--fg:", "--rule-style:"} {
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
