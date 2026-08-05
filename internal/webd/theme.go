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

// themeCSS renders a theme as a `:root` block.
func themeCSS(t theme.Theme) string {
	p := t.Palette()

	// A theme carries no background: on a terminal that belongs to the user's
	// own configuration, which is exactly why the BBS looks at home there. The
	// browser has to supply one, so the stylesheet's own light/dark defaults
	// stand and only the theme's own colours are overridden here.
	vars := [][2]string{
		{"--heading", p.Primary},
		{"--heading-alt", p.Secondary},
		{"--accent", p.Accent},
		{"--muted", p.Muted},
		{"--error", p.Danger},
		{"--success", p.Success},
		{"--fg", p.Text},
		{"--highlight", p.Highlight},
		{"--rule-style", t.BorderCSS()},
	}

	var b strings.Builder
	fmt.Fprintf(&b, "/* Generated from theme %q. Edit themes/*.toml, not this. */\n", t.Name)
	b.WriteString(":root {\n")
	for _, v := range vars {
		// An unresolvable colour is skipped rather than emitted empty, so the
		// stylesheet's own default survives instead of the variable resolving
		// to nothing and the text rendering black on black.
		if v[1] == "" {
			continue
		}
		fmt.Fprintf(&b, "  %s: %s;\n", v[0], v[1])
	}
	b.WriteString("}\n")
	return b.String()
}
