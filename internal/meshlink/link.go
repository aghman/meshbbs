package meshlink

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/governor"
	"github.com/aghman/meshbbs/internal/hammode"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"github.com/aghman/meshbbs/internal/rng"
)

// broadcastAddr is Meshtastic's "everyone" address.
const broadcastAddr uint32 = 0xFFFFFFFF

// Errors a caller can reasonably act on.
var (
	// ErrUnknownPeer means no radio address is bound to that node ID yet. It is
	// a temporary condition, not a permanent one: the peer becomes reachable as
	// soon as it announces, and the link asks whenever it hears an
	// unattributed radio.
	ErrUnknownPeer = errors.New("meshlink: no radio address known for peer")
	// ErrNotConnected means the radio is not currently attached. Reconnection
	// is automatic; the caller's send is not retried.
	ErrNotConnected = errors.New("meshlink: not connected to a radio")
	// ErrReservedFrame means a caller tried to send a frame type the link
	// itself owns.
	ErrReservedFrame = errors.New("meshlink: frame type is reserved for the link")
)

// Broadcast is the peer ID meaning "every listener on this mesh". It is
// link.Broadcast, re-exported so callers of this package need not import both.
func Broadcast() identity.NodeID { return link.Broadcast }

// Dialer opens a connection to the local radio. Serial and TCP both fit.
type Dialer func(ctx context.Context) (*meshtastic.Conn, error)

// Config configures a mesh link.
type Config struct {
	// Key is this instance's identity. It signs announcements.
	Key identity.NodeKey
	// Dial opens the radio connection, and is called again on every reconnect.
	Dial Dialer
	// Channel is the NAME of the Meshtastic channel carrying BBS traffic
	// (§7.1). The index is resolved from the radio's own config, because
	// indices are a local arrangement and names are what a sysop configures.
	Channel string
	// Governor decides what may be transmitted. Nil refuses everything.
	Governor Governor
	// Clock and Rand are injected per §12.1. Rand covers jitter and config
	// handshake IDs — anything a test may legitimately want to replay.
	Clock clock.Clock
	Rand  rng.Source

	// PacketIDs draws Meshtastic packet identifiers. It defaults to a source
	// seeded from system entropy, and MUST NOT be a fixed seed in production.
	//
	// # Why this is not just Rand
	//
	// Meshtastic firmware suppresses duplicates by (sender, packet_id) for
	// several minutes. A node that restarts and replays the same ID sequence
	// therefore has its traffic silently dropped by every peer that heard the
	// previous run — no error, no NAK, just an unreachable instance.
	//
	// This is not hypothetical: it is what the first two-radio bring-up ran
	// into. Two links with fixed seeds discovered each other on the very first
	// run and never again, because every later run reused the packet IDs the
	// radios had already seen. Deterministic seeding is exactly right for the
	// simulator (§12.1) and exactly wrong for packet IDs, so they get separate
	// sources.
	PacketIDs rng.Source

	// Part97Override is the sysop's acceptance of responsibility for
	// transmitting encrypted traffic under an amateur licence (§8.3). It is
	// plumbed rather than assumed because the check that matters — an encrypted
	// channel on a ham-mode node — can only be made once the radio has told us
	// both things, which is at connect time.
	Part97Override bool

	// HopLimit overrides the radio's own setting. Zero uses the radio's.
	//
	// §7.1 says set it explicitly and as low as the topology allows, because
	// hop limit multiplies R (§1.1) and therefore the airtime cost of
	// everything sent. The override is here so a BBS can be a better neighbour
	// than the node's default without changing that default for the sysop's
	// other traffic.
	HopLimit uint32

	// AnnounceInterval is how often to broadcast our radio binding.
	// Default 12h; announcements are also sent on connect and on request.
	AnnounceInterval time.Duration
	// AskInterval rate-limits asking one radio to identify itself. Default 15m.
	AskInterval time.Duration
	// Heartbeat keeps the radio's client session alive. Default 5m.
	Heartbeat time.Duration
	// Inbox bounds buffered inbound datagrams. Default 256.
	Inbox int
	// ReconnectBackoff is the first delay after a dropped connection; it
	// doubles up to a minute. Default 1s.
	ReconnectBackoff time.Duration

	// OnEvent receives one-line operational notes for the log: connects,
	// disconnects, peers learned. Optional.
	OnEvent func(string)
	// OnTrace, if set, reports EVERY packet the radio hands us and what became
	// of it.
	//
	// "Federation is not working" has too many possible causes to guess at from
	// the outside — wrong channel, wrong portnum, a peer we cannot attribute,
	// our own echo — and each looks identical from a silent inbox. This is how
	// a sysop tells them apart, and how the first two-radio bring-up was
	// debugged.
	OnTrace func(string)
}

