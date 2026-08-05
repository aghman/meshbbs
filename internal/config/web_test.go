package config

import (
	"strings"
	"testing"
)

// The web front end's settings all fail at RUN time in ways that do not say
// what is wrong — a mismatched origin is a bare browser error, and WebAuthn
// outside a secure context is simply an absent API. These tests are the reason
// a sysop finds out at startup instead.

// enabledWeb returns a config with a valid, minimal web front end.
func enabledWeb() Config {
	c := Default()
	c.Web.Enabled = true
	c.Web.Origin = "https://bbs.example.com"
	c.Web.TLSCert = "/etc/meshbbs/cert.pem"
	c.Web.TLSKey = "/etc/meshbbs/key.pem"
	return c
}

func TestWebDisabledNeedsNothing(t *testing.T) {
	// The default config has no origin and no certificate, and must still be
	// valid — otherwise every existing instance fails to start after upgrading.
	d := Default()
	if err := d.Validate(); err != nil {
		t.Fatalf("the default config must validate with the web front end off: %v", err)
	}
}

func TestWebEnabledValidates(t *testing.T) {
	c := enabledWeb()
	if err := c.Validate(); err != nil {
		t.Fatalf("a correctly configured web front end should validate: %v", err)
	}
}

func TestWebRejectsBadSettings(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		says   string
	}{
		"no origin": {
			func(c *Config) { c.Web.Origin = "" },
			"web.origin is required",
		},
		"http origin": {
			func(c *Config) { c.Web.Origin = "http://bbs.example.com" },
			"secure context",
		},
		"origin with a path": {
			func(c *Config) { c.Web.Origin = "https://bbs.example.com/bbs" },
			"bare origin",
		},
		"no certificate on a public bind": {
			func(c *Config) { c.Web.TLSCert, c.Web.TLSKey = "", "" },
			"tls_cert",
		},
		"half a certificate": {
			func(c *Config) { c.Web.TLSKey = "" },
			"together",
		},
		"impossible port": {
			func(c *Config) { c.Web.Port = 70000 },
			"web.port",
		},
		"no sessions allowed": {
			func(c *Config) { c.Web.MaxSessions = 0 },
			"max_sessions",
		},
		"instant code expiry": {
			func(c *Config) { c.Web.EnrolmentCodeTTLMins = 0 },
			"enrolment_code_ttl_mins",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := enabledWeb()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatal("expected a validation error, got nil")
			}
			// The message has to name the remedy, not just the field: these are
			// read by a sysop who cannot see why their browser said "no".
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("error does not mention %q:\n%v", tc.says, err)
			}
		})
	}
}

// TestWebAllowsLocalhostWithoutTLS keeps the try-it-out path open. Browsers
// treat localhost as a secure context, so demanding a certificate there would
// make the first run harder than it needs to be for no security gain.
func TestWebAllowsLocalhostWithoutTLS(t *testing.T) {
	for _, origin := range []string{"http://localhost:8443", "http://127.0.0.1:8443"} {
		c := Default()
		c.Web.Enabled = true
		c.Web.Bind = "127.0.0.1"
		c.Web.Origin = origin
		if err := c.Validate(); err != nil {
			t.Errorf("loopback origin %q should validate without TLS: %v", origin, err)
		}
	}
}

// TestWebUnlockedTimeoutIsShorter is a policy assertion, not a range check. A
// browser tab closing is a far less reliable goodbye than an SSH disconnect, so
// the timer bounding an in-memory mail passphrase is doing real security work
// (webui.md §9). If a later change makes the defaults equal, that reasoning has
// been quietly dropped.
func TestWebUnlockedTimeoutIsShorter(t *testing.T) {
	c := Default()
	if c.Web.UnlockedIdleTimeoutMins >= c.Web.IdleTimeoutMins {
		t.Errorf("unlocked_idle_timeout_mins (%d) should be shorter than idle_timeout_mins (%d): "+
			"a session holding a mail passphrase deserves a tighter bound",
			c.Web.UnlockedIdleTimeoutMins, c.Web.IdleTimeoutMins)
	}
}
