//go:build windows

package door

import (
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialAPI connects to a door API named pipe.
func dialAPI(addr string) (net.Conn, error) {
	timeout := 10 * time.Second
	return winio.DialPipe(addr, &timeout)
}