// Stats are counters for the sysop status screen (§11.6).
type Stats struct {
	Sent          uint64
	Received      uint64
	Refused       uint64 // sends the governor declined
	Unattributed  uint64 // packets from a radio with no known node ID
	Reconnects    uint64
	AnnouncesSent uint64
	PeersKnown    int
}

// Link is a link.Link over a Meshtastic radio.
//
// # Concurrency
//
// A supervisor goroutine owns the connection and the read loop; Send is called
// from the sync engine's goroutine. That is the same arrangement as the IP link
// and for the same reason: §12.1's determinism constraint is about the
// SIMULATION being replayable, and concurrency belongs at the edges, in real
// transports.
type Link struct {
	cfg  Config
	self identity.NodeID
	clk  clock.Clock

	peers *peerTable
	asks  *askRate
	inbox chan link.Datagram

	mu       sync.Mutex
	conn     *meshtastic.Conn
	radio    *meshtastic.RadioInfo
	policy   hammode.Policy
	chanIdx  uint32
	hopLimit uint32
	closed   bool

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// randMu guards Rand. An injected rng.Source is a seeded PRNG, not a
	// concurrency-safe one, and three goroutines draw from it: sends, the read
	// loop answering a who-is, and the keepalive timer.
	randMu sync.Mutex

	sent, received, refused  atomic.Uint64
	unattributed, reconnects atomic.Uint64
	announcesSent            atomic.Uint64
	connected                atomic.Bool
}

// New builds a link that is not yet connected.
func New(cfg Config) (*Link, error) {
	if cfg.Dial == nil {
		return nil, errors.New("meshlink: no dialer configured")
	}
	if cfg.Channel == "" {
		return nil, errors.New("meshlink: no channel name configured")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
	}
	if cfg.Rand == nil {
		return nil, errors.New("meshlink: no random source configured")
	}
	if cfg.PacketIDs == nil {
		var seed [8]byte
		if _, err := rng.NewSecret().Read(seed[:]); err != nil {
			return nil, fmt.Errorf("meshlink: seeding packet IDs: %w", err)
		}
		cfg.PacketIDs = rng.NewSeeded(binary.BigEndian.Uint64(seed[:]))
	}
	if cfg.AnnounceInterval <= 0 {
		cfg.AnnounceInterval = 12 * time.Hour
	}
	if cfg.AskInterval <= 0 {
		cfg.AskInterval = 15 * time.Minute
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 5 * time.Minute
	}
	if cfg.Inbox <= 0 {
		cfg.Inbox = 256
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}
	return &Link{
		cfg:   cfg,
		self:  cfg.Key.ID(),
		clk:   cfg.Clock,
		peers: newPeerTable(),
		asks:  newAskRate(cfg.AskInterval),
		inbox: make(chan link.Datagram, cfg.Inbox),
	}, nil
}

// Start connects to the radio and begins supervising the connection.
//
// The first connection is synchronous so that a missing radio, a wrong port or
// an absent channel is an error the sysop sees at startup rather than a silent
// federation outage. Later failures are not: a real node drops its WiFi
// connection for a few seconds at a time — observed on the bench — and a BBS
// that exited on the first of those would be down more often than the radio is.
func (l *Link) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		cancel()
		return link.ErrClosed
	}
	l.cancel = cancel
	l.mu.Unlock()

	if err := l.connect(ctx); err != nil {
		cancel()
		return err
	}

	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.supervise(ctx)
	}()
	return nil
}

