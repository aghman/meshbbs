// Package theme provides BBS colour schemes (design §5.4, [D15] [N5]).
//
// # Scope
//
// These are STYLE overrides: colours, box-drawing glyphs, accent characters.
// They are deliberately NOT ANSI art theme packs with screen templates,
// layout manifests and per-menu artwork — that is what [D15] declined, and
// keeping the boundary here is what keeps the loader about fifty lines instead
// of a subsystem.
//
// Colour choices live behind this struct rather than being hardcoded at call
// sites. That costs nothing now and is the difference between extending themes
// later being a weekend and being a rewrite.
package theme

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/charmbracelet/lipgloss"
)

// Theme is a complete colour and glyph scheme.
type Theme struct {
	Name        string `toml:"name"`
	Description string `toml:"description"`

	// Colours are ANSI colour names or numbers ("4", "12", "#5f87ff").
	// Numbers below 16 keep classic BBS clients happy.
	Primary   string `toml:"primary"`
	Secondary string `toml:"secondary"`
	Accent    string `toml:"accent"`
	Muted     string `toml:"muted"`
	Danger    string `toml:"danger"`
	Success   string `toml:"success"`
	Text      string `toml:"text"`
	Highlight string `toml:"highlight"`

	// Border selects the box-drawing set: "single", "double", or "ascii".
	Border string `toml:"border"`
}

// Styles is a Theme resolved into lipgloss styles, built once per session.
type Styles struct {
	Title     lipgloss.Style
	Heading   lipgloss.Style
	Body      lipgloss.Style
	Muted     lipgloss.Style
	Accent    lipgloss.Style
	Error     lipgloss.Style
	Success   lipgloss.Style
	Selected  lipgloss.Style
	Border    lipgloss.Border
	StatusBar lipgloss.Style
}

// Builtins are the themes compiled into the binary ([D15]).
func Builtins() []Theme {
	return []Theme{
		{
			Name:        "classic",
			Description: "Classic 16-colour ANSI, the way a BBS should look",
			Primary:     "12", Secondary: "14", Accent: "11", Muted: "8",
			Danger: "9", Success: "10", Text: "7", Highlight: "15",
			Border: "double",
		},
		{
			Name:        "muted",
			Description: "Softer 256-colour scheme for modern terminals",
			Primary:     "68", Secondary: "72", Accent: "179", Muted: "244",
			Danger: "167", Success: "108", Text: "252", Highlight: "255",
			Border: "single",
		},
		{
			Name:        "contrast",
			Description: "High contrast, for readability and low-vision users",
			Primary:     "15", Secondary: "15", Accent: "11", Muted: "7",
			Danger: "9", Success: "10", Text: "15", Highlight: "0",
			Border: "single",
		},
		{
			Name:        "mono",
			Description: "Monochrome, for serial links and stubborn terminals",
			Primary:     "7", Secondary: "7", Accent: "15", Muted: "8",
			Danger: "7", Success: "7", Text: "7", Highlight: "15",
			Border: "ascii",
		},
	}
}

// DefaultName is the theme used when none is configured.
const DefaultName = "classic"

// Set is the themes available to a running BBS.
type Set struct {
	byName map[string]Theme
	names  []string
}

// Load builds the theme set: built-ins first, then any *.toml overrides from
// dir merged on top ([N5]).
//
// A malformed theme file is an ERROR, not a silent fallback — same rule as
// config (§11.3). A sysop who typos a colour should be told at startup, not
// left wondering why the BBS looks wrong.
func Load(dir string) (*Set, error) {
	s := &Set{byName: map[string]Theme{}}
	for _, t := range Builtins() {
		s.byName[t.Name] = t
	}

	if dir != "" {
		matches, err := filepath.Glob(filepath.Join(dir, "*.toml"))
		if err != nil {
			return nil, fmt.Errorf("scan theme directory: %w", err)
		}
		// Sort so that load order — and therefore which of two files defining
		// the same theme name wins — is deterministic rather than filesystem
		// dependent.
		sort.Strings(matches)

		for _, path := range matches {
			t, err := loadFile(path, s.byName)
			if err != nil {
				return nil, err
			}
			s.byName[t.Name] = t
		}
	}

	for name := range s.byName {
		s.names = append(s.names, name)
	}
	sort.Strings(s.names)
	return s, nil
}

