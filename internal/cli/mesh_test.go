package cli

import (
	"bytes"
	"net"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"google.golang.org/protobuf/proto"
)

// fakeNode answers a want_config_id the way a real radio does, over TCP.
//
// It is built from this package's public API — AppendFrame and FrameReader —
// so the test exercises the real framing in both directions rather than a
// mock of it.
func fakeNode(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		fr := meshtastic.NewFrameReader(conn, nil)
		for {
			body, err := fr.ReadFrame()
			if err != nil {
				return
			}
			var m meshpb.ToRadio
			if err := proto.Unmarshal(body, &m); err != nil {
				return
			}
			id := m.GetWantConfigId()
			if id == 0 {
				continue
			}
			for _, msg := range []*meshpb.FromRadio{
				{PayloadVariant: &meshpb.FromRadio_MyInfo{
					MyInfo: &meshpb.MyNodeInfo{MyNodeNum: 0x0a0b0c0d}}},
				{PayloadVariant: &meshpb.FromRadio_Metadata{
					Metadata: &meshpb.DeviceMetadata{
						FirmwareVersion: "2.7.4.test",
						HwModel:         meshpb.HardwareModel_RAK4631,
					}}},
				{PayloadVariant: &meshpb.FromRadio_Config{Config: &meshpb.Config{
					PayloadVariant: &meshpb.Config_Lora{Lora: &meshpb.Config_LoRaConfig{
						UsePreset:   true,
						ModemPreset: meshpb.Config_LoRaConfig_LONG_FAST,
						Region:      meshpb.Config_LoRaConfig_EU_868,
						HopLimit:    3,
						TxEnabled:   true,
					}},
				}}},
				{PayloadVariant: &meshpb.FromRadio_Channel{Channel: &meshpb.Channel{
					Index:    1,
					Role:     meshpb.Channel_SECONDARY,
					Settings: &meshpb.ChannelSettings{Name: "bbsnet", Psk: []byte{9}},
				}}},
				{PayloadVariant: &meshpb.FromRadio_ConfigCompleteId{ConfigCompleteId: id}},
			} {
				raw, err := proto.Marshal(msg)
				if err != nil {
					return
				}
				frame, err := meshtastic.AppendFrame(nil, raw)
				if err != nil {
					return
				}
				if _, err := conn.Write(frame); err != nil {
					return
				}
			}
		}
	}()
	return ln.Addr().String()
}

// §7.8.3: the mesh commands must work with no BBS behind them — no init, no
// database, no key. Running against a temp dir that was never initialised is
// the assertion.
func TestMeshInfoNeedsNoInstance(t *testing.T) {
	addr := fakeNode(t)

	out, err := run(t, "--data-dir", t.TempDir(), "mesh", "info", "--tcp", addr)
	if err != nil {
		t.Fatalf("mesh info: %v\n%s", err, out)
	}

	for _, want := range []string{"2.7.4.test", "RAK4631", "EU_868", "LONG_FAST", "bbsnet", "encrypted"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// The hop limit is a multiplier on every airtime figure (§1.1), so it is
	// part of the minimum a sysop is shown.
	if !strings.Contains(out, "hop limit") {
		t.Errorf("output does not report the hop limit:\n%s", out)
	}
}

// A real radio reports all eight slots whether or not they are in use. Printing
// seven DISABLED rows buries the one line that matters, so unused slots collapse
// to a list of numbers — which is also the question a sysop is asking, since
// §7.1 wants a dedicated `bbsnet` channel in one of them.
func TestMeshInfoCollapsesUnusedChannelSlots(t *testing.T) {
	var buf bytes.Buffer
	channels := []meshtastic.ChannelInfo{
		{Index: 0, Role: "PRIMARY", Encrypted: true},
		{Index: 1, Role: "DISABLED"},
		{Index: 2, Name: "bbsnet", Role: "SECONDARY"},
		{Index: 3, Role: "DISABLED"},
		{Index: 4, Role: "DISABLED"},
	}
	printRadioInfo(&buf, &meshtastic.RadioInfo{UsePreset: true, TxEnabled: true, Channels: channels})
	out := buf.String()

	if strings.Contains(out, "DISABLED") {
		t.Errorf("unused slots are still listed in full:\n%s", out)
	}
	if !strings.Contains(out, "unused slots: 1, 3, 4") {
		t.Errorf("unused slot numbers missing:\n%s", out)
	}
	// An empty name on an active channel means the preset default, not "no name".
	if !strings.Contains(out, "(preset default)") {
		t.Errorf("empty channel name rendered misleadingly:\n%s", out)
	}
	if !strings.Contains(out, "bbsnet") {
		t.Errorf("active channel missing:\n%s", out)
	}
}

// A radio that can hear but not transmit is silently useless for federation.
func TestMeshInfoFlagsDisabledTransmit(t *testing.T) {
	var buf bytes.Buffer
	printRadioInfo(&buf, &meshtastic.RadioInfo{UsePreset: true, TxEnabled: false})
	if !strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("a node with tx_enabled = false is not flagged:\n%s", buf.String())
	}
}

func TestMeshPortsRuns(t *testing.T) {
	if _, err := run(t, "--data-dir", t.TempDir(), "mesh", "ports"); err != nil {
		t.Fatalf("mesh ports: %v", err)
	}
}
