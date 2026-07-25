// Package config implements the configuration layer described in design §11.
//
// Three rules from §11.3 shape this package, and all three are deliberate
// departures from what a typical Go config library does:
//
//  1. An unknown key is a startup ERROR, not a warning and not silence.
//     Silently ignoring `airtime_ceiling_percent` because the real key is
//     `airtime_ceiling_pct` is how a mesh gets flooded. This is also the
//     specific reason Viper is not used (§4).
//  2. Every setting has a working default, so the wizard can write a minimal
//     file rather than a 400-line commented dump (§11.2).
//  3. Secrets are never literals in the file by default: values may be
//     `env:`, `file:` or `base64:` references (§11.2).
//
// Resolution order is defaults -> file -> MESHBBS_* env -> flags (§11.2).
// Flags are applied by the caller after Load, since only the command layer
// knows which flags were actually set.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// Environment gates development-only behaviour (§6.7).
type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvProduction  Environment = "production"
)

// Config is the complete file-layer configuration.
//
// Only settings whose subsystems exist are present. Phase markers in §11.5
// describe what each later phase adds; inventing keys for unbuilt subsystems
// would produce a reference document that lies.
type Config struct {
	Node    Node    `toml:"node" doc:"Instance identity and operating environment."`
	SSH     SSH     `toml:"ssh" doc:"The SSH front end (§5.1)."`
	Users   Users   `toml:"users" doc:"Registration and account policy (§6.7)."`
	Theme   Theme   `toml:"theme" doc:"Appearance (§5.4)."`
	Log     Log     `toml:"log" doc:"Logging and observability."`
	Storage Storage `toml:"storage" doc:"Database and on-disk layout."`
}

// SSH configures the front end (§11.5, Phase 1).
type SSH struct {
	Enabled     bool   `toml:"enabled" default:"true" doc:"Serve SSH."`
	Bind        string `toml:"bind" default:"0.0.0.0" doc:"Address to listen on."`
	Port        int    `toml:"port" default:"2222" doc:"Port to listen on. 2222 avoids needing root; 22 is conventional for a dedicated host."`
	MaxSessions int    `toml:"max_sessions" default:"32" doc:"Maximum concurrent sessions."`
}

// Users configures registration policy (§6.7, [N7], [N9]).
type Users struct {
	RegistrationMode string `toml:"registration_mode" default:"open" doc:"open, approval, invite, or closed. Default 'open' with federated posting withheld: the door is open, the shared airtime is gated ([N7])."`
	GuestEnabled     bool   `toml:"guest_enabled" default:"true" doc:"Allow anonymous read-only access via ssh guest@."`
	DirectoryListed  bool   `toml:"default_directory_listed" default:"true" doc:"Whether new users are listed in the network directory ([N9])."`
}

// Theme configures appearance (§5.4, [D15], [N5]).
type Theme struct {
	Default  string `toml:"default" default:"classic" doc:"Built-in or file theme name. Run the BBS and press N for the list."`
	Dir      string `toml:"dir" default:"themes" doc:"Directory scanned for *.toml style overrides, relative to data_dir unless absolute ([N5])."`
	Encoding string `toml:"default_encoding" default:"auto" doc:"auto, utf8, or cp437. 'auto' guesses from the client's locale and terminal type."`
}

// Node is the instance's own identity (§11.5, Phase 0).
//
// There is deliberately no node_id key: the ID is derived from
// keys/node.ed25519 and is read-only everywhere (§6.1.1). `meshbbs id` prints
// it.
type Node struct {
	DisplayName  string      `toml:"display_name" default:"meshbbs" doc:"Self-declared label published in this node's NODE record. Not unique, not authoritative, never used for routing (§6.1.4)."`
	SysopName    string      `toml:"sysop_name" default:"" doc:"Sysop's name, for display."`
	SysopContact string      `toml:"sysop_contact" default:"" doc:"Free-text contact address for the sysop, published in the NODE record."`
	Timezone     string      `toml:"timezone" default:"Local" doc:"IANA timezone name used for display. Wall-clock time is advisory (§6.2.1)."`
	Environment  Environment `toml:"environment" default:"production" doc:"'development' or 'production'. The dev subcommands refuse to run against a production datadir (§6.7)."`
}

// Log configures slog (§11.5, Phase 0).
type Log struct {
	Level  string `toml:"level" default:"info" doc:"debug, info, warn, or error."`
	Format string `toml:"format" default:"text" doc:"text or json."`
	File   string `toml:"file" default:"" doc:"Path to a log file. Empty logs to stderr."`
}

