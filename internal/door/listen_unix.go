//go:build !windows

package door

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// maxSocketPath is the shortest of the platform limits on a Unix socket path.
//
// sockaddr_un.sun_path is 104 bytes on the BSDs and macOS and 108 on Linux, and
// the failure when it is exceeded is a bind error naming neither the limit nor
// the path. Checking here turns that into a sentence someone can act on. The
// margin below the real limit is for the null terminator and for being wrong
// about a platform we have not tried.
const maxSocketPath = 100

// listenAPI opens the door API socket inside dir.
//
// dir is already mode 0700 and belongs to the account the server runs as, which
// is what §9.1.1's "filesystem permissions restricting it to the door's user
// account" comes down to on Unix: the socket's own mode is set as well, but the
// directory is the part that holds even against a door that could otherwise
// reach the path.
func listenAPI(dir string) (net.Listener, string, error) {
	// Deliberately short. The directory is already a temporary path that can be
	// sixty characters on macOS, and every character here comes off the budget.
	path := filepath.Join(dir, "api")
	if len(path) > maxSocketPath {
		return nil, "", fmt.Errorf(
			"the door API socket path is %d bytes and the limit is %d: %s",
			len(path), maxSocketPath, path)
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", fmt.Errorf("open door API socket: %w", err)
	}
	// net.Listen applies the umask, which a sysop can have set to anything.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, "", fmt.Errorf("restrict door API socket: %w", err)
	}
	return ln, path, nil
}
