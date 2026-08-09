//go:build windows

package door

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

// listenAPI opens the door API named pipe (§9.1.1).
//
// A pipe rather than a socket in a directory, because Windows has no Unix
// domain socket a door can be reasonably expected to reach, and because the
// access control a pipe carries is the point: the descriptor below restricts it
// to the owner and to SYSTEM, so §9.1.1's "filesystem permissions restricting it
// to the door's user account" is expressed by the pipe itself rather than by the
// directory around it, as on Unix.
//
// The name embeds the invocation's directory name, which is already unique per
// invocation, so two doors running at once cannot collide on the pipe namespace.
func listenAPI(dir string) (net.Listener, string, error) {
	name := `\\.\pipe\meshbbs-door-` + filepathBase(dir)

	// SDDL: D:P means a discretionary ACL with inheritance blocked, so nothing
	// wider is picked up from a parent. The two entries grant generic-all to
	// the owner (OW) and to Local System (SY) — System because a service
	// managing the BBS has to be able to clean up after it, and nobody else at
	// all.
	ln, err := winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;OW)(A;;GA;;;SY)",
		MessageMode:        false,
	})
	if err != nil {
		return nil, "", fmt.Errorf("open door API pipe: %w", err)
	}
	return ln, name, nil
}
