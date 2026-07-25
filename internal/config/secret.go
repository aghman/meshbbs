package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Secret is a configuration value that must not sit in config.toml as a
// literal (§11.2). The Meshtastic channel PSK is the motivating case.
//
// Three forms are accepted:
//
//	env:MESHBBS_MESH_PSK    read from the environment
//	file:keys/mesh.psk      read from a file, which must be mode 0600
//	base64:...              a literal — permitted, but warned about at startup
//
// A bare value with no prefix is also treated as a literal, so that a sysop who
// has not read the docs still gets a working system and a warning rather than a
// cryptic parse error.
type Secret string

// IsSet reports whether a value was configured at all.
func (s Secret) IsSet() bool { return strings.TrimSpace(string(s)) != "" }

// IsLiteral reports whether the value is stored inline rather than referenced.
// Callers use this to emit the §11.2 startup warning.
func (s Secret) IsLiteral() bool {
	v := strings.TrimSpace(string(s))
	if v == "" {
		return false
	}
	return !strings.HasPrefix(v, "env:") && !strings.HasPrefix(v, "file:")
}

// Resolve returns the secret's bytes.
//
// The returned slice is freshly allocated; callers that hold it for a long time
// should zero it when done.
func (s Secret) Resolve() ([]byte, error) {
	v := strings.TrimSpace(string(s))
	switch {
	case v == "":
		return nil, nil

	case strings.HasPrefix(v, "env:"):
		name := strings.TrimPrefix(v, "env:")
		raw, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("secret references environment variable %s, which is not set", name)
		}
		if raw == "" {
			return nil, fmt.Errorf("environment variable %s is set but empty", name)
		}
		return []byte(raw), nil

	case strings.HasPrefix(v, "file:"):
		path := strings.TrimPrefix(v, "file:")
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("secret file %s: %w", path, err)
		}
		// A secret readable by other local users is not a secret. Windows does
		// not express this in a mode we can check meaningfully.
		if runtime.GOOS != "windows" {
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				return nil, fmt.Errorf(
					"secret file %s has permissions %04o; it must not be readable by other users (chmod 600 %s)",
					path, perm, path)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading secret file %s: %w", path, err)
		}
		return []byte(strings.TrimSpace(string(raw))), nil

	case strings.HasPrefix(v, "base64:"):
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "base64:"))
		if err != nil {
			return nil, fmt.Errorf("secret is not valid base64: %w", err)
		}
		return raw, nil

	default:
		return []byte(v), nil
	}
}
