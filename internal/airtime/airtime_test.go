package airtime

import (
	"math"
	"testing"
	"time"
)

// The whole point of this package: Appendix A's validation table, which was
// itself checked against Meshtastic's published figures. If these move, every
// budget in the design moves with them.
func TestAppendixAValidationTable(t *testing.T) {
	cases := []struct {
		payload int
		want    time.Duration
		note    string
	}{
		{16, 354 * time.Millisecond, "matches Meshtastic's documented 354 ms exactly"},
		{100, 1010 * time.Millisecond, "a digest"},
		{233, 1993 * time.Millisecond, "app payload without the header"},
		{256, 2157 * time.Millisecond, "a full frame; §1 quotes 2.16 s"},
	}
	for _, c := range cases {
		got := LongFast.Raw(c.payload)
		if !within(got, c.want, 3*time.Millisecond) {
			t.Errorf("LongFast.Raw(%d) = %v, want %v (%s)", c.payload, got, c.want, c.note)
		}
	}
}

// A full application payload costs the 256-byte frame, not the 233-byte one.
// Appendix A's last row omits the header; §1's "2.16 s" does not.
func TestFullApplicationPayloadCostsAFullFrame(t *testing.T) {
	got := LongFast.Packet(233)
	if !within(got, 2157*time.Millisecond, 3*time.Millisecond) {
		t.Errorf("Packet(233) = %v, want ~2.157s", got)
	}
	if LongFast.Packet(233) <= LongFast.Raw(233) {
		t.Error("the header is not being charged")
	}
}

// §7.6 says bytes are a bad proxy for airtime "because airtime is superlinear
// in payload size". That is backwards, and the direction matters.
//
// Airtime is affine in payload — a large fixed cost (preamble plus header) plus
// a linear term — so per byte it is strongly SUBlinear: 436 ms for one byte,
// 9.3 ms/byte for a full payload. The conclusion in §7.6 survives, but for the
// opposite reason: a byte-denominated budget does not under-price big packets,
// it wildly under-prices SMALL ones. Sending 233 bytes as one packet costs
// 2.16 s; sending them one byte at a time costs 101 s for the same data.
//
// That is a governor requirement, not a curiosity: cost must be charged per
// PACKET, or a chatty protocol that stays inside a byte budget can still take
// the channel apart.
func TestAirtimePerByteFallsWithPayloadSize(t *testing.T) {
	perByte := func(n int) float64 { return float64(LongFast.Packet(n)) / float64(n) }

	if perByte(1) <= perByte(233) {
		t.Fatal("per-byte cost does not fall with payload size")
	}
	// The fixed overhead is most of a small packet, which is what makes
	// fragmentation so expensive.
	if ratio := perByte(1) / perByte(233); ratio < 40 {
		t.Errorf("one-byte payload is only %.0fx worse per byte; expected far more", ratio)
	}

	// The concrete governor case: the same 233 bytes, split up.
	whole := LongFast.Packet(233)
	split := 233 * LongFast.Packet(1)
	if split < 40*whole {
		t.Errorf("splitting a payload costs %.1fx, expected an order of magnitude more",
			float64(split)/float64(whole))
	}
}

// The real reason bytes cannot price airtime: the same byte costs 280 times
// more at one preset than another, and the preset is the sysop's choice.
func TestPerByteCostVariesEnormouslyAcrossPresets(t *testing.T) {
	cheap := float64(ShortTurbo.Packet(100))
	dear := float64(VeryLongSlow.Packet(100))
	if ratio := dear / cheap; ratio < 100 {
		t.Errorf("preset spread is only %.0fx; expected two orders of magnitude", ratio)
	}
}

// Every step of spreading factor roughly doubles the time on air. This is what
// makes LONG_SLOW so expensive and why a sysop's preset choice dominates the
// budget more than anything meshbbs does.
func TestSlowerPresetsCostMore(t *testing.T) {
	ordered := []Preset{ShortTurbo, ShortFast, ShortSlow, MediumFast, MediumSlow, LongFast, LongSlow}
	for i := 1; i < len(ordered); i++ {
		prev, cur := ordered[i-1].Packet(100), ordered[i].Packet(100)
		if cur <= prev {
			t.Errorf("%s (%v) is not slower than %s (%v)",
				ordered[i].Name, cur, ordered[i-1].Name, prev)
		}
	}
	// VERY_LONG_SLOW is the extreme: one full packet holds the channel for
	// several seconds, and a node using it cannot federate meaningfully.
	if VeryLongSlow.Packet(233) < 5*time.Second {
		t.Errorf("VERY_LONG_SLOW full packet = %v, expected several seconds",
			VeryLongSlow.Packet(233))
	}
}

// The low data-rate optimization engages once a symbol exceeds 16.38 ms, and
// the step it causes is bigger than the doubling the spreading factor alone
// would give. At 125 kHz that boundary falls between SF10 (8.2 ms/symbol) and
// SF11 (16.4 ms) — NOT between SF11 and SF12, which are both already past it
// and so differ by a plain doubling.
func TestLowDataRateOptimizationThreshold(t *testing.T) {
	sf10 := Preset{"t10", 125_000, 10, 5}
	sf11 := Preset{"t11", 125_000, 11, 5}
	sf12 := Preset{"t12", 125_000, 12, 5}

	if crossing := float64(sf11.Packet(100)) / float64(sf10.Packet(100)); crossing <= 2.0 {
		t.Errorf("crossing the threshold cost %.2fx, expected more than a doubling", crossing)
	}
	if beyond := float64(sf12.Packet(100)) / float64(sf11.Packet(100)); beyond > 2.0 {
		t.Errorf("both sides of the threshold: ratio %.2f should be at most a doubling", beyond)
	}
}

// Cost is what the COMMONS pays, not what we pay: the §1.1 correction the whole
// governor rests on.
func TestCostMultipliesByTheFloodMultiplier(t *testing.T) {
	one := LongFast.Packet(100)
	if got := LongFast.Cost(100, 4); !within(got, 4*one, time.Millisecond) {
		t.Errorf("Cost at R=4 = %v, want %v", got, 4*one)
	}
	// R below 1 is meaningless — our own transmission always happens — and must
	// not be able to discount a packet below its true cost.
	if got := LongFast.Cost(100, 0); got != one {
		t.Errorf("Cost at R=0 = %v, want the unmultiplied %v", got, one)
	}
}

func TestPresetLookup(t *testing.T) {
	// The names must match what the radio reports, or a real node's config
	// silently falls back to a default and every figure is wrong.
	for _, name := range []string{"LONG_FAST", "MEDIUM_SLOW", "SHORT_TURBO", "VERY_LONG_SLOW"} {
		if _, err := PresetByName(name); err != nil {
			t.Errorf("PresetByName(%q): %v", name, err)
		}
	}
	if _, err := PresetByName("LONGFAST"); err == nil {
		t.Error("accepted a name no radio reports")
	}
}

func TestCustomPresetValidation(t *testing.T) {
	if _, err := Custom(250_000, 11, 5); err != nil {
		t.Errorf("rejected a valid custom preset: %v", err)
	}
	for _, bad := range [][3]int{{250_000, 6, 5}, {250_000, 13, 5}, {250_000, 11, 4}, {0, 11, 5}} {
		if _, err := Custom(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("accepted out-of-range parameters %v", bad)
		}
	}
}

func within(got, want, tol time.Duration) bool {
	return math.Abs(float64(got-want)) <= float64(tol)
}
