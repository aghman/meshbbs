package meshtastic

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
)

// configDump is the burst a node sends in answer to want_config_id.
func configDump(id uint32) []*meshpb.FromRadio {
	return []*meshpb.FromRadio{
		{PayloadVariant: &meshpb.FromRadio_MyInfo{MyInfo: &meshpb.MyNodeInfo{MyNodeNum: 0x11223344}}},
		{PayloadVariant: &meshpb.FromRadio_Metadata{Metadata: &meshpb.DeviceMetadata{
			FirmwareVersion: "2.7.4.abcdef",
			HwModel:         meshpb.HardwareModel_HELTEC_V3,
		}}},
		{PayloadVariant: &meshpb.FromRadio_Config{Config: &meshpb.Config{
			PayloadVariant: &meshpb.Config_Lora{Lora: &meshpb.Config_LoRaConfig{
				UsePreset:   true,
				ModemPreset: meshpb.Config_LoRaConfig_LONG_FAST,
				Region:      meshpb.Config_LoRaConfig_US,
				HopLimit:    3,
				TxEnabled:   true,
			}},
		}}},
		{PayloadVariant: &meshpb.FromRadio_Channel{Channel: &meshpb.Channel{
			Index: 0,
			Role:  meshpb.Channel_PRIMARY,
			Settings: &meshpb.ChannelSettings{
				Name: "LongFast",
				Psk:  []byte{0x01},
			},
		}}},
		{PayloadVariant: &meshpb.FromRadio_Channel{Channel: &meshpb.Channel{
			Index:    1,
			Role:     meshpb.Channel_SECONDARY,
			Settings: &meshpb.ChannelSettings{Name: "bbsnet"},
		}}},
		{PayloadVariant: &meshpb.FromRadio_ConfigCompleteId{ConfigCompleteId: id}},
	}
}

// answerConfig replies to a want_config_id with the given messages.
func answerConfig(msgs func(id uint32) []*meshpb.FromRadio) func(*radioSide, *meshpb.ToRadio) {
	return func(r *radioSide, m *meshpb.ToRadio) {
		id := m.GetWantConfigId()
		if id == 0 {
			return
		}
		for _, msg := range msgs(id) {
			r.send(msg)
		}
	}
}

func TestConfigureReadsRadioState(t *testing.T) {
	c := startFakeRadio(t, Options{}, answerConfig(configDump))

	info, err := Configure(context.Background(), c, ConfigRequest{ID: 42})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}

	if info.NodeNum != 0x11223344 {
		t.Errorf("NodeNum = %#x", info.NodeNum)
	}
	if info.FirmwareVersion != "2.7.4.abcdef" {
		t.Errorf("FirmwareVersion = %q", info.FirmwareVersion)
	}
	if info.HardwareModel != "HELTEC_V3" {
		t.Errorf("HardwareModel = %q", info.HardwareModel)
	}
	// The three numbers the governor's airtime model is built on (§7.6).
	if info.Region != "US" || info.ModemPreset != "LONG_FAST" || info.HopLimit != 3 {
		t.Errorf("region/preset/hop = %q/%q/%d", info.Region, info.ModemPreset, info.HopLimit)
	}
	if !info.TxEnabled {
		t.Error("TxEnabled = false")
	}

	if len(info.Channels) != 2 {
		t.Fatalf("channels = %+v, want 2", info.Channels)
	}
	// §8.3 needs to know which slots are encrypted, and never needs the key.
	if !info.Channels[0].Encrypted {
		t.Error("channel 0 should report as encrypted")
	}
	if info.Channels[1].Encrypted {
		t.Error("channel 1 has no PSK but reports as encrypted")
	}
	if info.Channels[1].Name != "bbsnet" || info.Channels[1].Role != "SECONDARY" {
		t.Errorf("channel 1 = %+v", info.Channels[1])
	}
}

// The whole point of the ID: a config dump left over from a previous client
// must not be mistaken for ours.
func TestConfigureIgnoresStaleCompletion(t *testing.T) {
	c := startFakeRadio(t, Options{}, answerConfig(func(id uint32) []*meshpb.FromRadio {
		msgs := []*meshpb.FromRadio{
			{PayloadVariant: &meshpb.FromRadio_ConfigCompleteId{ConfigCompleteId: id ^ 0xFFFF}},
		}
		return append(msgs, configDump(id)...)
	}))

	info, err := Configure(context.Background(), c, ConfigRequest{ID: 99})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if info.NodeNum == 0 {
		t.Error("returned at the stale completion, before any state arrived")
	}
}

