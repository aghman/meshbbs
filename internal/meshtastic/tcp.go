package meshtastic

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// DefaultTCPPort is the port the node's WiFi API listens on.
const DefaultTCPPort = 4403

// TCPConfig opens a network connection to a node.
type TCPConfig struct {
	// Host is the node's address, with or without a port.
	Host string
	// OnDeviceLog receives the node's debug output. Optional — over TCP the
	// firmware sends log lines as LogRecord messages rather than raw text, so
	// this normally stays silent.
	OnDeviceLog func(string)
}

// DialTCP connects to a node's WiFi API.
//
// §7.1 keeps this alongside serial because the best place for a radio (a roof,
// a mast, anywhere with sky) is rarely the best place for a server, and a node
// on WiFi can be sited for RF rather than for USB cable length.
func DialTCP(ctx context.Context, cfg TCPConfig) (*Conn, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("no host given")
	}
	addr := withDefaultPort(cfg.Host, DefaultTCPPort)

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	c := NewConn(conn, Options{Name: "tcp:" + addr, OnDeviceLog: cfg.OnDeviceLog})
	// Wake is harmless over TCP — the device is plainly awake if it accepted a
	// connection — but it still resynchronises a node whose parser was left
	// mid-frame by a client that vanished, which happens far more often over
	// WiFi than over a cable.
	if err := c.Wake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("wake %s: %w", addr, err)
	}
	return c, nil
}

// withDefaultPort appends a port if host does not carry one, handling bare
// IPv6 literals as well as names.
func withDefaultPort(host string, port int) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		// A bare IPv6 literal such as fd00::1.
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
