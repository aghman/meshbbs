package theme

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinsAreValid(t *testing.T) {
	for _, th := range Builtins() {
		if err := th.Validate(); err != nil {
			t.Errorf("built-in theme %q is invalid: %v", th.Name, err)
		}
		if th.Description == "" {
			t.Errorf("built-in theme %q has no description", th.Name)
		}
	}
}

func TestLoadWithoutADirectory(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has(DefaultName) {
		t.Fatalf("default theme %q missing", DefaultName)
	}
	if len(s.Names()) != len(Builtins()) {
		t.Fatalf("got %d themes, want %d", len(s.Names()), len(Builtins()))
	}
}

// [N5]: a sysop can drop a file in and retheme without a rebuild.
func TestFileThemeIsLoaded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sunset.toml"),
		[]byte("description = \"warm\"\nprimary = \"208\"\ntext = \"223\"\nmuted = \"240\"\nborder = \"single\"\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Has("sunset") {
		t.Fatal("file theme was not loaded")
	}
	if got := s.Get("sunset"); got.Primary != "208" {
		t.Fatalf("primary is %q, want 208", got.Primary)
	}
}

// A file may override a built-in and inherit the fields it does not set.
func TestFileThemeInheritsFromBuiltin(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "classic.toml"),
		[]byte("accent = \"200\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get("classic")
	if got.Accent != "200" {
		t.Fatalf("override did not apply: accent is %q", got.Accent)
	}
	builtin := Builtins()[0]
	if got.Primary != builtin.Primary {
		t.Fatalf("unset field was not inherited: primary is %q, want %q", got.Primary, builtin.Primary)
	}
}

// §11.3's rule applies here too: a malformed theme is a startup error, not a
// silent fallback. A sysop who typos a key should be told.
func TestMalformedThemeIsAnError(t *testing.T) {
	cases := map[string]string{
		"unknown key":  "primary = \"1\"\ntext=\"7\"\nmuted=\"8\"\ncolour = \"blue\"\n",
		"bad border":   "primary = \"1\"\ntext=\"7\"\nmuted=\"8\"\nborder = \"triple\"\n",
		"broken toml":  "primary = \n",
		"empty colour": "primary = \"\"\ntext=\"7\"\nmuted=\"8\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "broken.toml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatal("Load accepted a malformed theme")
			}
		})
	}
}

func TestUnknownThemeFallsBackToDefault(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get("no-such-theme"); got.Name != DefaultName {
		t.Fatalf("fallback returned %q, want %q", got.Name, DefaultName)
	}
}

// §5.4: a CP437 client must not be sent Unicode box-drawing characters, no
// matter what the theme asked for.
func TestLegacyClientsGetASCIIBorders(t *testing.T) {
	th := Builtins()[0] // classic uses double borders
	if th.Border != "double" {
		t.Fatalf("test assumes the classic theme uses double borders, got %q", th.Border)
	}

	unicodeStyles := th.Styles(true)
	if unicodeStyles.Border.TopLeft != "╔" {
		t.Fatalf("unicode client got border %q, want ╔", unicodeStyles.Border.TopLeft)
	}

	legacyStyles := th.Styles(false)
	if legacyStyles.Border.TopLeft != "+" {
		t.Fatalf("legacy client got border %q, want +", legacyStyles.Border.TopLeft)
	}
	for _, s := range []string{
		legacyStyles.Border.Top, legacyStyles.Border.Left,
		legacyStyles.Border.TopLeft, legacyStyles.Border.BottomRight,
	} {
		for _, r := range s {
			if r > 127 {
				t.Fatalf("legacy border contains a non-ASCII rune %U", r)
			}
		}
	}
}

func TestNamesAreSorted(t *testing.T) {
	s, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	names := s.Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("theme names are not sorted: %v", names)
		}
	}
}

func TestWriteExampleProducesALoadableTheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "example.toml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("the example theme does not load: %v", err)
	}
	if !s.Has("example") {
		t.Fatal("the example theme was not picked up")
	}
	body, _ := os.ReadFile(path)
	if !strings.Contains(string(body), "#") {
		t.Error("the example has no explanatory comments")
	}
}
