package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTOML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// §11.3: an unknown key must be a hard startup error. This is the single most
// important behaviour in this package — it is why Viper is not used (§4).
func TestUnknownKeyIsAnError(t *testing.T) {
	cases := []struct{ name, body string }{
		{"misspelled key", "[node]\ndisplay_nam = \"x\"\n"},
		{"plausible-but-wrong", "[log]\nlevel = \"info\"\nlogformat = \"json\"\n"},
		{"unknown section", "[mesh]\nchannel_name = \"bbsnet\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTOML(t, tc.body))
			if err == nil {
				t.Fatal("expected an error for an unknown key, got nil")
			}
			if !strings.Contains(err.Error(), "unknown configuration key") {
				t.Fatalf("expected an unknown-key error, got: %v", err)
			}
		})
	}
}

func TestDefaultsApply(t *testing.T) {
	c := Default()
	if c.Log.Level != "info" {
		t.Errorf("log.level default is %q, want info", c.Log.Level)
	}
	if c.Log.Format != "text" {
		t.Errorf("log.format default is %q, want text", c.Log.Format)
	}
	if c.Node.Environment != EnvProduction {
		t.Errorf("node.environment default is %q, want production", c.Node.Environment)
	}
	if c.Storage.Database != "bbs.db" {
		t.Errorf("storage.database default is %q, want bbs.db", c.Storage.Database)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
}

// §11.2: an empty path yields working defaults, so the binary is usable before
// `meshbbs init` has written anything.
func TestLoadWithNoFile(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if c.Log.Level != "info" {
		t.Fatalf("expected defaults, got log.level=%q", c.Log.Level)
	}
}

func TestFileOverridesDefault(t *testing.T) {
	c, err := Load(writeTOML(t, "[log]\nlevel = \"debug\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Log.Level != "debug" {
		t.Fatalf("log.level is %q, want debug", c.Log.Level)
	}
	// Untouched keys keep their defaults.
	if c.Log.Format != "text" {
		t.Fatalf("log.format is %q, want the default text", c.Log.Format)
	}
}

// §11.2 precedence: defaults -> file -> env.
func TestEnvOverridesFile(t *testing.T) {
	t.Setenv("MESHBBS_LOG_LEVEL", "warn")
	c, err := Load(writeTOML(t, "[log]\nlevel = \"debug\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Log.Level != "warn" {
		t.Fatalf("log.level is %q, want warn (env must beat file)", c.Log.Level)
	}
}

func TestEnvInvalidValueIsAnError(t *testing.T) {
	t.Setenv("MESHBBS_LOG_LEVEL", "chatty")
	if _, err := Load(""); err == nil {
		t.Fatal("expected validation to reject an invalid log level")
	}
}

func TestValidateRejectsBadValues(t *testing.T) {
	cases := map[string]func(*Config){
		"bad environment": func(c *Config) { c.Node.Environment = "staging" },
		"bad log level":   func(c *Config) { c.Log.Level = "verbose" },
		"bad log format":  func(c *Config) { c.Log.Format = "xml" },
		"empty database":  func(c *Config) { c.Storage.Database = "" },
		"empty keys dir":  func(c *Config) { c.Storage.KeysDir = "" },
		"long name":       func(c *Config) { c.Node.DisplayName = strings.Repeat("x", 33) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			mutate(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected a validation error, got nil")
			}
		})
	}
}

// §11.2: the generated reference must cover every key, since it is the
// document sysops are pointed at when they hit an unknown-key error.
func TestReferenceCoversEveryKey(t *testing.T) {
	ref := Reference()
	if len(ref) == 0 {
		t.Fatal("Reference() returned nothing")
	}
	want := map[string]bool{
		"node.display_name": false, "node.sysop_name": false,
		"node.sysop_contact": false, "node.timezone": false,
		"node.environment": false, "log.level": false, "log.format": false,
		"log.file": false, "storage.data_dir": false,
		"storage.database": false, "storage.keys_dir": false,
	}
	for _, e := range ref {
		if _, ok := want[e.Key]; ok {
			want[e.Key] = true
		}
		if e.Doc == "" {
			t.Errorf("key %s has no doc string; the reference would be useless for it", e.Key)
		}
		if e.Env == "" {
			t.Errorf("key %s has no environment variable name", e.Key)
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("key %s missing from the generated reference", k)
		}
	}
}

func TestSecretResolution(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		t.Setenv("MESHBBS_TEST_PSK", "hunter2")
		got, err := Secret("env:MESHBBS_TEST_PSK").Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "hunter2" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("missing env is an error", func(t *testing.T) {
		if _, err := Secret("env:MESHBBS_DEFINITELY_UNSET").Resolve(); err == nil {
			t.Fatal("expected an error for an unset environment variable")
		}
	})

	t.Run("file", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "psk")
		if err := os.WriteFile(p, []byte("  s3cret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := Secret("file:" + p).Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "s3cret" {
			t.Fatalf("got %q, want the trimmed contents", got)
		}
	})

	t.Run("file must not be world readable", func(t *testing.T) {
		// Secret.Resolve skips this check on Windows, which has no Unix
		// permission bits to inspect.
		if runtime.GOOS == "windows" {
			t.Skip("Unix file permissions are not represented on Windows")
		}
		if os.Getuid() == 0 {
			t.Skip("running as root")
		}
		p := filepath.Join(t.TempDir(), "psk")
		if err := os.WriteFile(p, []byte("s3cret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Secret("file:" + p).Resolve(); err == nil {
			t.Fatal("accepted a world-readable secret file")
		}
	})

	t.Run("base64", func(t *testing.T) {
		got, err := Secret("base64:aGVsbG8=").Resolve()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("literal detection", func(t *testing.T) {
		if !Secret("base64:aGk=").IsLiteral() {
			t.Error("base64: should count as a literal (it is inline)")
		}
		if !Secret("plain").IsLiteral() {
			t.Error("a bare value should count as a literal")
		}
		if Secret("env:X").IsLiteral() {
			t.Error("env: reference should not count as a literal")
		}
		if Secret("").IsSet() {
			t.Error("empty secret should not be considered set")
		}
	})
}
