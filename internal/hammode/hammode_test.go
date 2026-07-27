package hammode

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/meshtastic"
)

func channels(encrypted bool) []meshtastic.ChannelInfo {
	return []meshtastic.ChannelInfo{
		{Index: 0, Role: "PRIMARY", Encrypted: true},
		{Index: 2, Name: "bbsnet", Role: "SECONDARY", Encrypted: encrypted},
	}
}

// The default case: an ISM-band instance, where none of this applies.
func TestUnlicensedIsUnrestricted(t *testing.T) {
	p := FromRadio(&meshtastic.RadioInfo{IsLicensed: false, Channels: channels(true)}, false)

	if p.Restricted() {
		t.Error("an unlicensed node is restricted")
	}
	if !p.AllowsEncryptedDMs() {
		t.Error("encrypted DMs blocked on an ISM instance")
	}
	if err := p.CheckChannel(channels(true), "bbsnet"); err != nil {
		t.Errorf("an encrypted channel was refused off ham mode: %v", err)
	}
	if p.Banner() != "" {
		t.Errorf("banner shown when nothing applies: %q", p.Banner())
	}
	if p.ExplainRefusal() != "" {
		t.Error("a refusal was explained when nothing is refused")
	}
}

func TestLicensedBlocksEncryptedDMs(t *testing.T) {
	p := FromRadio(&meshtastic.RadioInfo{IsLicensed: true, Channels: channels(false)}, false)

	if !p.Restricted() {
		t.Fatal("a licensed node is not restricted")
	}
	if p.AllowsEncryptedDMs() {
		t.Error("encrypted DMs allowed under Part 97")
	}
}

// §8.3's "consequence v0.1 missed": the channel PSK is encryption too, so
// disabling DMs is not enough — the instance must refuse to start.
func TestEncryptedChannelIsRefusedInHamMode(t *testing.T) {
	p := FromRadio(&meshtastic.RadioInfo{IsLicensed: true}, false)

	err := p.CheckChannel(channels(true), "bbsnet")
	if !errors.Is(err, ErrEncryptedChannel) {
		t.Fatalf("err = %v, want ErrEncryptedChannel", err)
	}
	// The fix is on the radio, so the message has to say so.
	for _, want := range []string{"bbsnet", "Meshtastic app", OverrideKey, "Signing is unaffected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}

	// An unencrypted BBS channel is fine even though the PRIMARY channel is
	// still encrypted: we only transmit on ours.
	if err := p.CheckChannel(channels(false), "bbsnet"); err != nil {
		t.Errorf("an unencrypted BBS channel was refused: %v", err)
	}
}

// The override exists, works, and is impossible to set by accident.
func TestOverrideRestoresEverythingAndWarnsLoudly(t *testing.T) {
	p := FromRadio(&meshtastic.RadioInfo{IsLicensed: true}, true)

	if p.Restricted() {
		t.Error("the override did not take effect")
	}
	if !p.AllowsEncryptedDMs() {
		t.Error("DMs still blocked despite the override")
	}
	if err := p.CheckChannel(channels(true), "bbsnet"); err != nil {
		t.Errorf("an encrypted channel was refused despite the override: %v", err)
	}

	// §8.3 requires a banner every launch, not a quiet flag.
	banner := p.Banner()
	if banner == "" {
		t.Fatal("no banner shown when the override is active")
	}
	for _, want := range []string{"HAM MODE", "Part 97", OverrideKey, "licence at risk is yours"} {
		if !strings.Contains(banner, want) {
			t.Errorf("override banner does not mention %q:\n%s", want, banner)
		}
	}

	// The key name must be self-documenting: nobody sets this without seeing
	// what they are agreeing to.
	if !strings.Contains(OverrideKey, "part97") || !strings.Contains(OverrideKey, "responsibility") {
		t.Errorf("override key %q is not self-documenting", OverrideKey)
	}
}

// The thing sysops get wrong: assuming ham mode makes the whole system
// unusable. The banner has to say what still works.
func TestRestrictedBannerSaysWhatStillWorks(t *testing.T) {
	p := FromRadio(&meshtastic.RadioInfo{IsLicensed: true}, false)
	banner := p.Banner()

	if !strings.Contains(banner, "Signing is unaffected") {
		t.Errorf("the banner does not say signing still works:\n%s", banner)
	}
	if !strings.Contains(banner, "Local DMs") {
		t.Errorf("the banner does not say local mail is unaffected:\n%s", banner)
	}
}

// §8.3 requires the refusal to reach "any user who tries to compose one", which
// means it has to be readable by someone who has never heard of Part 97.
func TestUserFacingRefusalIsPlainLanguage(t *testing.T) {
	p := FromRadio(&meshtastic.RadioInfo{IsLicensed: true}, false)
	msg := p.ExplainRefusal()

	if msg == "" {
		t.Fatal("no explanation for a blocked message")
	}
	if strings.Contains(msg, "Part 97") || strings.Contains(msg, "FCC") {
		t.Errorf("the user-facing text cites regulations at someone composing mail:\n%s", msg)
	}
	for _, want := range []string{"Mail to users", "forum posts federate", "sysop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the explanation is missing %q:\n%s", want, msg)
		}
	}
}

func TestNilRadioIsSafe(t *testing.T) {
	// A link that has not connected yet must not report a licensed operator it
	// has never heard from.
	p := FromRadio(nil, false)
	if p.Restricted() {
		t.Error("an unknown radio was treated as licensed")
	}
}