// connect dials, runs the config exchange, resolves the channel and announces.
func (l *Link) connect(ctx context.Context) error {
	conn, err := l.cfg.Dial(ctx)
	if err != nil {
		return err
	}

	// Packets arriving mid-handshake are held, not delivered.
	//
	// Delivering them here would filter them against the channel index from the
	// PREVIOUS connection — zero on the first one — so the hook that exists to
	// stop a reconnect losing a fountain symbol would have discarded exactly
	// the packets it was added to keep.
	var pending []*meshpb.MeshPacket
	info, err := meshtastic.Configure(ctx, conn, meshtastic.ConfigRequest{
		ID:       l.nextID() | 1,
		OnPacket: func(p *meshpb.MeshPacket) { pending = append(pending, p) },
	})
	if err != nil {
		conn.Close()
		return err
	}

	idx, err := channelIndex(info, l.cfg.Channel)
	if err != nil {
		conn.Close()
		return err
	}

	// §8.3: a ham-mode node may not run an encrypted channel. Checked here
	// rather than at config load because it needs the radio's own answer to
	// both questions, and a sysop can enable ham mode long after configuring
	// meshbbs.
	policy := hammode.FromRadio(info, l.cfg.Part97Override)
	if err := policy.CheckChannel(info.Channels, l.cfg.Channel); err != nil {
		conn.Close()
		return err
	}
	if b := policy.Banner(); b != "" {
		l.event(b)
	}

	hop := l.cfg.HopLimit
	if hop == 0 {
		hop = info.HopLimit
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		conn.Close()
		return link.ErrClosed
	}
	l.conn, l.radio, l.chanIdx, l.hopLimit = conn, info, idx, hop
	l.policy = policy
	l.mu.Unlock()
	l.connected.Store(true)

	l.event(fmt.Sprintf("radio attached: %s, node 0x%08x, channel %q (index %d), hop limit %d",
		conn.Name(), info.NodeNum, l.cfg.Channel, idx, hop))

	// Announce immediately. A node that has just come up is exactly the node
	// its peers cannot address yet.
	//
	// This only reaches peers that are already listening — the first node on a
	// mesh announces to nobody — and that is deliberate. Having every peer
	// answer a newcomer would rebuild §7.3's reply storm at the link layer, so
	// an established node is instead discovered on demand, when it transmits
	// and someone asks (see askWhoIs).
	if err := l.announce(ctx); err != nil {
		l.event("announce failed: " + err.Error())
	}

	// Now that the channel is resolved, the held packets can be filtered
	// correctly.
	for _, p := range pending {
		l.deliver(p)
	}
	return nil
}

// channelIndex resolves a channel name against what the radio reports.
func channelIndex(info *meshtastic.RadioInfo, name string) (uint32, error) {
	var available []string
	for _, ch := range info.Channels {
		if ch.Role == "DISABLED" {
			continue
		}
		if ch.Name == name {
			return uint32(ch.Index), nil
		}
		// A stock radio's primary channel carries the preset's default name and
		// reports it as empty, so listing names alone tells a sysop the radio
		// has no channels at all — which is wrong and unactionable.
		label := fmt.Sprintf("%d:%q", ch.Index, ch.Name)
		if ch.Name == "" {
			label = fmt.Sprintf("%d:(preset default)", ch.Index)
		}
		available = append(available, label)
	}
	// Naming the alternatives matters: the failure is nearly always that the
	// channel has not been created on the node yet, and the fix is in the
	// Meshtastic app, not in meshbbs.
	return 0, fmt.Errorf("meshlink: the radio has no enabled channel named %q\n"+
		"enabled channels are: %v\n"+
		"create %q in the Meshtastic app as a secondary channel", name, available, name)
}

// supervise runs the read loop and reconnects when it ends.
func (l *Link) supervise(ctx context.Context) {
	backoff := l.cfg.ReconnectBackoff
	const maxBackoff = time.Minute

	for {
		conn := l.currentConn()
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.keepalive(ctx, conn)
		}()

		err := l.readLoop(ctx)
		l.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		l.event("radio connection lost: " + errText(err))

		l.mu.Lock()
		if c := l.conn; c != nil {
			c.Close()
			l.conn = nil
		}
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-l.clk.After(backoff):
			}
			if err := l.connect(ctx); err == nil {
				l.reconnects.Add(1)
				backoff = l.cfg.ReconnectBackoff
				break
			} else {
				l.event("reconnect failed: " + err.Error())
				if backoff *= 2; backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}
}

