package meshtastic

import "testing"

func TestLooksLikeNodePort(t *testing.T) {
	yes := []string{
		"/dev/ttyUSB0",
		"/dev/ttyACM1",
		"/dev/cu.usbmodem14201",
		"/dev/cu.usbserial-0001",
		"/dev/cu.wchusbserial56230160321",
		"/dev/cu.SLAB_USBtoUART",
	}
	for _, p := range yes {
		if _, ok := looksLikeNodePort(p); !ok {
			t.Errorf("looksLikeNodePort(%q) = false, want true", p)
		}
	}

	no := []string{
		"/dev/ttyS0", // built-in UART
		"/dev/cu.Bluetooth-Incoming-Port",
		"/dev/null",
		// The tty twin of a cu device: same hardware, but opening it blocks
		// until carrier detect, which presents as meshbbs hanging at startup.
		"/dev/tty.usbmodem14201",
		"/dev/tty.usbserial-0001",
	}
	for _, p := range no {
		if _, ok := looksLikeNodePort(p); ok {
			t.Errorf("looksLikeNodePort(%q) = true, want false", p)
		}
	}
}

func TestLookupUSB(t *testing.T) {
	if name, ok := lookupUSB("10C4", "EA60"); !ok || name == "" {
		t.Errorf("uppercase identifiers did not match: %q %v", name, ok)
	}
	if _, ok := lookupUSB("dead", "beef"); ok {
		t.Error("matched an identifier that is not in the table")
	}
}

// DialSerial with no port configured takes the first candidate, so the order
// has to be deterministic and put recognised hardware first.
func TestRankPrefersKnownDevicesAndIsStable(t *testing.T) {
	in := []Candidate{
		{Port: "/dev/ttyUSB1"},
		{Port: "/dev/ttyUSB2", Known: true},
		{Port: "/dev/ttyUSB0"},
		{Port: "/dev/ttyUSB3", Known: true},
	}
	got := rank(in)
	want := []string{"/dev/ttyUSB2", "/dev/ttyUSB3", "/dev/ttyUSB0", "/dev/ttyUSB1"}
	for i, w := range want {
		if got[i].Port != w {
			t.Fatalf("rank() = %v, want %v", ports(got), want)
		}
	}
}

func ports(cs []Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Port
	}
	return out
}

// Enumeration talks to the OS three different ways (sysfs, the Windows
// registry, device names) and each one gets exercised by CI on its own runner.
// A machine with no radio attached is the normal case here; the assertion is
// that asking does not fail.
func TestDetectPortsDoesNotError(t *testing.T) {
	cands, err := DetectPorts()
	if err != nil {
		t.Fatalf("DetectPorts: %v", err)
	}
	for _, c := range cands {
		if c.Port == "" {
			t.Error("candidate with an empty port")
		}
		if c.Why == "" {
			t.Errorf("candidate %s has no explanation", c.Port)
		}
	}
}
