// Package airtime computes how long a LoRa packet occupies the channel
// (design Appendix A).
//
// # Why this is its own package, and why it is exact
//
// Every budget in the design is denominated in airtime-seconds rather than
// bytes, because airtime is superlinear in payload at high spreading factors —
// a packet twice as long costs rather more than twice as much. The governor
// (§7.6) prices transmissions with this, and `mesh survey` (§7.8) uses it to
// generate a load of known size. Both need the same numbers, and neither can
// afford them to be approximate: the whole federation budget is 5% of a shared
// channel, so a 20% error in the model is a fifth of the budget.
//
// The formula is Semtech's, and the design validates it against Meshtastic's
// own published figures. Those validations are tests here.
package airtime

import (
	"fmt"
	"math"
	"time"
)

// Preset is a Meshtastic modem preset: a named (bandwidth, spreading factor,
// coding rate) triple.
//
// The names match the values the radio reports in its LoRa config, so a preset
// read off hardware maps straight onto one of these.
type Preset struct {
	Name string
	// BandwidthHz is the channel bandwidth.
	BandwidthHz int
	// SpreadFactor is the LoRa SF, 7 to 12. Every step doubles symbol time.
	SpreadFactor int
	// CodingRate is the denominator of 4/x: 5 means 4/5.
	CodingRate int
}

// The presets Meshtastic ships, by the enum names the firmware reports.
//
// LONG_FAST is the default and the one every airtime figure in the design is
// computed against; it is also what a sysop who has changed nothing is running.
var (
	LongFast     = Preset{"LONG_FAST", 250_000, 11, 5}
	LongSlow     = Preset{"LONG_SLOW", 125_000, 12, 8}
	MediumFast   = Preset{"MEDIUM_FAST", 250_000, 9, 5}
	MediumSlow   = Preset{"MEDIUM_SLOW", 250_000, 10, 5}
	ShortFast    = Preset{"SHORT_FAST", 250_000, 7, 5}
	ShortSlow    = Preset{"SHORT_SLOW", 250_000, 8, 5}
	ShortTurbo   = Preset{"SHORT_TURBO", 500_000, 7, 5}
	VeryLongSlow = Preset{"VERY_LONG_SLOW", 62_500, 12, 8}
)

var presets = []Preset{
	LongFast, LongSlow, MediumFast, MediumSlow,
	ShortFast, ShortSlow, ShortTurbo, VeryLongSlow,
}

// PresetByName looks up a preset by the firmware's enum name.
func PresetByName(name string) (Preset, error) {
	for _, p := range presets {
		if p.Name == name {
			return p, nil
		}
	}
	return Preset{}, fmt.Errorf("airtime: unknown modem preset %q", name)
}

// Meshtastic's fixed radio parameters. These are firmware constants, not
// choices available to us, and getting one wrong shifts every figure at once.
const (
	// preambleSymbols is what Meshtastic configures on every preset.
	preambleSymbols = 16
	// explicitHeader: Meshtastic uses the explicit header, so IH = 0.
	implicitHeader = 0
	// crcOn: the CRC is enabled, so CRC = 1.
	crcOn = 1
	// HeaderBytes is everything on air in front of the application payload.
	//
	// §1 fixes both ends of this: 233 usable application bytes inside a
	// 256-byte frame. The difference is the header, and it is charged on every
	// packet — a caller reasoning in application bytes would otherwise
	// under-price every transmission by the same amount, about 190 ms at
	// LongFast.
	HeaderBytes = 256 - 233
)

// Packet returns the time on air for n APPLICATION bytes, header included.
//
// Note this is the honest cost of a full payload, and it is slightly higher
// than one figure in Appendix A: that table reads "PL=233 (full app payload) →
// 1.993 s", but 233 there is the payload WITHOUT the header, so the real cost
// of a full application payload is the 256-byte frame — 2.157 s, which is what
// §1 quotes as "2.16 s" for a full packet. The two statements in the design are
// consistent once the header is accounted for; this function accounts for it.
func (p Preset) Packet(n int) time.Duration {
	return p.Raw(n + HeaderBytes)
}

// Raw returns the time on air for a physical-layer payload of n bytes.
//
// This is the function Appendix A's validation table is computed against, so it
// is exported: a conformance vector set (§12.6) needs to state airtime in terms
// the Semtech formula uses, not in terms of our framing.
func (p Preset) Raw(n int) time.Duration {
	sf := float64(p.SpreadFactor)

	// Symbol time.
	ts := math.Exp2(sf) / float64(p.BandwidthHz)

	// Low data-rate optimization is mandatory once a symbol exceeds 16.38 ms,
	// and it costs throughput — which is why SF12 is so much worse than SF11
	// rather than merely twice as slow.
	de := 0.0
	if ts > 0.01638 {
		de = 1
	}

	tPreamble := (preambleSymbols + 4.25) * ts

	num := 8*float64(n) - 4*sf + 28 + 16*crcOn - 20*implicitHeader
	den := 4 * (sf - 2*de)
	// The formula's (CR+4) term, where CR is the 1-4 coding-rate index, is just
	// the denominator of 4/x — 5 for 4/5, 8 for 4/8.
	payloadSymbols := 8 + math.Max(math.Ceil(num/den)*float64(p.CodingRate), 0)

	return time.Duration((tPreamble + payloadSymbols*ts) * float64(time.Second))
}

// Cost returns what one packet of n application bytes costs THE COMMONS: the
// time on air multiplied by the flood multiplier R (§1.1).
//
// This is the number the governor budgets against, and the distinction is the
// single most important correction in the design. A node that budgets its own
// transmission time is measuring what it costs itself; on a flood mesh every
// packet is rebroadcast by every neighbour that hears it, so the channel pays R
// times what the sender does. Budgeting the wrong one is how a well-behaved
// node becomes a bad neighbour.
func (p Preset) Cost(n int, floodMultiplier float64) time.Duration {
	if floodMultiplier < 1 {
		// R below 1 is meaningless: our own transmission always happens.
		floodMultiplier = 1
	}
	return time.Duration(float64(p.Packet(n)) * floodMultiplier)
}

// String renders the preset for logs and reports.
func (p Preset) String() string {
	return fmt.Sprintf("%s (SF%d, BW%dkHz, CR4/%d)",
		p.Name, p.SpreadFactor, p.BandwidthHz/1000, p.CodingRate)
}

// Custom builds a preset from hand-set radio parameters, for the sysop who has
// turned use_preset off.
func Custom(bandwidthHz, spreadFactor, codingRate int) (Preset, error) {
	if spreadFactor < 7 || spreadFactor > 12 {
		return Preset{}, fmt.Errorf("airtime: spreading factor %d out of range 7-12", spreadFactor)
	}
	if codingRate < 5 || codingRate > 8 {
		return Preset{}, fmt.Errorf("airtime: coding rate 4/%d out of range 4/5-4/8", codingRate)
	}
	if bandwidthHz <= 0 {
		return Preset{}, fmt.Errorf("airtime: bandwidth %d Hz out of range", bandwidthHz)
	}
	return Preset{"CUSTOM", bandwidthHz, spreadFactor, codingRate}, nil
}
