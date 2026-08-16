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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/aghman/meshbbs/internal/governor"
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
	Telnet  Telnet  `toml:"telnet" doc:"The legacy plaintext front end, off by default ([D12])."`
	Web     Web     `toml:"web" doc:"The browser front end, off by default (webui.md [D16])."`
	Users   Users   `toml:"users" doc:"Registration and account policy (§6.7)."`
	Theme   Theme   `toml:"theme" doc:"Appearance (§5.4)."`
	Mesh    Mesh    `toml:"mesh" doc:"The Meshtastic link and the airtime governor (§7.1, §7.6)."`
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

// Web configures the browser front end (webui.md).
//
// Off by default like telnet, but for the opposite reason: telnet is dangerous
// when on, whereas this simply needs a hostname and a certificate that only the
// sysop can supply.
type Web struct {
	Enabled bool   `toml:"enabled" default:"false" doc:"Serve the browser front end. Off by default: it needs a public origin and a TLS certificate, which have no sensible defaults."`
	Bind    string `toml:"bind" default:"0.0.0.0" doc:"Address to listen on."`
	Port    int    `toml:"port" default:"8443" doc:"Port to listen on. 443 is conventional but needs root."`

	// Origin has no default ON PURPOSE. A passkey is bound to an origin, so a
	// wrong value here does not degrade — it fails totally, with a browser
	// error that says nothing about the cause. Better to refuse to start.
	Origin string `toml:"origin" default:"" doc:"Public origin browsers reach this BBS at, e.g. https://bbs.example.com. REQUIRED when enabled and has no default: passkeys are bound to it, and a mismatch fails every sign-in with an error that does not say why (webui.md §7.1)."`

	TLSCert string `toml:"tls_cert" default:"" doc:"Path to the TLS certificate chain. Required when enabled unless bound to loopback, which browsers treat as a secure context."`
	TLSKey  string `toml:"tls_key" default:"" doc:"Path to the TLS private key."`

	MaxSessions        int `toml:"max_sessions" default:"64" doc:"Maximum concurrent browser sessions. A public listener with no cap is a file-descriptor exhaustion away from taking SSH down with it."`
	MaxSessionsPerUser int `toml:"max_sessions_per_user" default:"8" doc:"Maximum concurrent browser sessions for one account."`

	IdleTimeoutMins int `toml:"idle_timeout_mins" default:"30" doc:"Disconnect a browser session idle this long."`
	// A closing tab is a less reliable goodbye than an SSH connection ending —
	// a killed tab, a locked phone and a sleeping laptop may send nothing at
	// all — so for a session holding a mail passphrase in memory the timer is
	// doing real security work rather than tidying up (webui.md §9).
	UnlockedIdleTimeoutMins int `toml:"unlocked_idle_timeout_mins" default:"10" doc:"Disconnect a session that has unlocked mail after this much idleness. Shorter than idle_timeout_mins on purpose: such a session holds the passphrase in memory, and a closing browser tab is a far less reliable goodbye than an SSH disconnect (webui.md §9)."`
	SessionTTLHours         int `toml:"session_ttl_hours" default:"12" doc:"Absolute lifetime of a browser session, however active it is."`

	EnrolmentCodeTTLMins int `toml:"enrolment_code_ttl_mins" default:"10" doc:"How long a passkey-enrolment code stays valid ([D18]). It is read off a terminal and typed into a browser on the same desk, so minutes are generous."`

	// The unauthenticated endpoints are the ones a stranger can reach, and each
	// attempt costs a database lookup and a hash. These are ceilings on that
	// work, not the thing making a 64-bit code unguessable.
	EnrolAttemptsPerHour int `toml:"enrol_attempts_per_hour" default:"10" doc:"Passkey-enrolment code attempts allowed per client per hour. A person typing a code off their SSH session needs two or three; a script guessing codes needs far more."`

	AuthAttemptsPerHour int `toml:"auth_attempts_per_hour" default:"60" doc:"Sign-in attempts allowed per client per hour. Looser than enrolment: a passkey prompt that the user dismisses costs an attempt, and that is a normal thing to do more than once."`

	// Without this, X-Forwarded-For is ignored entirely — a client sets that
	// header, so honouring it unconditionally means an attacker sends a fresh
	// value per request and the limiter counts each separately, which is worse
	// than no limiter because it looks like protection.
	TrustedProxies []string `toml:"trusted_proxies" default:"" doc:"IPs or CIDRs of reverse proxies allowed to report the real client via X-Forwarded-For. EMPTY BY DEFAULT, which means the header is ignored and every request is attributed to whatever address it arrived from — correct when nothing sits in front, and it collapses per-client rate limits into one shared allowance when something does. Set this only for proxies you run: any address listed here can claim to be any client."`
}

