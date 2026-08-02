package meshlink

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"google.golang.org/protobuf/proto"
)

// fakeMesh is a set of virtual radios that hear each other.
//
// Every byte between a Link and its radio crosses the real framing and the real
// protobufs — the radios are fake, the wire is not. That matters because the
// two things most likely to be wrong here are the packet mapping and the
// channel/portnum filtering, and a mock that handed Links each other's
// datagrams directly would test neither.
type fakeMesh struct {
	t *testing.T

	mu     sync.Mutex
	radios map[uint32]*fakeRadio

	// drop, if set, decides which transmissions vanish before delivery. It is
	// how a test arranges for a peer to be heard before its announcement is,
	// which is the case the who-is exchange exists for.
	//
	// It sees the whole packet rather than just the payload because the
	// destination is the interesting axis for some faults: real radios lose
	// direct messages while broadcasts on the same channel keep flowing, and a
	// filter that could only match on payload bytes could not describe that.
	drop func(from uint32, pkt *meshpb.MeshPacket) bool
}

// setDrop installs (or clears) the delivery filter.
func (m *fakeMesh) setDrop(fn func(from uint32, pkt *meshpb.MeshPacket) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drop = fn
}

func newFakeMesh(t *testing.T) *fakeMesh {
	return &fakeMesh{t: t, radios: map[uint32]*fakeRadio{}}
}

type fakeRadio struct {
	mesh *fakeMesh
	num  uint32
	// channels is what this radio reports in its config dump.
	channels []meshtastic.ChannelInfo

	mu   sync.Mutex
	out  chan *meshpb.FromRadio
	conn net.Conn // radio side of the current connection
	// deaf drops everything on the way to the client without closing anything;
	// see setDeaf.
	deaf bool
	// dialled counts connections, so a test can assert a reconnect happened.
	dialled int
	// injected is delivered in the middle of the config dump, which is when a
	// real radio keeps handing over traffic it is hearing.
	injected []*meshpb.MeshPacket
	// licensed reports ham mode, which a radio carries in its own NodeInfo.
	licensed bool
}

// injectDuringConfig arranges for packets to arrive mid-handshake.
func (m *fakeMesh) injectDuringConfig(num uint32, pkts ...*meshpb.MeshPacket) {
	m.mu.Lock()
	r := m.radios[num]
	m.mu.Unlock()
	r.mu.Lock()
	r.injected = pkts
	r.mu.Unlock()
}

// defaultChannels: a stock primary plus the BBS channel §7.1 asks for.
func defaultChannels() []meshtastic.ChannelInfo {
	return []meshtastic.ChannelInfo{
		{Index: 0, Name: "", Role: "PRIMARY", Encrypted: true},
		{Index: 1, Name: "bbsnet", Role: "SECONDARY", Encrypted: true},
		{Index: 2, Role: "DISABLED"},
	}
}

// addLicensedRadio registers a radio whose operator has declared an amateur
// licence — Meshtastic's ham mode (§8.3).
func (m *fakeMesh) addLicensedRadio(num uint32, channels []meshtastic.ChannelInfo) Dialer {
	d := m.addRadio(num, channels)
	m.mu.Lock()
	m.radios[num].licensed = true
	m.mu.Unlock()
	return d
}

// addRadio registers a radio and returns a Dialer for it.
func (m *fakeMesh) addRadio(num uint32, channels []meshtastic.ChannelInfo) Dialer {
	r := &fakeRadio{mesh: m, num: num, channels: channels}
	m.mu.Lock()
	m.radios[num] = r
	m.mu.Unlock()

	return func(ctx context.Context) (*meshtastic.Conn, error) {
		client, server := net.Pipe()
		out := make(chan *meshpb.FromRadio, 64)

		r.mu.Lock()
		r.conn, r.out, r.dialled = server, out, r.dialled+1
		r.mu.Unlock()

		// Writes go through a queue so that one Link that has stopped reading
		// cannot block delivery to every other radio on the mesh.
		go r.writeLoop(server, out)
		go r.readLoop(server, out)
		return meshtastic.NewConn(client, meshtastic.Options{Name: "fake"}), nil
	}
}

// setDeaf makes a radio stop delivering anything to its client while still
// accepting everything the client writes.
//
// That asymmetry is the point: it is what a half-dead USB serial handle looks
// like, and it is invisible to every check that only writes. A test that closed
// the connection instead would exercise the error path the supervisor already
// handles, not the silent one.
func (m *fakeMesh) setDeaf(num uint32, deaf bool) {
	m.mu.Lock()
	r := m.radios[num]
	m.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	r.deaf = deaf
	r.mu.Unlock()
}