// Storage configures the database and data layout (§11.5, Phase 0).
type Storage struct {
	DataDir  string `toml:"data_dir" default:"" doc:"Root data directory. Empty selects the OS convention (~/.local/share/meshbbs, ~/Library/Application Support/MeshBBS, %APPDATA%\\MeshBBS)."`
	Database string `toml:"database" default:"bbs.db" doc:"SQLite database filename, relative to data_dir unless absolute."`
	KeysDir  string `toml:"keys_dir" default:"keys" doc:"Directory holding node and host keys, relative to data_dir unless absolute. Must be mode 0700 with 0600 keys."`
}

// Default returns a Config with every field set to its documented default.
func Default() Config {
	var c Config
	applyDefaults(&c)
	return c
}

// Load reads a config file, applies env overrides, and validates the result.
//
// If path is empty, defaults plus env overrides are returned — which is what
// makes `meshbbs` usable before `meshbbs init` has written anything.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		md, err := toml.DecodeFile(path, &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("reading %s: %w", path, err)
		}
		// §11.3: unknown keys are errors. This is the whole reason for
		// decoding with BurntSushi/toml rather than something more forgiving.
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, 0, len(undecoded))
			for _, k := range undecoded {
				keys = append(keys, k.String())
			}
			return Config{}, fmt.Errorf(
				"%s: unknown configuration key(s): %s\n"+
					"(run `meshbbs config reference` for the full list of valid keys)",
				path, strings.Join(keys, ", "))
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks cross-field constraints. §11.3 puts these in one place
// rather than scattering them, because that is where the ham-mode checks and
// airtime ceilings will land in later phases.
func (c *Config) Validate() error {
	var problems []string

	switch c.Node.Environment {
	case EnvDevelopment, EnvProduction:
	default:
		problems = append(problems, fmt.Sprintf(
			"node.environment is %q, want %q or %q", c.Node.Environment, EnvDevelopment, EnvProduction))
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		problems = append(problems, fmt.Sprintf(
			"log.level is %q, want debug, info, warn, or error", c.Log.Level))
	}

	switch c.Log.Format {
	case "text", "json":
	default:
		problems = append(problems, fmt.Sprintf("log.format is %q, want text or json", c.Log.Format))
	}

	switch c.Users.RegistrationMode {
	case "open", "approval", "invite", "closed":
	default:
		problems = append(problems, fmt.Sprintf(
			"users.registration_mode is %q, want open, approval, invite, or closed",
			c.Users.RegistrationMode))
	}

	switch c.Theme.Encoding {
	case "auto", "utf8", "cp437":
	default:
		problems = append(problems, fmt.Sprintf(
			"theme.default_encoding is %q, want auto, utf8, or cp437", c.Theme.Encoding))
	}

	if c.SSH.Port < 1 || c.SSH.Port > 65535 {
		problems = append(problems, fmt.Sprintf("ssh.port is %d, want 1-65535", c.SSH.Port))
	}
	if c.SSH.MaxSessions < 1 {
		problems = append(problems, "ssh.max_sessions must be at least 1")
	}

	if strings.TrimSpace(c.Storage.Database) == "" {
		problems = append(problems, "storage.database must not be empty")
	}
	if strings.TrimSpace(c.Storage.KeysDir) == "" {
		problems = append(problems, "storage.keys_dir must not be empty")
	}
	if len(c.Node.DisplayName) > 32 {
		problems = append(problems, fmt.Sprintf(
			"node.display_name is %d bytes, limit is 32 (it travels in the NODE record)",
			len(c.Node.DisplayName)))
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// ResolvedDataDir returns the data directory, falling back to the OS
// convention when unset (§10).
func (c *Config) ResolvedDataDir() (string, error) {
	if c.Storage.DataDir != "" {
		return expandPath(c.Storage.DataDir)
	}
	return defaultDataDir()
}

// DatabasePath returns the absolute path to the SQLite database.
func (c *Config) DatabasePath() (string, error) {
	return c.resolveUnder(c.Storage.Database)
}

// KeysPath returns the absolute path to the keys directory.
func (c *Config) KeysPath() (string, error) {
	return c.resolveUnder(c.Storage.KeysDir)
}

// ThemePath returns the absolute path to the theme override directory.
func (c *Config) ThemePath() (string, error) {
	return c.resolveUnder(c.Theme.Dir)
}

func (c *Config) resolveUnder(p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	dir, err := c.ResolvedDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, p), nil
}

func expandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", p, err)
		}
		return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
	}
	return filepath.Abs(p)
}

func defaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "MeshBBS"), nil
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "MeshBBS"), nil
		}
		return filepath.Join(home, "AppData", "Roaming", "MeshBBS"), nil
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "meshbbs"), nil
		}
		return filepath.Join(home, ".local", "share", "meshbbs"), nil
	}
}

// DefaultConfigPath returns where config.toml lives for a given data dir.
func DefaultConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "config.toml")
}