// loadFile reads one theme file. A file may override a built-in by name, in
// which case unspecified fields inherit from it.
func loadFile(path string, existing map[string]Theme) (Theme, error) {
	base := strings.TrimSuffix(filepath.Base(path), ".toml")

	var t Theme
	if prior, ok := existing[base]; ok {
		t = prior // inherit, so a file may set only the fields it cares about
	} else {
		t = existing[DefaultName]
	}
	t.Name = base

	md, err := toml.DecodeFile(path, &t)
	if err != nil {
		return Theme{}, fmt.Errorf("theme %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return Theme{}, fmt.Errorf("theme %s: unknown key(s): %s", path, strings.Join(keys, ", "))
	}
	if t.Name == "" {
		t.Name = base
	}
	if err := t.Validate(); err != nil {
		return Theme{}, fmt.Errorf("theme %s: %w", path, err)
	}
	return t, nil
}

// Validate checks a theme is usable.
func (t Theme) Validate() error {
	switch t.Border {
	case "single", "double", "ascii", "":
	default:
		return fmt.Errorf("border is %q, want single, double or ascii", t.Border)
	}
	for name, v := range map[string]string{
		"primary": t.Primary, "text": t.Text, "muted": t.Muted,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%s colour is empty", name)
		}
	}
	return nil
}

// Names returns the available theme names, sorted.
func (s *Set) Names() []string { return append([]string(nil), s.names...) }

// Get returns a theme by name, falling back to the default.
func (s *Set) Get(name string) Theme {
	if t, ok := s.byName[name]; ok {
		return t
	}
	return s.byName[DefaultName]
}

// Has reports whether a theme exists.
func (s *Set) Has(name string) bool { _, ok := s.byName[name]; return ok }

// Styles resolves a theme into lipgloss styles.
//
// unicode selects the glyph set: a legacy CP437 client gets ASCII borders
// regardless of what the theme asked for, because box-drawing characters would
// arrive as mojibake (§5.4).
func (t Theme) Styles(unicode bool) Styles {
	border := lipgloss.NormalBorder()
	switch {
	case !unicode || t.Border == "ascii":
		border = lipgloss.Border{
			Top: "-", Bottom: "-", Left: "|", Right: "|",
			TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
		}
	case t.Border == "double":
		border = lipgloss.DoubleBorder()
	}

	c := func(v string) lipgloss.Color { return lipgloss.Color(v) }
	return Styles{
		Title:     lipgloss.NewStyle().Bold(true).Foreground(c(t.Highlight)),
		Heading:   lipgloss.NewStyle().Bold(true).Foreground(c(t.Primary)),
		Body:      lipgloss.NewStyle().Foreground(c(t.Text)),
		Muted:     lipgloss.NewStyle().Foreground(c(t.Muted)),
		Accent:    lipgloss.NewStyle().Foreground(c(t.Accent)),
		Error:     lipgloss.NewStyle().Bold(true).Foreground(c(t.Danger)),
		Success:   lipgloss.NewStyle().Foreground(c(t.Success)),
		Selected:  lipgloss.NewStyle().Bold(true).Foreground(c(t.Highlight)).Background(c(t.Primary)),
		Border:    border,
		StatusBar: lipgloss.NewStyle().Foreground(c(t.Muted)),
	}
}

// WriteExample writes a commented sample theme file, so a sysop has something
// to copy rather than a blank page.
func WriteExample(path string) error {
	body := `# meshbbs theme
#
# Style overrides only: colours, borders, accents. Anything not set here is
# inherited from the built-in theme of the same name, or from "classic".
#
# Colours are ANSI names, numbers, or hex. Numbers below 16 keep classic BBS
# terminal clients happy; 256-colour and hex need a modern terminal.

description = "My BBS colours"

primary   = "12"
secondary = "14"
accent    = "11"
muted     = "8"
danger    = "9"
success   = "10"
text      = "7"
highlight = "15"

# single, double, or ascii
border = "double"
`
	return os.WriteFile(path, []byte(body), 0o644)
}
