package dmkey

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/aghman/meshbbs/internal/keyring"
)

// ErrNoKeyFile is returned when the key file does not exist.
var ErrNoKeyFile = errors.New("no DM key file")

// ErrKeyFileExists is returned rather than overwriting one.
//
// Overwriting a DM key destroys every message ever sent to it, permanently and
// with no error at the time — the mail is still there and is now ciphertext
// nobody can open. `init` therefore refuses rather than prompting, because a
// prompt is a thing people say yes to.
var ErrKeyFileExists = errors.New("a DM key file already exists")

// DefaultPath is where the key lives when the user does not say.
//
// os.UserConfigDir rather than a hand-rolled ~/.meshbbs: it is what the
// platform says, which on Windows is %AppData% and on macOS is
// ~/Library/Application Support. A helper that scattered dotfiles would be one
// more thing for a user to lose, and losing this file loses their mail.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user config directory: %w", err)
	}
	return filepath.Join(dir, "meshbbs", "dm.key"), nil
}

// Save writes the wrapped private key, refusing to clobber an existing one.
//
// The file is 0600 and its directory 0700. On Windows those bits are close to
// meaningless, which is stated where a user will read it rather than left as a
// silent difference between platforms — see the warning Save returns.
func Save(path string, w *keyring.WrappedKey) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w at %s", ErrKeyFileExists, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check for an existing key file: %w", err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// O_EXCL rather than a plain create, so the check above and the write
	// cannot straddle another process doing the same thing. The Stat is for the
	// error message; this is the guarantee.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w at %s", ErrKeyFileExists, path)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(w.Encode()); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("flush %s: %w", path, err)
	}
	return f.Close()
}

// Load reads the wrapped private key. The passphrase is still needed to open it.
func Load(path string) (*keyring.WrappedKey, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s", ErrNoKeyFile, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return keyring.DecodeWrapped(b)
}

// PermissionWarning reports a key file readable by anyone but its owner.
//
// A warning rather than a refusal. The file is ciphertext — an attacker who
// reads it still needs the passphrase — so refusing to run would be punishing a
// user for a permission bit while their mail sits unread. But it is worth
// saying, because the passphrase is the only thing left at that point.
//
// Empty on Windows: the mode bits Go reports there do not describe the ACL that
// actually governs the file, and a warning derived from them would be a
// confident statement about something not measured.
func PermissionWarning(path string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Sprintf("%s is mode %04o — readable by others. "+
			"It is ciphertext, so your passphrase still stands between them and your mail, "+
			"but it is the only thing that does. Fix with: chmod 600 %s", path, mode, path)
	}
	return ""
}

// runtimeIsWindows is here so tests can skip the permission assertions without
// importing runtime, which would read as though the package branched on the OS
// in more places than it does.
func runtimeIsWindows() bool { return runtime.GOOS == "windows" }
