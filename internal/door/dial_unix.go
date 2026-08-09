//go:build !windows

package door

import "net"

// dialAPI connects to a door API socket.
func dialAPI(addr string) (net.Conn, error) { return net.Dial("unix", addr) }
