package survey

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"github.com/aghman/meshbbs/internal/rng"
)

// Radio is a Node backed by a real Meshtastic connection.
//
// It owns the connection's read loop, which is why the survey does not go
// through meshlink: the link filters to one channel and one portnum, and a
// census wants everything the radio hears, including traffic it cannot decrypt.
type Radio struct {
	conn    *meshtastic.Conn
	preset  airtime.Preset
	region  string
	channel uint32
	selfNum uint32
	telemS  uint32
	clk     clock.Clock
	rnd     rng.Source
	refresh time.Duration

	heard chan Heard

	mu     sync.Mutex
	latest Metrics
	have   bool
	// wbuf is reused for the load payload, which is otherwise the only
	// allocation in the transmit path.
	wbuf []byte
}

// RadioConfig configures the adapter.
type RadioConfig struct {
	// Channel is the name of the channel to transmit the load on. Empty means
	// the primary.
	//
	// It matters less than it looks: channel separation is not airtime
	// separation (§7.1), so R measures the same physical channel either way.
	// What it changes is who sees the load — putting it on a named BBS channel
	// keeps unknown PRIVATE_APP frames off the community's primary.
	Channel string
	// Refresh is how often to ask the node for its metrics again.
	Refresh time.Duration
	Clock   clock.Clock
	Rand    rng.Source
}

// Attach runs the config exchange and starts the read loop.
func Attach(ctx context.Context, conn *meshtastic.Conn, cfg RadioConfig) (*Radio, error) {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
	}
	if cfg.Rand == nil {
		return nil, fmt.Errorf("survey: no random source configured")
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = time.Minute
	}

	info, err := meshtastic.Configure(ctx, conn, meshtastic.ConfigRequest{
		ID: uint32(cfg.Rand.Uint64()) | 1,
	})
	if err != nil {
		return nil, err
	}

	preset, err := presetFor(info)
	if err != nil {
		return nil, err
	}
	idx, err := channelIndex(info, cfg.Channel)
	if err != nil {
		return nil, err
	}
	if !info.TxEnabled {
		return nil, fmt.Errorf("survey: this node has transmit disabled, so it cannot generate a load")
	}

	r := &Radio{
		conn:    conn,
		preset:  preset,
		region:  info.Region,
		channel: idx,
		selfNum: info.NodeNum,
		telemS:  info.TelemetryIntervalSecs,
		clk:     cfg.Clock,
		rnd:     cfg.Rand,
		refresh: cfg.Refresh,
		heard:   make(chan Heard, 1024),
	}
	if info.Metrics != nil {
		r.record(info.Metrics)
	}
	return r, nil
}

// presetFor turns what the radio reported into airtime parameters.
func presetFor(info *meshtastic.RadioInfo) (airtime.Preset, error) {
	if !info.UsePreset {
		// A sysop who has hand-set the modem gets their own numbers used, not a
		// preset's — otherwise every figure in the report would be for a radio
		// configuration they are not running.
		return airtime.Custom(int(info.Bandwidth)*1000, int(info.SpreadFactor), int(info.CodingRate))
	}
	return airtime.PresetByName(info.ModemPreset)
}

func channelIndex(info *meshtastic.RadioInfo, name string) (uint32, error) {
	for _, ch := range info.Channels {
		if ch.Role == "DISABLED" {
			continue
		}
		if name == "" && ch.Role == "PRIMARY" {
			return uint32(ch.Index), nil
		}
		if name != "" && ch.Name == name {
			return uint32(ch.Index), nil
		}
	}
	if name == "" {
		return 0, fmt.Errorf("survey: the radio reports no enabled primary channel")
	}
	return 0, fmt.Errorf("survey: the radio has no enabled channel named %q", name)
}

