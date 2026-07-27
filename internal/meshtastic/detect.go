package meshtastic

import (
	"sort"
	"strings"
)

// Candidate is a serial port that might have a Meshtastic node behind it.
type Candidate struct {
	// Port is the device path to hand to DialSerial.
	Port string
	// VID and PID are lowercase hex USB identifiers, empty where the platform
	// cannot tell us (macOS, without the IOKit cgo bindings we deliberately do
	// not link — see detect_names.go).
	VID, PID string
	// Device names the board when the USB identifiers recognise one.
	Device string
	// Why explains the match in one line, for `meshbbs mesh info`. Detection is
	// a guess, and a sysop staring at three ports needs to know which guess.
	Why string
	// Known is true when the USB identifiers matched the table below. Sorting
	// puts these first.
	Known bool
}

// usbDevice is one row of the USB identifier table.
type usbDevice struct {
	vid, pid string
	name     string
}

// knownDevices are the USB serial chips and native-USB MCUs that Meshtastic
// hardware ships with.
//
// The identifiers are cribbed from meshtastic-python's supported_device.py,
// which maintains this list against real boards. It is a heuristic and always
// will be — most of these are generic USB-UART bridges that anything might use,
// which is exactly why detection RANKS ports rather than picking one silently,
// and why the config file can always name the port outright (§11.5).
var knownDevices = []usbDevice{
	{"10c4", "ea60", "CP210x (T-Beam, Heltec, RAK)"},
	{"1a86", "55d4", "CH9102 (T-Beam, T-Echo)"},
	{"1a86", "7523", "CH340 (Heltec, LilyGO)"},
	{"239a", "0029", "nRF52840 (RAK4631, bootloader)"},
	{"239a", "8029", "nRF52840 (RAK4631)"},
	{"2886", "0059", "Seeed XIAO nRF52840"},
	{"303a", "1001", "ESP32-S3 native USB (Heltec V3, T-Deck)"},
}

func lookupUSB(vid, pid string) (string, bool) {
	vid, pid = strings.ToLower(vid), strings.ToLower(pid)
	for _, d := range knownDevices {
		if d.vid == vid && d.pid == pid {
			return d.name, true
		}
	}
	return "", false
}

// portNamePrefixes are the device-name patterns USB serial adapters take on the
// platforms where we cannot read USB identifiers.
var portNamePrefixes = []string{
	"cu.usbmodem",     // native USB (nRF52840, ESP32-S3), macOS
	"cu.usbserial",    // FTDI and generic bridges, macOS
	"cu.wchusbserial", // CH340/CH9102, macOS
	"cu.SLAB_USBtoUART",
	"ttyUSB", // USB-UART bridges, Linux
	"ttyACM", // CDC-ACM, Linux
}

// looksLikeNodePort matches a port path by name.
//
// On macOS this deliberately ignores /dev/tty.* in favour of /dev/cu.*. They
// are the same hardware, but opening the tty device blocks until carrier
// detect asserts, so a detector that offered it would hand back a port that
// hangs on open — a bug that presents as "meshbbs freezes at startup".
func looksLikeNodePort(path string) (string, bool) {
	name := path
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if strings.HasPrefix(name, "tty.") {
		return "", false
	}
	for _, p := range portNamePrefixes {
		if strings.HasPrefix(name, p) {
			return "matches a USB serial device name (" + p + "*)", true
		}
	}
	return "", false
}

// rank orders candidates: recognised USB identifiers first, then by port name
// so the result is stable. Stability matters because DialSerial with no port
// configured takes the first entry, and a detector that reordered itself
// between runs would connect to a different radio each time.
func rank(cands []Candidate) []Candidate {
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Known != cands[j].Known {
			return cands[i].Known
		}
		return cands[i].Port < cands[j].Port
	})
	return cands
}
