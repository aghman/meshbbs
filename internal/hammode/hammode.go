// Package hammode enforces the amateur-radio rules in design §8.3 `[D11]`.
//
// # The rule, and why it is a hard block
//
// A sysop who turns on Meshtastic's ham mode (`is_licensed = true`) unlocks
// higher transmit power on amateur allocations and, in the United States, comes
// under FCC Part 97 — which prohibits "messages encoded for the purpose of
// obscuring their meaning". Encrypted DMs are exactly that. So is the channel
// PSK.
//
// The design's decision is a hard block with a loudly-named override, and the
// reasoning is in its last line: sysops should not stumble into an FCC violation
// because our defaults were convenient. A licence is a personal legal
// instrument; the person who loses it is the sysop, not us, and they will lose
// it for traffic our software chose to encrypt.
//
// # What is NOT restricted, which matters more than what is
//
// Signing. Ed25519 signatures authenticate without obscuring anything, so the
// record log, NODE records, SUCCESSION records and the whole of forum
// federation operate normally under Part 97. Only confidentiality is the
// problem.
//
// This bears stating plainly because the obvious reading of "no encryption on
// amateur bands" is that the system is unusable there, and that is wrong: a
// ham-mode instance federates public traffic exactly like any other. It gives
// up private mail over the mesh, and nothing else.
//
// # Where this does not apply
//
// On the default ISM allocations — US 902-928 MHz under Part 15, EU 868 MHz —
// encryption is fine and every check here is inert. The overwhelming majority of
// instances will never see it.
package hammode

import (
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/meshtastic"
)

// OverrideKey is the config key that disables the block, named so that nobody
// can set it without noticing what they are agreeing to (§8.3, §11.5).
const OverrideKey = "i_accept_part97_responsibility"

// Policy is the ham-mode decision for this instance.
type Policy struct {
	// Licensed is what the radio reports.
	Licensed bool
	// Override is the sysop's explicit acceptance of responsibility.
	Override bool
}

// FromRadio builds a policy from what the node reported.
func FromRadio(info *meshtastic.RadioInfo, override bool) Policy {
	if info == nil {
		return Policy{Override: override}
	}
	return Policy{Licensed: info.IsLicensed, Override: override}
}

// Restricted reports whether Part 97 rules are being enforced.
func (p Policy) Restricted() bool { return p.Licensed && !p.Override }

// AllowsEncryptedDMs reports whether private mail may cross the mesh.
func (p Policy) AllowsEncryptedDMs() bool { return !p.Restricted() }

// ErrEncryptedChannel is returned when a ham-mode node's BBS channel has a PSK.
//
// This is §8.3's "consequence v0.1 missed": disabling encrypted DMs is not
// enough, because the channel PSK encrypts everything on it — including the
// public forum traffic that would otherwise be perfectly legal to send.
var ErrEncryptedChannel = fmt.Errorf("hammode: the BBS channel is encrypted and this node is in ham mode")

// CheckChannel refuses to start with an encrypted channel in ham mode.
//
// It names the channel and the fix, because the fix is on the radio and not in
// meshbbs: the sysop clears the PSK in the Meshtastic app, or accepts
// responsibility in config.
func (p Policy) CheckChannel(channels []meshtastic.ChannelInfo, name string) error {
	if !p.Restricted() {
		return nil
	}
	for _, ch := range channels {
		if ch.Name != name || !ch.Encrypted {
			continue
		}
		return fmt.Errorf("%w\n"+
			"  channel %q (slot %d) has a pre-shared key, which encrypts everything on it,\n"+
			"  and FCC Part 97 prohibits obscuring the meaning of amateur transmissions.\n"+
			"  Either clear the channel's PSK in the Meshtastic app, or set\n"+
			"  %s = true if you have decided that is your call to make.\n"+
			"  Signing is unaffected: a ham-mode instance federates public traffic normally.",
			ErrEncryptedChannel, name, ch.Index, OverrideKey)
	}
	return nil
}

// Banner is the startup warning. §8.3 requires it every launch, not once.
//
// Returns an empty string when there is nothing to say, so a caller can print
// it unconditionally.
func (p Policy) Banner() string {
	switch {
	case p.Licensed && p.Override:
		var b strings.Builder
		b.WriteString("⚠️  HAM MODE WITH PART 97 OVERRIDE\n")
		b.WriteString("    This node reports a licensed operator, and " + OverrideKey + " is set.\n")
		b.WriteString("    Encrypted direct messages and an encrypted channel are ENABLED.\n")
		b.WriteString("    FCC Part 97 prohibits transmissions encoded to obscure their meaning.\n")
		b.WriteString("    The licence at risk is yours.")
		return b.String()
	case p.Licensed:
		var b strings.Builder
		b.WriteString("Ham mode: this node reports a licensed operator (FCC Part 97).\n")
		b.WriteString("    Encrypted direct messages over the mesh are disabled.\n")
		b.WriteString("    Signing is unaffected — forums, NODE records and federation work normally.\n")
		b.WriteString("    Local DMs between users on this instance are unaffected.")
		return b.String()
	default:
		return ""
	}
}

// ExplainRefusal is what a user composing mail is told, per §8.3's requirement
// that the refusal reach "any user who tries to compose one".
//
// It is written for someone who does not know what Part 97 is and should not
// have to care: it says what cannot happen, what still can, and who to ask.
func (p Policy) ExplainRefusal() string {
	if !p.Restricted() {
		return ""
	}
	return "Private mail cannot leave this instance.\n" +
		"This BBS transmits over an amateur radio licence, and the rules for those " +
		"bands do not permit messages encrypted to hide their contents. Mail to users " +
		"here is unaffected; mail to another BBS is not possible from this node. " +
		"Public forum posts federate normally. Your sysop can explain the details."
}
