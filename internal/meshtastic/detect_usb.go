//go:build linux || windows

package meshtastic

import (
	"fmt"

	"go.bug.st/serial/enumerator"
)

// DetectPorts lists serial ports that might have a node behind them, best
// guess first (§7.1: "scan serial ports for Meshtastic VID/PIDs").
//
// # Why this file has a build tag
//
// The enumerator that reads USB identifiers is pure Go on Linux (sysfs) and
// Windows (the registry), but on macOS it needs IOKit through cgo — and §4
// makes cgo-free builds non-negotiable, since one cgo import breaks
// cross-compilation for all five targets at once. So macOS and everything else
// fall back to matching device names, in detect_names.go.
func DetectPorts() ([]Candidate, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		return nil, fmt.Errorf("enumerate serial ports: %w", err)
	}
	var out []Candidate
	for _, p := range ports {
		c := Candidate{Port: p.Name, VID: p.VID, PID: p.PID}
		switch {
		case p.IsUSB:
			if name, known := lookupUSB(p.VID, p.PID); known {
				c.Device, c.Known = name, true
				c.Why = fmt.Sprintf("USB %s:%s — %s", p.VID, p.PID, name)
			} else {
				c.Why = fmt.Sprintf("USB %s:%s — unrecognised, but a serial device", p.VID, p.PID)
			}
		default:
			// A non-USB serial port is almost never a Meshtastic node: it is a
			// built-in UART, a Bluetooth rfcomm device, or a virtual port. Keep
			// it in the list — a node wired to a Pi's GPIO UART is a real
			// configuration — but never rank it above a USB device.
			why, ok := looksLikeNodePort(p.Name)
			if !ok {
				continue
			}
			c.Why = why
		}
		out = append(out, c)
	}
	return rank(out), nil
}