// Telnet configures the legacy plaintext front end ([D12]).
//
// Off by default. Enabling it puts credentials on the wire in the clear; the
// server warns at every start, and only guest access is served.
type Telnet struct {
	Enabled     bool   `toml:"enabled" default:"false" doc:"Serve telnet. OFF by default: telnet is plaintext, so anything typed can be read by anyone on the network path ([D12])."`
	Bind        string `toml:"bind" default:"0.0.0.0" doc:"Address to listen on."`
	Port        int    `toml:"port" default:"2323" doc:"Port to listen on. 23 is conventional but needs root."`
	MaxSessions int    `toml:"max_sessions" default:"16" doc:"Maximum concurrent telnet sessions. This is a public plaintext port, so it is capped."`
	GuestOnly   bool   `toml:"guest_only" default:"true" doc:"Serve read-only guest sessions only. Recommended: browsing over plaintext costs nothing, typing a password over it does."`
}

// Users configures registration policy (§6.7, [N7], [N9]).
type Users struct {
	RegistrationMode string `toml:"registration_mode" default:"open" doc:"open, approval, invite, or closed. Default 'open' with federated posting withheld: the door is open, the shared airtime is gated ([N7])."`
	GuestEnabled     bool   `toml:"guest_enabled" default:"true" doc:"Allow anonymous read-only access via ssh guest@."`
	DirectoryListed  bool   `toml:"default_directory_listed" default:"true" doc:"Whether new users are listed in the network directory ([N9])."`
	// SessionTimeLimitMins is §11.5's session_time_limit, with the unit in the
	// name as the web keys already do — a bare number of unstated units is the
	// one thing a config reference cannot fix afterwards.
	SessionTimeLimitMins int `toml:"session_time_limit_mins" default:"0" doc:"End a session after this many minutes. 0 means no limit. Sysops are never timed out: the limit shares lines between callers, and the operator is not competing for one."`
}