func TestConfigureRejectsZeroID(t *testing.T) {
	c := startFakeRadio(t, Options{}, nil)
	if _, err := Configure(context.Background(), c, ConfigRequest{ID: 0}); err == nil {
		t.Fatal("accepted want_config_id = 0, which the firmware treats as unset")
	}
}

// A node that reboots mid-dump leaves a half-populated RadioInfo. Returning it
// would hand the governor an airtime model built from missing fields.
func TestConfigureFailsOnReboot(t *testing.T) {
	c := startFakeRadio(t, Options{}, answerConfig(func(id uint32) []*meshpb.FromRadio {
		return []*meshpb.FromRadio{
			{PayloadVariant: &meshpb.FromRadio_MyInfo{MyInfo: &meshpb.MyNodeInfo{MyNodeNum: 1}}},
			{PayloadVariant: &meshpb.FromRadio_Rebooted{Rebooted: true}},
		}
	}))

	if _, err := Configure(context.Background(), c, ConfigRequest{ID: 5}); err == nil {
		t.Fatal("configure succeeded across a reboot")
	}
}

// Traffic keeps arriving while the radio dumps its config. Dropping it is
// survivable but not free, so the caller gets the chance to keep it.
func TestConfigureForwardsPacketsSeenDuringHandshake(t *testing.T) {
	c := startFakeRadio(t, Options{}, answerConfig(func(id uint32) []*meshpb.FromRadio {
		msgs := configDump(id)
		packet := &meshpb.FromRadio{PayloadVariant: &meshpb.FromRadio_Packet{
			Packet: &meshpb.MeshPacket{Id: 1234, From: 0x99},
		}}
		// Insert before the completion message.
		return append(msgs[:len(msgs)-1], packet, msgs[len(msgs)-1])
	}))

	var got []uint32
	_, err := Configure(context.Background(), c, ConfigRequest{
		ID:       7,
		OnPacket: func(p *meshpb.MeshPacket) { got = append(got, p.GetId()) },
	})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if len(got) != 1 || got[0] != 1234 {
		t.Errorf("packets seen = %v, want [1234]", got)
	}
}

// Over TCP the firmware's log output arrives as LogRecord rather than as raw
// text between frames. The sysop should not have to know that.
func TestConfigureRoutesLogRecordsToTheSink(t *testing.T) {
	var logs []string
	c := startFakeRadio(t, Options{OnDeviceLog: func(s string) { logs = append(logs, s) }},
		answerConfig(func(id uint32) []*meshpb.FromRadio {
			msgs := []*meshpb.FromRadio{{PayloadVariant: &meshpb.FromRadio_LogRecord{
				LogRecord: &meshpb.LogRecord{Message: "starting\x1b[0m radio"},
			}}}
			return append(msgs, configDump(id)...)
		}))

	if _, err := Configure(context.Background(), c, ConfigRequest{ID: 3}); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0] != "starting.[0m radio" {
		t.Errorf("device log = %q, want the sanitised line", logs)
	}
}

// A silent radio must not hang the caller forever, and cancelling must leave
// nothing behind — the connection is closed, not left mid-dump.
func TestConfigureHonoursContextCancellation(t *testing.T) {
	c := startFakeRadio(t, Options{}, nil) // never answers

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := Configure(ctx, c, ConfigRequest{ID: 1})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v to give up", elapsed)
	}
	if err := c.Send(&meshpb.ToRadio{}); err == nil {
		t.Error("connection still usable after a cancelled handshake")
	}
}

// Reading the diagnostic counters after a cancelled handshake races the read
// loop, which outlives the cancellation by however long the close takes to
// land. `mesh info` does exactly this on every timeout.
func TestCountersAreSafeToReadAfterCancellation(t *testing.T) {
	c := startFakeRadio(t, Options{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Configure(ctx, c, ConfigRequest{ID: 1}); err == nil {
		t.Fatal("expected the handshake to time out")
	}
	if c.Frames() != 0 {
		t.Errorf("Frames() = %d, want 0 from a silent radio", c.Frames())
	}
	_ = c.Skipped()
}