// keepalive sends heartbeats and periodic announcements until the connection
// ends or the link closes.
func (l *Link) keepalive(ctx context.Context, conn *meshtastic.Conn) {
	// Jitter the announcement so fifty instances that all restarted after a
	// power cut do not announce in the same second (§7.3's digest storm, in
	// miniature).
	next := l.cfg.AnnounceInterval/2 + l.jitter(l.cfg.AnnounceInterval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-l.clk.After(l.cfg.Heartbeat):
		}

		// Bound to ONE connection: after a reconnect this goroutine must exit
		// rather than start heartbeating the new one alongside its successor.
		if conn == nil || l.currentConn() != conn {
			return
		}
		if err := conn.Heartbeat(); err != nil {
			return // the read loop will see the same failure and reconnect
		}

		if next -= l.cfg.Heartbeat; next <= 0 {
			next = l.cfg.AnnounceInterval
			if err := l.announce(ctx); err != nil {
				l.event("periodic announce failed: " + err.Error())
			}
		}
	}
}

// readLoop pumps received packets until the connection fails.
func (l *Link) readLoop(ctx context.Context) error {
	conn := l.currentConn()
	if conn == nil {
		return ErrNotConnected
	}
	for {
		msg, err := conn.Recv()
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p := msg.GetPacket(); p != nil {
			l.deliver(p)
		}
	}
}

// deliver turns one received packet into a datagram, or handles it here.
func (l *Link) deliver(p *meshpb.MeshPacket) {
	d := p.GetDecoded()
	l.trace(func() string {
		kind := "decoded"
		if d == nil {
			kind = fmt.Sprintf("encrypted(%dB)", len(p.GetEncrypted()))
		}
		return fmt.Sprintf("rx from=0x%08x ch=%d port=%v %s len=%d",
			p.GetFrom(), p.GetChannel(), d.GetPortnum(), kind, len(d.GetPayload()))
	})
	if d == nil {
		// An `encrypted` payload means the radio holds no key for that
		// channel. Not ours, and not something we can or should decrypt.
		l.drop("no key for that channel")
		return
	}
	if d.GetPortnum() != meshpb.PortNum_PRIVATE_APP {
		l.drop("not our portnum: " + d.GetPortnum().String())
		return
	}
	l.mu.Lock()
	ours := l.chanIdx
	self := uint32(0)
	if l.radio != nil {
		self = l.radio.NodeNum
	}
	l.mu.Unlock()

	if p.GetChannel() != ours {
		l.drop(fmt.Sprintf("channel %d, we are on %d", p.GetChannel(), ours))
		return
	}
	if p.GetFrom() == self {
		// Not noise: a packet of ours coming back is a rebroadcast, which is
		// the one thing about R this node can observe for itself (§7.6).
		if w, ok := l.cfg.Governor.(EchoWatcher); ok {
			w.NoteEcho()
		}
		l.drop("our own broadcast, heard back")
		return
	}
	payload := d.GetPayload()
	if len(payload) == 0 {
		l.drop("empty payload")
		return
	}

	switch payload[0] {
	case FrameAnnounce:
		l.onAnnounce(payload, p.GetFrom())
		return
	case FrameWhoIs:
		l.onWhoIs(p.GetFrom())
		return
	}

	from, ok := l.peers.idFor(p.GetFrom())
	if !ok {
		l.drop(fmt.Sprintf("no node ID known for radio 0x%08x yet", p.GetFrom()))
		// Undeliverable, because the layer above keys fountain decoder state on
		// the sender and derives repair masks from it (§7.2): a datagram
		// attributed to the wrong node ID would not merely be misfiled, it
		// would corrupt a decode. Ask who this is instead.
		l.unattributed.Add(1)
		l.askWhoIs(p.GetFrom())
		return
	}

	// Per-peer inbound quota (§7.6). A peer over quota is dropped here rather
	// than in the sync engine, so a flood costs us nothing above the radio.
	if lim, ok := l.cfg.Governor.(InboundLimiter); ok {
		if !lim.NoteInbound(from, len(payload)) {
			l.drop("peer is over its inbound quota")
			return
		}
	}

	l.received.Add(1)
	dg := link.Datagram{From: from, Data: payload, ReceivedAt: l.clk.Now()}
	select {
	case l.inbox <- dg:
	default:
		// A full inbox means the sync engine is not draining. Dropping is
		// honest: this is a datagram link and the protocol above already
		// handles loss, where an unbounded buffer would turn a backpressure bug
		// into an out-of-memory one.
	}
}

func (l *Link) onAnnounce(payload []byte, from uint32) {
	a, err := DecodeAnnounce(payload, from)
	if err != nil {
		l.event(fmt.Sprintf("rejected announcement from 0x%08x: %v", from, err))
		return
	}
	if a.ID == l.self {
		return
	}
	if err := l.peers.learn(a, l.clk.Now()); err != nil {
		return // stale or replayed; nothing to do and not worth a line
	}
	l.event(fmt.Sprintf("peer %s is at radio 0x%08x", a.ID.Short(), a.Radio))
}