// Theme configures appearance (§5.4, [D15], [N5]).
type Theme struct {
	Default  string `toml:"default" default:"classic" doc:"Built-in or file theme name, applied to every session on this instance. 'meshbbs serve' prints the available names at startup; there is no per-user theme picker in the interface."`
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

// Mesh configures the radio link and the airtime governor (§11.5, Phase 3).
//
// Off by default. A BBS with no radio is a complete BBS — Phase 1 shipped one —
// and turning this on commits the instance to transmitting on a shared band, so
// it is a decision a sysop makes rather than one they inherit.
//
// Deliberately absent: the channel PSK, the port number and the channel index.
// §11.5 lists them, but meshbbs never writes radio configuration — the sysop
// owns their node, sets the channel up in the Meshtastic app, and we resolve it
// by NAME because indices are a local arrangement that differs between radios.
type Mesh struct {
	Enabled bool `toml:"enabled" default:"false" doc:"Federate over a Meshtastic radio. Off by default: enabling it transmits on a shared band."`

	Mode         string `toml:"mode" default:"auto" doc:"How to reach the radio: 'serial', 'tcp', or 'auto' (try the configured serial device or auto-detect, then fall back to tcp_host)."`
	SerialDevice string `toml:"serial_device" default:"" doc:"Serial port, e.g. /dev/ttyUSB0 or COM3. Empty auto-detects; run 'meshbbs mesh ports' to see candidates."`
	SerialBaud   int    `toml:"serial_baud" default:"115200" doc:"Serial baud rate. Every current firmware uses 115200."`
	TCPHost      string `toml:"tcp_host" default:"" doc:"Host of a node on WiFi. Port defaults to 4403 if not given."`

	ChannelName string `toml:"channel_name" default:"bbsnet" doc:"Name of the Meshtastic channel carrying BBS traffic (§7.1). Create it in the Meshtastic app as a secondary channel with the same name and key on every instance."`
	RxTimeoutS  int    `toml:"rx_timeout_secs" default:"300" doc:"Reconnect if the radio has sent nothing for this many seconds. A USB serial handle can go one-way — writes keep succeeding and transmissions go out while nothing is ever received — and only silence reveals it. Zero disables the check. Below about 60 a busy-but-quiet radio may be reconnected needlessly, and each reconnect re-announces."`
	HopLimit    int    `toml:"hop_limit" default:"0" doc:"Hop limit for BBS packets, 0-7. Zero uses the radio's own setting. Hop limit multiplies what every packet costs the mesh (§1.1), so set it as low as your topology allows."`

	AirtimeCeilingPct       float64 `toml:"airtime_ceiling_pct" default:"5" doc:"Share of the channel the WHOLE BBS network should use, as a percentage. Divided by expected_instance_count to get this node's allowance. Clamped to 15 in code (§7.6)."`
	ExpectedInstanceCount   int     `toml:"expected_instance_count" default:"50" doc:"How many instances divide the ceiling. The design plans for 50 ([D2])."`
	FloodMultiplier         float64 `toml:"flood_multiplier" default:"4" doc:"R: how many times the mesh rebroadcasts each packet. Every airtime figure scales linearly with it, and 4 is a GUESS — run 'meshbbs mesh survey' to measure yours (§7.8)."`
	FloodMultiplierOverride bool    `toml:"flood_multiplier_override" default:"false" doc:"Pin flood_multiplier and disable live refinement. Testing only: it stops the node correcting a value that is too low."`
	QuietHours              string  `toml:"quiet_hours" default:"" doc:"Comma-separated local-time windows of zero transmission, e.g. '22:00-06:00'. Windows may wrap midnight."`

	DoorEventBatchWindow string `toml:"door_event_batch_window" default:"auto" doc:"How long a partial batch of door-league events waits before it is sent (§9.5). 'auto' derives it from what the area's airtime share actually buys, so a measured flood multiplier propagates without a config edit. An explicit Go duration like '30m' overrides, and is clamped to 5m-12h."`
	DoorEventMaxAge      string `toml:"door_event_max_age" default:"24h" doc:"How long a queued door-league event may wait before it is dropped unsent (§9.5). Generous on purpose: it exists to stop a node that has been offline for a week spending its whole budget on a game that finished, not to tighten latency."`

	HamModeOverride string `toml:"ham_mode_override" default:"" doc:"Set to 'i_accept_part97_responsibility' to transmit encrypted traffic while the radio reports a licensed operator. FCC Part 97 prohibits obscuring the meaning of amateur transmissions; the licence at risk is yours (§8.3)."`
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
	FilesDir string `toml:"files_dir" default:"files" doc:"Directory holding file areas, served over SFTP. Relative to data_dir unless absolute."`
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

	if _, _, err := c.DoorEventWindow(); err != nil {
		problems = append(problems, err.Error())
	}
	if _, err := c.DoorEventMaxAge(); err != nil {
		problems = append(problems, err.Error())
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

	problems = append(problems, c.validateWeb()...)

	if c.SSH.Port < 1 || c.SSH.Port > 65535 {
		problems = append(problems, fmt.Sprintf("ssh.port is %d, want 1-65535", c.SSH.Port))
	}
	if c.SSH.MaxSessions < 1 {
		problems = append(problems, "ssh.max_sessions must be at least 1")
	}
	if _, err := c.Location(); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Telnet.Enabled {
		if c.Telnet.Port < 1 || c.Telnet.Port > 65535 {
			problems = append(problems, fmt.Sprintf("telnet.port is %d, want 1-65535", c.Telnet.Port))
		}
		if c.Telnet.Port == c.SSH.Port {
			problems = append(problems, "telnet.port and ssh.port are the same")
		}
		if c.Telnet.MaxSessions < 1 {
			problems = append(problems, "telnet.max_sessions must be at least 1")
		}
	}

	if c.Mesh.Enabled {
		switch c.Mesh.Mode {
		case "serial", "tcp", "auto":
		default:
			problems = append(problems, fmt.Sprintf(
				"mesh.mode is %q, want serial, tcp, or auto", c.Mesh.Mode))
		}
		if c.Mesh.Mode == "tcp" && strings.TrimSpace(c.Mesh.TCPHost) == "" {
			problems = append(problems, "mesh.mode is \"tcp\" but mesh.tcp_host is empty")
		}
		if strings.TrimSpace(c.Mesh.ChannelName) == "" {
			problems = append(problems, "mesh.channel_name must not be empty")
		}
		if c.Mesh.HopLimit < 0 || c.Mesh.HopLimit > 7 {
			problems = append(problems, fmt.Sprintf(
				"mesh.hop_limit is %d, want 0-7", c.Mesh.HopLimit))
		}
		if c.Mesh.SerialBaud < 1200 {
			problems = append(problems, fmt.Sprintf("mesh.serial_baud is %d", c.Mesh.SerialBaud))
		}
		// A floor rather than a free hand: every reconnect re-announces, so a
		// value of a few seconds would spend airtime saying hello instead of
		// federating. Zero is the way to turn the check off.
		if c.Mesh.RxTimeoutS != 0 && c.Mesh.RxTimeoutS < 30 {
			problems = append(problems, fmt.Sprintf(
				"mesh.rx_timeout_secs is %d, want 0 (disabled) or at least 30", c.Mesh.RxTimeoutS))
		}
		// The ceiling is clamped rather than rejected in the governor, but a
		// sysop who typed 60 should hear about it here rather than discover
		// months later that it never took effect.
		if c.Mesh.AirtimeCeilingPct <= 0 || c.Mesh.AirtimeCeilingPct > MaxAirtimeCeilingPct {
			problems = append(problems, fmt.Sprintf(
				"mesh.airtime_ceiling_pct is %g, want a positive value up to %g — "+
					"the ceiling is what the whole BBS network takes from other people's mesh (§7.6)",
				c.Mesh.AirtimeCeilingPct, MaxAirtimeCeilingPct))
		}
		if c.Mesh.ExpectedInstanceCount < 1 {
			problems = append(problems, "mesh.expected_instance_count must be at least 1")
		}
		if c.Mesh.FloodMultiplier < 1 {
			problems = append(problems, fmt.Sprintf(
				"mesh.flood_multiplier is %g, want at least 1 — our own transmission always happens",
				c.Mesh.FloodMultiplier))
		}
		if _, err := c.QuietHourWindows(); err != nil {
			problems = append(problems, err.Error())
		}
		if o := strings.TrimSpace(c.Mesh.HamModeOverride); o != "" && o != HamModeOverridePhrase {
			problems = append(problems, fmt.Sprintf(
				"mesh.ham_mode_override is %q; it only takes effect when set to exactly %q",
				o, HamModeOverridePhrase))
		}
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

// MaxAirtimeCeilingPct is the hard limit on the mesh-wide share (§7.6).
const MaxAirtimeCeilingPct = 15.0

// HamModeOverridePhrase is the exact value that disables the Part 97 block.
//
// A phrase rather than a boolean so that nobody sets it while skimming: writing
// it out is the acknowledgement (§8.3).
const HamModeOverridePhrase = "i_accept_part97_responsibility"

// AcceptsPart97Responsibility reports whether the override is properly set.
// validateWeb checks the browser front end's settings (webui.md §7.1, §10).
//
// Every problem here is one that fails at RUN time in a way that does not say
// what is wrong: a passkey against a mismatched origin produces a bare browser
// error, and WebAuthn outside a secure context simply does not exist as an API.
// Catching them at startup is the difference between a config typo and an
// afternoon.
func (c *Config) validateWeb() []string {
	if !c.Web.Enabled {
		return nil
	}
	var problems []string

	if c.Web.Port < 1 || c.Web.Port > 65535 {
		problems = append(problems, fmt.Sprintf("web.port is %d, want 1-65535", c.Web.Port))
	}
	if c.Web.MaxSessions < 1 {
		problems = append(problems, "web.max_sessions must be at least 1")
	}

	switch {
	case c.Web.Origin == "":
		problems = append(problems, "web.origin is required when web.enabled is true: passkeys are "+
			"bound to an origin, so there is no safe default (e.g. https://bbs.example.com)")
	default:
		u, err := url.Parse(c.Web.Origin)
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("web.origin %q is not a URL: %v", c.Web.Origin, err))
		case u.Scheme != "https" && !isLoopbackHost(u.Hostname()):
			// Browsers make localhost a secure context so a sysop can try the
			// thing out without a certificate. Everywhere else, http means
			// WebAuthn is simply absent.
			problems = append(problems, fmt.Sprintf(
				"web.origin %q must be https: browsers refuse WebAuthn outside a secure context, "+
					"so passkeys would not work at all (localhost is the only exception)", c.Web.Origin))
		case u.Path != "" && u.Path != "/":
			problems = append(problems, fmt.Sprintf(
				"web.origin %q must be a bare origin with no path", c.Web.Origin))
		}
	}

	// A certificate is required unless the listener is loopback-only, where a
	// browser grants a secure context without one.
	if !isLoopbackHost(c.Web.Bind) {
		if c.Web.TLSCert == "" || c.Web.TLSKey == "" {
			problems = append(problems, "web.tls_cert and web.tls_key are required when serving "+
				"on a non-loopback address: the web front end is TLS-only")
		}
	}
	if (c.Web.TLSCert == "") != (c.Web.TLSKey == "") {
		problems = append(problems, "web.tls_cert and web.tls_key must be set together")
	}

	if c.Users.SessionTimeLimitMins < 0 {
		problems = append(problems,
			"users.session_time_limit_mins cannot be negative; use 0 for no limit")
	}

	if c.Web.EnrolmentCodeTTLMins < 1 {
		problems = append(problems, "web.enrolment_code_ttl_mins must be at least 1")
	}
	if c.Web.SessionTTLHours < 1 {
		problems = append(problems, "web.session_ttl_hours must be at least 1")
	}
	if c.Web.EnrolAttemptsPerHour < 1 {
		problems = append(problems, "web.enrol_attempts_per_hour must be at least 1: "+
			"zero would lock everyone out of passkey enrolment, not harden it")
	}
	if c.Web.AuthAttemptsPerHour < 1 {
		problems = append(problems, "web.auth_attempts_per_hour must be at least 1: "+
			"zero would lock everyone out of signing in")
	}

	// A malformed entry here fails OPEN — the address matches nothing, the
	// header is ignored, and rate limiting quietly attributes every request to
	// the proxy. Better to refuse to start than to run in a state the sysop
	// believes is per-client and is not.
	for _, p := range c.Web.TrustedProxies {
		if _, err := parseProxyRange(p); err != nil {
			problems = append(problems, fmt.Sprintf(
				"web.trusted_proxies entry %q is not an IP or CIDR: %v", p, err))
		}
	}
	return problems
}

// parseProxyRange accepts either a bare IP or a CIDR block.
//
// Exported behaviour lives in webd; this is here so a bad entry is caught at
// startup by the same validation that catches every other config mistake
// (§11.3), rather than at the first request.
func parseProxyRange(s string) (*net.IPNet, error) {
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		return n, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("not an IP address")
	}
	// A bare address is a /32 or /128.
	bits := 8 * net.IPv6len
	if ip.To4() != nil {
		ip, bits = ip.To4(), 8*net.IPv4len
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

// TrustedProxyRanges returns the parsed proxy allow list. Validate has already
// rejected anything malformed, so errors here are ignored rather than surfaced
// twice.
func (c *Config) TrustedProxyRanges() []*net.IPNet {
	out := make([]*net.IPNet, 0, len(c.Web.TrustedProxies))
	for _, p := range c.Web.TrustedProxies {
		if n, err := parseProxyRange(p); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (c *Config) AcceptsPart97Responsibility() bool {
	return strings.TrimSpace(c.Mesh.HamModeOverride) == HamModeOverridePhrase
}

// DoorEventWindow parses mesh.door_event_batch_window.
//
// Returns ok=false for "auto", which is not an error — it is the caller being
// told to derive the window from the governor rather than being handed one.
func (c *Config) DoorEventWindow() (d time.Duration, ok bool, err error) {
	raw := strings.TrimSpace(c.Mesh.DoorEventBatchWindow)
	if raw == "" || strings.EqualFold(raw, "auto") {
		return 0, false, nil
	}
	d, err = time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("mesh.door_event_batch_window %q is not a duration like 30m (or \"auto\"): %w", raw, err)
	}
	if d <= 0 {
		return 0, false, fmt.Errorf("mesh.door_event_batch_window must be positive, got %s", d)
	}
	return d, true, nil
}

// DoorEventMaxAge parses mesh.door_event_max_age.
func (c *Config) DoorEventMaxAge() (time.Duration, error) {
	raw := strings.TrimSpace(c.Mesh.DoorEventMaxAge)
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("mesh.door_event_max_age %q is not a duration like 24h: %w", raw, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("mesh.door_event_max_age must be positive, got %s", d)
	}
	return d, nil
}

// QuietHourWindows parses mesh.quiet_hours into governor windows.
//
// The format is what a sysop would write without reading documentation:
// "22:00-06:00, 13:00-13:30". A window whose end precedes its start wraps
// midnight, because "quiet overnight" is the case this exists for.
func (c *Config) QuietHourWindows() ([]governor.Window, error) {
	raw := strings.TrimSpace(c.Mesh.QuietHours)
	if raw == "" {
		return nil, nil
	}
	var out []governor.Window
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		halves := strings.SplitN(part, "-", 2)
		if len(halves) != 2 {
			return nil, fmt.Errorf("mesh.quiet_hours entry %q is not a range like 22:00-06:00", part)
		}
		start, err := parseClock(strings.TrimSpace(halves[0]))
		if err != nil {
			return nil, fmt.Errorf("mesh.quiet_hours entry %q: %w", part, err)
		}
		end, err := parseClock(strings.TrimSpace(halves[1]))
		if err != nil {
			return nil, fmt.Errorf("mesh.quiet_hours entry %q: %w", part, err)
		}
		if start == end {
			return nil, fmt.Errorf("mesh.quiet_hours entry %q starts and ends at the same time", part)
		}
		out = append(out, governor.Window{Start: start, End: end})
	}
	return out, nil
}

func parseClock(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a 24-hour time like 22:00", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
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

// Location resolves node.timezone into a *time.Location.
//
// Every timestamp the BBS renders goes through this. Without it, formatting
// falls back to the host's local zone, which makes the same message read
// differently depending on where the server happens to run — and makes any
// test asserting on a rendered time machine-dependent.
func (c *Config) Location() (*time.Location, error) {
	name := strings.TrimSpace(c.Node.Timezone)
	if name == "" || name == "Local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("node.timezone %q is not a known IANA timezone: %w", name, err)
	}
	return loc, nil
}

// FilesPath returns the absolute path to the file area directory.
func (c *Config) FilesPath() (string, error) {
	return c.resolveUnder(c.Storage.FilesDir)
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
