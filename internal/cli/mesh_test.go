package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
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

// The mesh keys have to reach the generated reference, since §11.5's whole
// point is that the documentation is the source rather than a copy of it.
func TestConfigReferenceIncludesMeshKeys(t *testing.T) {
	dir := initInstance(t)
	out, err := run(t, "--data-dir", dir, "config", "reference")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"mesh.enabled", "mesh.channel_name", "mesh.airtime_ceiling_pct",
		"mesh.flood_multiplier", "mesh.quiet_hours", "mesh.ham_mode_override",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("config reference omits %s", key)
		}
	}
	// The two values a sysop most needs warned about.
	if !strings.Contains(out, "GUESS") {
		t.Error("the reference does not flag flood_multiplier as a guess")
	}
	if !strings.Contains(out, "Part 97") {
		t.Error("the reference does not explain what the ham override accepts")
	}
}

// Enabling the mesh with no reachable radio must fail startup with something a
// sysop can act on, rather than leaving an instance that looks healthy and
// federates nothing.
func TestServeFailsClearlyWithNoRadio(t *testing.T) {
	dir := initInstance(t)
	cfgPath := filepath.Join(dir, "config.toml")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte("\n[mesh]\nenabled = true\nmode = \"tcp\"\ntcp_host = \"127.0.0.1:1\"\n")...)
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "--data-dir", dir, "serve")
	if err == nil {
		t.Fatal("serve started with an unreachable radio")
	}
	if !strings.Contains(err.Error(), "mesh") {
		t.Errorf("the failure does not say it was the mesh: %v\n%s", err, out)
	}
}

// Federating an area is the decision that puts a conversation on other people's
// radios. It needs a supported way to make it — the alternative is a sysop
// editing the database by hand, which is what this replaces.
func TestAreaFederateRoundTrip(t *testing.T) {
	dir := initInstance(t)

	// Areas exist once the BBS has run; seed one the same way serve does.
	if _, err := run(t, "--data-dir", dir, "dev", "seed", "--seed", "1", "--users", "1", "--posts", "1"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "--data-dir", dir, "area", "list")
	if err != nil {
		t.Fatalf("area list: %v", err)
	}
	if !strings.Contains(out, "local only") {
		t.Errorf("areas are not local-only by default:\n%s", out)
	}

	if _, err := run(t, "--data-dir", dir, "area", "federate", "general"); err != nil {
		t.Fatalf("area federate: %v", err)
	}
	out, _ = run(t, "--data-dir", dir, "area", "list")
	if !strings.Contains(out, "yes") {
		t.Errorf("the area did not become federated:\n%s", out)
	}

	// And off again — which must say plainly that it does not unsend anything.
	out, err = run(t, "--data-dir", dir, "area", "federate", "general", "--off")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cannot be unsent") {
		t.Errorf("taking an area off the mesh implies posts are withdrawn:\n%s", out)
	}

	if _, err := run(t, "--data-dir", dir, "area", "federate", "nosucharea"); err == nil {
		t.Error("federated an area that does not exist")
	}
}

// An area's tag is derived from its name, which is the entire coordination
// mechanism between instances: same name, same tag, same conversation. There is
// no registry to consult, exactly as there is none for node IDs.
func TestAreaCreateDerivesTheTagFromTheName(t *testing.T) {
	one := initInstance(t)
	two := initInstance(t)

	var tags []string
	for _, dir := range []string{one, two} {
		out, err := run(t, "--data-dir", dir, "area", "create", "swapmeet", "--federated")
		if err != nil {
			t.Fatalf("area create: %v", err)
		}
		if !strings.Contains(out, "federates") {
			t.Errorf("creating a federated area did not say so:\n%s", out)
		}
		list, _ := run(t, "--data-dir", dir, "area", "list")
		for _, line := range strings.Split(list, "\n") {
			if strings.HasPrefix(line, "swapmeet") {
				tags = append(tags, strings.Fields(line)[1])
			}
		}
	}
	if len(tags) != 2 {
		t.Fatalf("expected a tag from each instance, got %v", tags)
	}
	if tags[0] != tags[1] {
		t.Errorf("two instances derived different tags for one name: %v", tags)
	}

	// A duplicate name is refused rather than silently creating a second area
	// that shares a tag.
	if _, err := run(t, "--data-dir", one, "area", "create", "swapmeet"); err == nil {
		t.Error("created the same area twice")
	}
}