func (r *fakeRadio) writeLoop(conn net.Conn, out chan *meshpb.FromRadio) {
	for msg := range out {
		r.mu.Lock()
		deaf := r.deaf
		r.mu.Unlock()
		if deaf {
			continue // swallowed: the client will never hear this
		}
		body, err := proto.Marshal(msg)
		if err != nil {
			return
		}
		frame, err := meshtastic.AppendFrame(nil, body)
		if err != nil {
			return
		}
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

func (r *fakeRadio) readLoop(conn net.Conn, out chan *meshpb.FromRadio) {
	defer func() {
		conn.Close()
		// Closing under the same lock enqueue holds: another radio's transmit
		// can be delivering into this queue at the moment the connection ends,
		// and close-while-sending is a panic, not a race we can shrug at.
		r.mu.Lock()
		if r.out == out {
			close(out)
			r.out = nil
		}
		r.mu.Unlock()
	}()

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
		switch {
		case m.GetWantConfigId() != 0:
			r.sendConfig(out, m.GetWantConfigId())
		case m.GetPacket() != nil:
			r.mesh.transmit(r.num, m.GetPacket())
		}
	}
}

func (r *fakeRadio) sendConfig(out chan *meshpb.FromRadio, id uint32) {
	r.mu.Lock()
	licensed := r.licensed
	r.mu.Unlock()

	msgs := []*meshpb.FromRadio{
		{PayloadVariant: &meshpb.FromRadio_MyInfo{MyInfo: &meshpb.MyNodeInfo{MyNodeNum: r.num}}},
		{PayloadVariant: &meshpb.FromRadio_NodeInfo{NodeInfo: &meshpb.NodeInfo{
			Num:  r.num,
			User: &meshpb.User{IsLicensed: licensed},
		}}},
		{PayloadVariant: &meshpb.FromRadio_Metadata{Metadata: &meshpb.DeviceMetadata{
			FirmwareVersion: "2.7.15.fake", HwModel: meshpb.HardwareModel_HELTEC_V3,
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
	}
	for _, ch := range r.channels {
		role := meshpb.Channel_DISABLED
		switch ch.Role {
		case "PRIMARY":
			role = meshpb.Channel_PRIMARY
		case "SECONDARY":
			role = meshpb.Channel_SECONDARY
		}
		var psk []byte
		if ch.Encrypted {
			psk = []byte{0x01}
		}
		msgs = append(msgs, &meshpb.FromRadio{PayloadVariant: &meshpb.FromRadio_Channel{
			Channel: &meshpb.Channel{
				Index:    ch.Index,
				Role:     role,
				Settings: &meshpb.ChannelSettings{Name: ch.Name, Psk: psk},
			},
		}})
	}
	r.mu.Lock()
	injected := r.injected
	r.mu.Unlock()
	for _, p := range injected {
		msgs = append(msgs, &meshpb.FromRadio{PayloadVariant: &meshpb.FromRadio_Packet{Packet: p}})
	}

	msgs = append(msgs, &meshpb.FromRadio{
		PayloadVariant: &meshpb.FromRadio_ConfigCompleteId{ConfigCompleteId: id},
	})

	for _, msg := range msgs {
		r.enqueue(out, msg)
	}
}

// enqueue delivers to the radio's current queue, if it still has one.
//
// The send is non-blocking, so holding the lock cannot stall a delivery to
// another radio.
func (r *fakeRadio) enqueue(out chan *meshpb.FromRadio, msg *meshpb.FromRadio) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.out != out {
		return // this connection has ended
	}
	select {
	case out <- msg:
	default:
	}
}

// transmit delivers one packet to every radio that should hear it.
func (m *fakeMesh) transmit(from uint32, pkt *meshpb.MeshPacket) {
	m.mu.Lock()
	drop := m.drop
	targets := make([]*fakeRadio, 0, len(m.radios))
	for _, r := range m.radios {
		targets = append(targets, r)
	}
	m.mu.Unlock()

	if drop != nil && drop(from, pkt) {
		return
	}

	for _, r := range targets {
		if r.num == from {
			continue // a radio does not hear itself
		}
		if pkt.GetTo() != broadcastAddr && pkt.GetTo() != r.num {
			continue
		}
		// The firmware fills in the sender; a client never sets it.
		delivered := proto.Clone(pkt).(*meshpb.MeshPacket)
		delivered.From = from

		r.mu.Lock()
		out := r.out
		r.mu.Unlock()
		if out != nil {
			r.enqueue(out, &meshpb.FromRadio{
				PayloadVariant: &meshpb.FromRadio_Packet{Packet: delivered},
			})
		}
	}
}

// dropConnection kills a radio's current link, as an ESP32 losing WiFi does.
func (m *fakeMesh) dropConnection(num uint32) {
	m.mu.Lock()
	r := m.radios[num]
	m.mu.Unlock()
	if r == nil {
		return
	}
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

func (m *fakeMesh) dialCount(num uint32) int {
	m.mu.Lock()
	r := m.radios[num]
	m.mu.Unlock()
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dialled
}