func (l *Link) onWhoIs(from uint32) {
	// Answer by unicast: the asker is the only node that does not know.
	if err := l.sendFrame(context.Background(), from, l.announcement(), false); err != nil {
		l.event(fmt.Sprintf("could not answer who-is from 0x%08x: %v", from, err))
	}
}

func (l *Link) askWhoIs(radio uint32) {
	if !l.Connected() {
		// Check before taking a token: otherwise a packet held from the config
		// handshake spends the fifteen-minute allowance on a question that
		// could not be asked, and the peer stays unattributable for as long.
		return
	}
	if !l.asks.allow(radio, l.clk.Now()) {
		return
	}
	if err := l.sendFrame(context.Background(), radio, EncodeWhoIs(), true); err != nil {
		l.event(fmt.Sprintf("could not ask 0x%08x to identify itself: %v", radio, err))
	}
}

func (l *Link) announcement() []byte {
	l.mu.Lock()
	var num uint32
	if l.radio != nil {
		num = l.radio.NodeNum
	}
	l.mu.Unlock()
	return EncodeAnnounce(l.cfg.Key, num, l.clk.Now())
}

// Announce broadcasts this node's radio binding immediately.
//
// Exposed because a sysop who has just moved to new hardware should not have to
// wait up to twelve hours for peers to find them again, and because the
// connect-time announcement only reaches peers that were already listening.
func (l *Link) Announce(ctx context.Context) error { return l.announce(ctx) }

func (l *Link) announce(ctx context.Context) error {
	if err := l.sendFrame(ctx, broadcastAddr, l.announcement(), false); err != nil {
		return err
	}
	l.announcesSent.Add(1)
	return nil
}

func (l *Link) Name() string { return "mesh" }

// MTU is the Meshtastic application payload limit, unreduced.
//
// The link adds no header of its own: it reads the frame-type byte the layer
// above already writes rather than prepending a second one, which would cost a
// byte per fountain symbol — about 15 per bundle at K=15 — for no gain the
// design's byte budget (§12.7) would forgive.
func (l *Link) MTU() int { return meshtastic.MTU }

// Send transmits one datagram, subject to the governor.
//
// The priority class is inferred from the frame type, which is all this layer
// can see: a control frame is control traffic, and anything else is bulk. The
// sync engine knows more — whether a bundle is mail or a forum post — and says
// so through SendClass.
func (l *Link) Send(ctx context.Context, to identity.NodeID, payload []byte) error {
	class := governor.ClassForum
	if len(payload) > 0 && payload[0] == FrameControl {
		class = governor.ClassControl
	}
	return l.SendClass(ctx, to, payload, class)
}

// SendClass transmits one datagram at an explicit priority.
func (l *Link) SendClass(ctx context.Context, to identity.NodeID, payload []byte, class governor.Class) error {
	if len(payload) > l.MTU() {
		return link.ErrTooLarge
	}
	if len(payload) == 0 {
		return errors.New("meshlink: empty payload")
	}
	// A caller must not be able to forge a binding by writing an announcement
	// itself. The link owns these two frame types.
	if payload[0] == FrameAnnounce || payload[0] == FrameWhoIs {
		return fmt.Errorf("%w: %d", ErrReservedFrame, payload[0])
	}

	dest := broadcastAddr
	if to != link.Broadcast {
		radio, ok := l.peers.radioFor(to)
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownPeer, to.Short())
		}
		dest = radio
	}

	gov := l.cfg.Governor
	if gov == nil {
		return fmt.Errorf("%w: no airtime governor configured", link.ErrNoBudget)
	}
	// Check connectivity BEFORE charging: airtime spent on a packet that never
	// reached the radio is airtime the governor will refuse to a later one that
	// could have gone out.
	if !l.Connected() {
		return ErrNotConnected
	}
	if !gov.Allow(len(payload), class) {
		l.refused.Add(1)
		return link.ErrNoBudget
	}

	// want_ack only for small unicast control traffic, per §7.1: firmware
	// retries are limited, and asking for them on a broadcast would have every
	// hearer acknowledge — the digest storm with a different name.
	wantAck := dest != broadcastAddr && payload[0] == FrameControl
	if err := l.sendFrame(ctx, dest, payload, wantAck); err != nil {
		return err
	}
	l.sent.Add(1)
	return nil
}

