//go:build windows

package door

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

func dialAPI(addr string) (net.Conn, error) {
	timeout := 5 * time.Second
	return winio.DialPipe(addr, &timeout)
}

func isWindows() bool { return true }
