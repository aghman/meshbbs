package meshtastic

import (
	"context"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
)

// RadioInfo is what the node tells us about itself during the config exchange.
//
// The fields are chosen for what the rest of Phase 3 needs, not for
// completeness: the governor needs the modem preset and region to compute
// airtime (Appendix A) and the hop limit to charge the flood multiplier
// correctly (§1.1); the ham-mode safety check needs to see whether a channel
// carries a PSK (§8.3); and a sysop needs the firmware version when something
// misbehaves.
type RadioInfo struct {
	// NodeNum is the radio's own address on the mesh. It is NOT the BBS node
	// ID: §6.1 derives that from our Ed25519 key and it has no relationship to
	// the radio's identity, which is why a node can be replaced without the
	// federation noticing.
	NodeNum         uint32
	FirmwareVersion string
	HardwareModel   string

	Region      string
	ModemPreset string
	// UsePreset is false when the sysop has hand-set bandwidth, spreading
	// factor and coding rate. The governor's airtime model has to read those
	// directly in that case rather than assuming a preset's numbers.
	UsePreset    bool
	Bandwidth    uint32
	SpreadFactor uint32
	CodingRate   uint32
	HopLimit     uint32
	TxEnabled    bool

	Channels []ChannelInfo
}

// ChannelInfo describes one of the node's eight channel slots.
type ChannelInfo struct {
	Index int32
	Name  string
	Role  string
	// Encrypted reports whether the slot has a PSK. The PSK itself is
	// deliberately not carried here: §8.3 needs to know that encryption is ON
	// (which is illegal on amateur bands), and copying key material into a
	// struct that gets printed by `mesh info` would be a good way to leak it
	// into a log.
	Encrypted bool
}

// ConfigRequest parameterises the config exchange.
type ConfigRequest struct {
	// ID must be non-zero: the firmware uses it to tag the completion message,
	// so a client can tell a fresh config dump from the tail of a stale one.
	// It comes from the caller rather than being generated here because §12.1
	// requires randomness to come from an injected source.
	ID uint32
	// OnPacket, if set, receives mesh packets that arrive mid-handshake.
	//
	// The radio does not hold traffic while it dumps its configuration, so
	// without this they would be read off the stream and dropped. That is
	// survivable — L1 repairs loss (§7.2) — but silently discarding received
	// packets at every reconnect is the kind of small leak that shows up later
	// as "sync is slower than the model says".
	OnPacket func(*meshpb.MeshPacket)
}

// Configure runs the config exchange and returns what the node reported.
//
// The radio answers a want_config_id with a burst of its own state — node
// info, channels, config, module config — terminated by a matching
// config_complete_id. Until that arrives the node will not accept packets to
// send, so this is the first thing any client does after connecting.
//
// Cancelling ctx CLOSES the connection. A blocked read on a serial port cannot
// be interrupted any other way, and a caller who abandons the handshake has no
// use for a stream sitting mid-dump.
func Configure(ctx context.Context, c *Conn, req ConfigRequest) (*RadioInfo, error) {
	if req.ID == 0 {
		return nil, errors.New("meshtastic: want_config_id must be non-zero")
	}
	if err := c.Send(&meshpb.ToRadio{
		PayloadVariant: &meshpb.ToRadio_WantConfigId{WantConfigId: req.ID},
	}); err != nil {
		return nil, fmt.Errorf("request config: %w", err)
	}

	type result struct {
		info *RadioInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		info, err := readConfig(c, req)
		done <- result{info, err}
	}()

	select {
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case r := <-done:
		return r.info, r.err
	}
}

func readConfig(c *Conn, req ConfigRequest) (*RadioInfo, error) {
	info := &RadioInfo{}
	for {
		msg, err := c.Recv()
		if err != nil {
			return nil, fmt.Errorf("reading config from %s: %w", c.name, err)
		}

		switch {
		case msg.GetConfigCompleteId() != 0:
			if msg.GetConfigCompleteId() != req.ID {
				// The tail of a previous client's config dump. Ignoring it is
				// the entire reason the ID exists.
				continue
			}
			return info, nil

		case msg.GetMyInfo() != nil:
			info.NodeNum = msg.GetMyInfo().GetMyNodeNum()

		case msg.GetMetadata() != nil:
			md := msg.GetMetadata()
			info.FirmwareVersion = sanitiseText(md.GetFirmwareVersion())
			info.HardwareModel = md.GetHwModel().String()

		case msg.GetConfig().GetLora() != nil:
			lora := msg.GetConfig().GetLora()
			info.Region = lora.GetRegion().String()
			info.ModemPreset = lora.GetModemPreset().String()
			info.UsePreset = lora.GetUsePreset()
			info.Bandwidth = lora.GetBandwidth()
			info.SpreadFactor = lora.GetSpreadFactor()
			info.CodingRate = lora.GetCodingRate()
			info.HopLimit = lora.GetHopLimit()
			info.TxEnabled = lora.GetTxEnabled()

		case msg.GetChannel() != nil:
			ch := msg.GetChannel()
			info.Channels = append(info.Channels, ChannelInfo{
				Index: ch.GetIndex(),
				// A channel name is set by whoever configured the radio and is
				// rendered straight into a terminal by `mesh info`.
				Name:      sanitiseText(ch.GetSettings().GetName()),
				Role:      ch.GetRole().String(),
				Encrypted: len(ch.GetSettings().GetPsk()) > 0,
			})

		case msg.GetLogRecord() != nil:
			c.deviceLog(sanitiseText(msg.GetLogRecord().GetMessage()))

		case msg.GetPacket() != nil:
			if req.OnPacket != nil {
				req.OnPacket(msg.GetPacket())
			}

		case msg.GetRebooted():
			// The node restarted underneath us, so everything collected so far
			// describes a device that no longer exists. Fail rather than return
			// a half-populated RadioInfo the governor would then compute
			// airtime from.
			return nil, errors.New("meshtastic: node rebooted during config exchange")
		}
	}
}
