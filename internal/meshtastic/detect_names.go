//go:build !linux && !windows

package meshtastic

import (
	"fmt"

	"go.bug.st/serial"
)

// DetectPorts lists serial ports that might have a node behind them, matching
// on device names because USB identifiers are not available here without cgo.
// See detect_usb.go for why that trade is made this way round.
func DetectPorts() ([]Candidate, error) {
	ports, err := serial.GetPortsList()
	if err != nil {
		return nil, fmt.Errorf("list serial ports: %w", err)
	}
	var out []Candidate
	for _, p := range ports {
		why, ok := looksLikeNodePort(p)
		if !ok {
			continue
		}
		// Known stays false on purpose. The name says "something USB-serial is
		// here", which is weaker evidence than a matched VID/PID, and the sysop
		// reading `meshbbs mesh info` should see that distinction rather than a
		// confident claim we cannot support.
		out = append(out, Candidate{Port: p, Why: why})
	}
	return rank(out), nil
}
