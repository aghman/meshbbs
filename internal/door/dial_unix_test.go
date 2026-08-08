//go:build !windows

package door

import "net"

func dialAPI(addr string) (net.Conn, error) { return net.Dial("unix", addr) }

func isWindows() bool { return false }