// sendFrame puts one payload on the air. It bypasses the governor, and only
// link-owned traffic may call it that way: an announcement is what makes a node
// addressable at all, and a budget that silenced it would leave the instance
// unreachable with no way to recover.
func (l *Link) sendFrame(ctx context.Context, to uint32, payload []byte, wantAck bool) error {
	l.mu.Lock()
	conn, chanIdx, hop := l.conn, l.chanIdx, l.hopLimit
	l.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return conn.Send(&meshpb.ToRadio{PayloadVariant: &meshpb.ToRadio_Packet{
		Packet: &meshpb.MeshPacket{
			To:       to,
			Channel:  chanIdx,
			Id:       l.nextID(),
			HopLimit: hop,
			WantAck:  wantAck,
			PayloadVariant: &meshpb.MeshPacket_Decoded{Decoded: &meshpb.Data{
				Portnum: meshpb.PortNum_PRIVATE_APP,
				Payload: payload,
			}},
		},
	}})
}

func (l *Link) Recv() <-chan link.Datagram { return l.inbox }

// Budget reports what the governor currently allows.
func (l *Link) Budget() link.Budget {
	if l.cfg.Governor == nil {
		return link.Budget{Backpressure: true}
	}
	return l.cfg.Governor.Budget()
}

// Caps describes the mesh transport.
func (l *Link) Caps() link.Caps {
	return link.Caps{
		// One transmission reaches every listener, which is the property §7.2
		// builds the whole fountain-coding argument on.
		Broadcast: true,
		// want_ack exists but firmware retries are limited enough that treating
		// this as reliable would be a lie; L1 owns reliability.
		Reliable:    false,
		Ordered:     false,
		Addressable: true,
	}
}

// Connected reports whether a radio is currently attached.
func (l *Link) Connected() bool { return l.connected.Load() }

// Part97 reports the ham-mode policy in force, which is only known once the
// radio has been read.
func (l *Link) Part97() hammode.Policy {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.policy
}

// Radio returns what the attached node reported, or nil when disconnected.
func (l *Link) Radio() *meshtastic.RadioInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.radio
}

// Peers lists the learned node-ID-to-radio bindings.
func (l *Link) Peers() []Binding { return l.peers.bindings() }

// Stats reports counters for the status screen.
func (l *Link) Stats() Stats {
	return Stats{
		Sent:          l.sent.Load(),
		Received:      l.received.Load(),
		Refused:       l.refused.Load(),
		Unattributed:  l.unattributed.Load(),
		Reconnects:    l.reconnects.Load(),
		AnnouncesSent: l.announcesSent.Load(),
		PeersKnown:    len(l.peers.bindings()),
	}
}

// Close stops the link and closes the Recv channel.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	conn, cancel := l.conn, l.cancel
	l.conn = nil
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		conn.Close()
	}
	l.wg.Wait()
	l.connected.Store(false)
	close(l.inbox)
	return nil
}

// nextID draws a packet identifier. See Config.PacketIDs for why this does not
// come from Rand.
func (l *Link) nextID() uint32 {
	l.randMu.Lock()
	defer l.randMu.Unlock()
	return uint32(l.cfg.PacketIDs.Uint64())
}

// jitter returns a random duration in [0, d).
func (l *Link) jitter(d time.Duration) time.Duration {
	secs := int(d / time.Second)
	if secs < 1 {
		secs = 1
	}
	l.randMu.Lock()
	defer l.randMu.Unlock()
	return time.Duration(l.cfg.Rand.IntN(secs)) * time.Second
}

func (l *Link) currentConn() *meshtastic.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

func (l *Link) event(s string) {
	if l.cfg.OnEvent != nil {
		l.cfg.OnEvent(s)
	}
}

// trace takes a closure so the formatting cost is not paid when tracing is off,
// which matters on a path that runs for every packet on the channel.
func (l *Link) trace(f func() string) {
	if l.cfg.OnTrace != nil {
		l.cfg.OnTrace(f())
	}
}

func (l *Link) drop(why string) { l.trace(func() string { return "  dropped: " + why }) }

func errText(err error) string {
	if err == nil {
		return "stream ended"
	}
	return err.Error()
}

// Compile-time proof that the mesh is just another Link (§3).
var _ link.Link = (*Link)(nil)