// Run pumps the connection until ctx ends. It must be running for Metrics and
// Heard to produce anything.
func (r *Radio) Run(ctx context.Context) error {
	go r.refreshLoop(ctx)
	defer close(r.heard)

	for {
		msg, err := r.conn.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}

		if n := msg.GetNodeInfo(); n != nil && n.GetNum() == r.selfNum {
			if d := n.GetDeviceMetrics(); d != nil {
				r.record(&meshtastic.DeviceMetrics{
					ChannelUtilization: d.GetChannelUtilization(),
					AirUtilTx:          d.GetAirUtilTx(),
					UptimeSecs:         d.GetUptimeSeconds(),
				})
			}
		}

		if p := msg.GetPacket(); p != nil && p.GetFrom() != r.selfNum {
			h := Heard{
				From:     p.GetFrom(),
				ID:       p.GetId(),
				HopStart: p.GetHopStart(),
				HopLimit: p.GetHopLimit(),
				SNR:      p.GetRxSnr(),
				RSSI:     p.GetRxRssi(),
				Bytes:    len(p.GetDecoded().GetPayload()) + len(p.GetEncrypted()),
				Portnum:  portnumName(p),
				At:       r.clk.Now(),
			}
			select {
			case r.heard <- h:
			default:
				// A full census queue loses detail, not correctness: R comes
				// from telemetry, not from this.
			}
		}
	}
}

// portnumName labels traffic for the census, including what we cannot read.
func portnumName(p *meshpb.MeshPacket) string {
	if d := p.GetDecoded(); d != nil {
		return d.GetPortnum().String()
	}
	// Encrypted for a channel we hold no key for — still a packet on the air,
	// and still airtime, so it belongs in the census.
	return "ENCRYPTED"
}

// refreshLoop asks the node to re-send its state, which is the only way to get
// a fresh metrics reading: there is no on-demand telemetry request that the
// firmware answers (it replies to a self-addressed one with a routing error).
func (r *Radio) refreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.clk.After(r.refresh):
		}
		id := uint32(r.rnd.Uint64()) | 1
		if err := r.conn.Send(&meshpb.ToRadio{
			PayloadVariant: &meshpb.ToRadio_WantConfigId{WantConfigId: id},
		}); err != nil {
			return
		}
	}
}

func (r *Radio) record(d *meshtastic.DeviceMetrics) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest = Metrics{
		ChannelUtilization: float64(d.ChannelUtilization),
		AirUtilTx:          float64(d.AirUtilTx),
		Uptime:             time.Duration(d.UptimeSecs) * time.Second,
		At:                 r.clk.Now(),
	}
	r.have = true
}

// Metrics returns the latest reading.
func (r *Radio) Metrics() (Metrics, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latest, r.have
}

// Transmit puts one load packet on the air.
func (r *Radio) Transmit(ctx context.Context, n int, hopLimit uint32) error {
	r.mu.Lock()
	if cap(r.wbuf) < n {
		r.wbuf = make([]byte, n)
	}
	payload := r.wbuf[:n]
	// Fill from the injected source: a constant payload would compress
	// differently at other layers and is a poor stand-in for real traffic.
	for i := range payload {
		payload[i] = byte(r.rnd.Uint64())
	}
	id := uint32(r.rnd.Uint64())
	r.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	return r.conn.Send(&meshpb.ToRadio{PayloadVariant: &meshpb.ToRadio_Packet{
		Packet: &meshpb.MeshPacket{
			To:       0xFFFFFFFF,
			Channel:  r.channel,
			Id:       id,
			HopLimit: hopLimit,
			PayloadVariant: &meshpb.MeshPacket_Decoded{Decoded: &meshpb.Data{
				Portnum: meshpb.PortNum_PRIVATE_APP,
				Payload: payload,
			}},
		},
	}})
}

func (r *Radio) Heard() <-chan Heard           { return r.heard }
func (r *Radio) Preset() airtime.Preset        { return r.preset }
func (r *Radio) TelemetryIntervalSecs() uint32 { return r.telemS }

// Region is what the node reports, for the report header.
func (r *Radio) Region() string { return r.region }

var _ Node = (*Radio)(nil)
