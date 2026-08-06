package webd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/aghman/meshbbs/internal/theme"
)

// Serving the configured theme as CSS custom properties (webui.md §11).
//
// This is what makes themes/*.toml retheming ([N5]) reach the browser with no
// second mechanism and no second file format: the same eight colour fields the
// terminal reads become the same eight variables the stylesheet reads.
//
// Only the variables are generated. Everything about layout stays in the static
// stylesheet, which keeps this to a table of colours rather than a templating
// engine, and keeps [D15]'s boundary intact — themes are style overrides, not
// screen templates.

// handleThemeCSS serves the configured theme's variables.
func (s *Server) handleThemeCSS(w http.ResponseWriter, r *http.Request) {
	th := s.opts.Themes.Get(s.opts.ThemeName)

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	// The theme is fixed for the process, and a sysop editing a theme file
	// restarts to pick it up (§11.3's hot reload does not extend here yet), so
	// a short cache is safe and keeps a reload from re-fetching it.
	w.Header().Set("Cache-Control", "max-age=60")
	_, _ = w.Write([]byte(themeCSS(th)))
}

// themeCSS renders a theme as `:root` blocks for both colour schemes.
//
// This file is served AFTER app.css, so these declarations win. That order is
// load-bearing: both define the same variables at the same specificity, and an
// earlier theme.css would simply be overwritten by the stylesheet's defaults.
//
// The light-mode block is generated here rather than expressed in CSS because
// the correction needs the actual colour. An earlier attempt wrote
// `--fg: color-mix(in srgb, var(--fg) 55%, black)` in a media query, which is a
// custom property referring to itself — a cycle, invalid at computed-value
// time, and silently a no-op. Doing the arithmetic in Go has no such trap and
// produces a better answer besides: it darkens only what is actually illegible.
func themeCSS(t theme.Theme) string {
	p := t.Palette()

	// A theme carries no background: on a terminal that belongs to the user's
	// own configuration, which is exactly why the BBS looks at home there. The
	// browser supplies one, so only the theme's own colours appear here.
	//
	// Only colours that MEAN something are taken from the theme: an error is
	// red, a heading is the theme's blue, and those read as themselves against
	// any background.
	//
	// The theme's `text` and `highlight` are deliberately absent. They are not
	// hues, they are "whatever contrasts with the background" — every built-in
	// sets text to some near-white, because a terminal is dark — and the
	// background is the one thing a theme does not supply. Foreground and
	// background are a pair, and splitting ownership of a pair is how this file
	// first shipped body text that was invisible in light mode: theme.css loads
	// after app.css, so an unconditional --fg here beat the stylesheet's own
	// light-mode value. The stylesheet owns both halves.
	vars := [][2]string{
		{"--heading", p.Primary},
		{"--heading-alt", p.Secondary},
		{"--accent", p.Accent},
		{"--muted", p.Muted},
		{"--error", p.Danger},
		{"--success", p.Success},
	}

	var b strings.Builder
	fmt.Fprintf(&b, "/* Generated from theme %q. Edit themes/*.toml, not this. */\n", t.Name)

	writeBlock := func(correct func(string) string) {
		b.WriteString(":root {\n")
		for _, v := range vars {
			// An unresolvable colour is skipped rather than emitted empty, so
			// the stylesheet's own default survives instead of the variable
			// resolving to nothing and text rendering black on black.
			if v[1] == "" {
				continue
			}
			fmt.Fprintf(&b, "  %s: %s;\n", v[0], correct(v[1]))
		}
		fmt.Fprintf(&b, "  --rule-style: %s;\n", t.BorderCSS())
		b.WriteString("}\n")
	}

	writeBlock(func(c string) string { return c })

	b.WriteString("\n@media (prefers-color-scheme: light) {\n")
	writeBlock(theme.ForLightBackground)
	b.WriteString("}\n")

	return b.String()
}
