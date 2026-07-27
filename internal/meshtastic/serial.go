package meshtastic

import (
	"fmt"

	"go.bug.st/serial"
)

// DefaultBaud is the rate every current Meshtastic firmware exposes its API at.
const DefaultBaud = 115200

// SerialConfig opens a serial connection to a node.
type SerialConfig struct {
	// Port is the device path: /dev/ttyUSB0, /dev/cu.usbserial-0001, COM3.
	// Empty means auto-detect (§7.1) — see DetectPorts.
	Port string
	// Baud defaults to DefaultBaud.
	Baud int
	// OnDeviceLog receives the node's debug output. Optional.
	OnDeviceLog func(string)
}

// DialSerial opens a serial port and wakes the node on it.
//
// §7.1 recommends serial as the default: it is the most reliable of the two
// transports, and unlike TCP it does not depend on the node's WiFi staying up —
// which on an ESP32 sharing an antenna with a LoRa radio is not a given.
func DialSerial(cfg SerialConfig) (*Conn, error) {
	if cfg.Baud == 0 {
		cfg.Baud = DefaultBaud
	}
	port := cfg.Port
	if port == "" {
		found, err := DetectPorts()
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("no Meshtastic serial device found; set the port explicitly")
		}
		port = found[0].Port
	}

	p, err := serial.Open(port, &serial.Mode{BaudRate: cfg.Baud})
	if err != nil {
		return nil, fmt.Errorf("open %s at %d baud: %w", port, cfg.Baud, err)
	}

	// Block in Read rather than poll.
	//
	// A per-read timeout would return (0, nil) on expiry, and bufio treats a
	// hundred of those in a row as io.ErrNoProgress — so a "helpful" timeout
	// here would turn a quiet radio into a spurious error. Close is what
	// interrupts a blocked read, and Conn.Close does exactly that.
	if err := p.SetReadTimeout(serial.NoTimeout); err != nil {
		p.Close()
		return nil, fmt.Errorf("configure %s: %w", port, err)
	}

	c := NewConn(p, Options{Name: "serial:" + port, OnDeviceLog: cfg.OnDeviceLog})
	if err := c.Wake(); err != nil {
		c.Close()
		return nil, fmt.Errorf("wake %s: %w", port, err)
	}
	return c, nil
}
