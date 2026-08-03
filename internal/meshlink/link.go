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

	// errNotRetryable marks a connect failure that waiting cannot fix.
	//
	// The first connection retries, because a radio can be briefly unable to
	// accept one and that is not a misconfigured instance. But a channel the
	// node does not have, or an encrypted channel under an amateur licence, is
	// the same answer however many times it is asked — and §7.1 wants those in
	// front of the sysop at startup, not forty-five seconds later.
	errNotRetryable = errors.New("meshlink: not retryable")
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
	// AnswerInterval rate-limits answering who-is. Default 1m.
	//
	// Shorter than AskInterval because the two do different jobs: AskInterval
	// stops one unattributable peer from provoking a question per packet,
	// while this only has to collapse a burst of askers into a single
	// broadcast. Set it as long as AskInterval and a lost answer would leave
	// the asker waiting through a second ask cycle for no reason.
	AnswerInterval time.Duration
	// Heartbeat keeps the radio's client session alive. Default 5m.
	Heartbeat time.Duration
	// ConnectRetryWindow is how long Start keeps trying the first connection
	// before giving up. Default 45s; zero means one attempt.
	//
	// Long enough to outlast a Meshtastic node that has not yet released the
	// previous TCP client — about fifteen seconds on the bench — and short
	// enough that a genuinely absent radio still fails startup rather than
	// hanging a sysop who is waiting to find out.
	ConnectRetryWindow time.Duration
	// ConnectTimeout bounds the config handshake. Default 30s.
	//
	// The dump takes a second or two from a healthy radio, so this is only ever
	// reached by one that is not answering — and reaching it has to be possible,
	// or a reconnect can hang forever where a failure would have been retried.
	ConnectTimeout time.Duration
	// RxTimeout forces a reconnect when the radio has sent us nothing for this
	// long. Default 5m. Zero on a link that should never do this.
	//
	// It is deliberately far shorter than a digest interval. A stall that lasts
	// longer than one destroys a whole reconciliation round — the peer asks,
	// the deaf node never answers, and both sides wait out the next cadence —
	// so detection has to be quick relative to the protocol above, not merely
	// eventual. A radio reports its own telemetry about once a minute, which is
	// the cadence this is measured against.
	//
	// # Why a heartbeat is not enough
	//
	// Conn.Heartbeat is a write and nothing more: it proves the host can still
	// reach the radio and says nothing about the other direction. A USB serial
	// handle that has gone one-way therefore looks perfectly healthy — writes
	// succeed, the session is kept alive, transmissions go out and are received
	// by peers — while Recv blocks forever and the node is deaf.
	//
	// Nothing detects that. The supervisor reconnects when the read loop
	// returns an ERROR, and a Recv that simply never returns is not an error.
	// Observed on the bench: a node stopped hearing its own radio's telemetry
	// and went on transmitting digests for twenty-four minutes, while its peer
	// answered into a void.
	//
	// The timer is armed by ANY message from the radio, not just mesh packets —
	// local telemetry and queue status arrive regardless of whether the mesh is
	// busy — so a legitimately silent channel does not trip it. That matters:
	// the cost of a false positive is one reconnect, and the cost of a false
	// negative is an instance that is deaf until someone notices by hand.
	RxTimeout time.Duration
	// Inbox bounds buffered inbound datagrams. Default 256.
	Inbox int
	// ReconnectBackoff is the first delay after a dropped connection; it
	// doubles up to MaxReconnectBackoff. Default 1s.
	ReconnectBackoff time.Duration
	// MaxReconnectBackoff caps that doubling. Default 1m.
	//
	// It bounds how long a link can be down after the radio is healthy again,
	// and how much quiet a radio gets when it is not. Those pull in opposite
	// directions: the bench fault this escalation exists for — a node holding a
	// previous client's slots — cleared after about two minutes of being left
	// alone, so a ceiling far below that would keep poking a radio that wants
	// silence, while one far above it would leave a working radio unattached
	// for no reason.
	MaxReconnectBackoff time.Duration

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
//
// The three rx-side refusals are separate numbers on purpose. "Federation is
// quiet" has causes that live at different layers and need different fixes, and
// a single lumped drop count cannot tell them apart: Undecryptable is the radio
// failing to decrypt (a key problem, below us), WrongChannel is another mesh's
// traffic (nothing to fix), and Unattributed is our own peer we cannot yet name
// (a discovery problem, which the who-is exchange is meant to resolve on its
// own). A sysop reading a nonzero Undecryptable knows to go look at the radios,
// not at the sync engine.
type Stats struct {
	Sent          uint64
	Received      uint64
	Refused       uint64 // sends the governor declined
	Undecryptable uint64 // packets the radio could not decrypt for us
	WrongChannel  uint64 // packets on a channel index that is not ours
	Unattributed  uint64 // packets from a radio with no known node ID
	Reconnects    uint64
	RxStalls      uint64 // reconnects forced because the radio went quiet
	AnnouncesSent uint64
	PeersKnown    int
	// SinceRx is how long ago the radio last said anything. A value creeping up
	// past a minute or two on a live mesh is the shape of a one-way connection.
	SinceRx time.Duration
	// SerialSkipped is bytes the frame reader consumed outside any frame, and
	// SerialFrames is frames it read whole.
	//
	// # What Skipped does NOT mean
	//
	// It is tempting to read a large Skipped as corruption on the wire, and it
	// is not. Meshtastic firmware writes its plaintext debug log to the same
	// serial line as the binary protocol, so every log line the radio emits is
	// counted here on its way to the device-log sink. Tens of kilobytes against
	// a few dozen frames is the normal ratio for a chatty node, and it says
	// nothing at all about whether the bytes are arriving intact.
	//
	// Genuine desync is counted here too, and that is the problem: the two are
	// indistinguishable in this number. It is useful for "is this port carrying
	// anything at all" and for spotting a device that is not a Meshtastic node,
	// which is what internal/cli/mesh.go already uses it for. It is not
	// evidence about corruption, and reading it as such wasted an hour.
	//
	// What WOULD be evidence: the local link has no integrity check of its own
	// — the framing is `0x94 0xC3 <length> <payload>`, a firmware constant —
	// so damage inside a payload passes protobuf and reaches the application
	// looking valid. Detecting that needs a check at a layer that knows what
	// the bytes should be, which is what the bundle ID comparison in
	// internal/bsmp does.
	SerialSkipped uint64
	SerialFrames  uint64
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

	peers   *peerTable
	asks    *askRate
	answers *askRate
	inbox   chan link.Datagram

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

	sent, received, refused     atomic.Uint64
	unattributed, reconnects    atomic.Uint64
	undecryptable, wrongChannel atomic.Uint64
	announcesSent               atomic.Uint64
	rxStalls                    atomic.Uint64
	connected                   atomic.Bool
	// sessionRx reports whether the CURRENT connection has carried anything
	// through the read loop, as distinct from merely having been established.
	// The config handshake does not count: it is the one exchange a radio in a
	// bad state still completes.
	sessionRx atomic.Bool
	// lastRx is when the radio last said anything at all, in Unix nanoseconds.
	// Zero means nothing has arrived on this connection yet.
	lastRx atomic.Int64
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
	if cfg.AnswerInterval <= 0 {
		cfg.AnswerInterval = time.Minute
	}
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 5 * time.Minute
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 30 * time.Second
	}
	if cfg.ConnectRetryWindow < 0 {
		cfg.ConnectRetryWindow = 0
	} else if cfg.ConnectRetryWindow == 0 {
		cfg.ConnectRetryWindow = 45 * time.Second
	}
	if cfg.RxTimeout < 0 {
		cfg.RxTimeout = 0
	} else if cfg.RxTimeout == 0 {
		cfg.RxTimeout = 5 * time.Minute
	}
	if cfg.Inbox <= 0 {
		cfg.Inbox = 256
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}
	if cfg.MaxReconnectBackoff <= 0 {
		cfg.MaxReconnectBackoff = time.Minute
	}
	if cfg.MaxReconnectBackoff < cfg.ReconnectBackoff {
		cfg.MaxReconnectBackoff = cfg.ReconnectBackoff
	}
	return &Link{
		cfg:     cfg,
		self:    cfg.Key.ID(),
		clk:     cfg.Clock,
		peers:   newPeerTable(),
		asks:    newAskRate(cfg.AskInterval),
		answers: newAskRate(cfg.AnswerInterval),
		inbox:   make(chan link.Datagram, cfg.Inbox),
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

	if err := l.connectWithRetry(ctx); err != nil {
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

// connectWithRetry makes the first connection, tolerating a radio that is
// briefly unable to take one.
//
// # Why the first connect cannot simply fail
//
// A caller reads the radio's configuration before building the governor — the
// modem preset decides what airtime costs — and then this link dials the same
// radio again. Over a cable that second open is local and instant. Over TCP it
// is a new client session, and Meshtastic firmware does not free the previous
// one immediately: measured on the bench, a reconnect within a second of
// closing is reset by the node, and the same connection succeeds fifteen
// seconds later.
//
// Failing there is not a report of a misconfigured instance, it is a report of
// having been too quick, and it made `mode = "tcp"` unusable from startup —
// every run died on the second connection with "connection reset by peer" while
// a single probe worked every time.
//
// The window is bounded so a genuine fault still stops startup, which is the
// property §7.1 wanted: a wrong port or an absent channel should be an error the
// sysop sees now, not a silent federation outage. It just should not be reported
// for a radio that needed a moment.
func (l *Link) connectWithRetry(ctx context.Context) error {
	deadline := l.clk.Now().Add(l.cfg.ConnectRetryWindow)
	backoff := l.cfg.ReconnectBackoff
	for attempt := 1; ; attempt++ {
		err := l.connect(ctx)
		if err == nil {
			return nil
		}
		if errors.Is(err, errNotRetryable) || ctx.Err() != nil ||
			!l.clk.Now().Add(backoff).Before(deadline) {
			return err
		}
		l.event(fmt.Sprintf("radio did not accept the connection (attempt %d: %v); retrying in %v",
			attempt, err, backoff))
		select {
		case <-ctx.Done():
			return err
		case <-l.clk.After(backoff):
		}
		if backoff *= 2; backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

// connect dials, runs the config exchange, resolves the channel and announces.
func (l *Link) connect(ctx context.Context) error {
	conn, err := l.cfg.Dial(ctx)
	if err != nil {
		return err
	}

	// Bound the config exchange. Configure waits for a dump that a one-way
	// connection will never deliver, and it only gives up when its context does
	// — which, on the link's own lifetime context, is never. Without this the
	// rx watchdog below would be detection without recovery: it would close the
	// dead connection, redial, and hang here instead, having moved the stall
	// rather than cleared it.
	handshake := ctx
	if l.cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		handshake, cancel = context.WithTimeout(ctx, l.cfg.ConnectTimeout)
		defer cancel()
	}

	// Packets arriving mid-handshake are held, not delivered.
	//
	// Delivering them here would filter them against the channel index from the
	// PREVIOUS connection — zero on the first one — so the hook that exists to
	// stop a reconnect losing a fountain symbol would have discarded exactly
	// the packets it was added to keep.
	var pending []*meshpb.MeshPacket
	info, err := meshtastic.Configure(handshake, conn, meshtastic.ConfigRequest{
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
		return fmt.Errorf("%w: %w", errNotRetryable, err)
	}

	// §8.3: a ham-mode node may not run an encrypted channel. Checked here
	// rather than at config load because it needs the radio's own answer to
	// both questions, and a sysop can enable ham mode long after configuring
	// meshbbs.
	policy := hammode.FromRadio(info, l.cfg.Part97Override)
	if err := policy.CheckChannel(info.Channels, l.cfg.Channel); err != nil {
		conn.Close()
		return fmt.Errorf("%w: %w", errNotRetryable, err)
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
	// Arm the rx watchdog from the attach. The config handshake has just
	// finished, so we have demonstrably heard this radio; without this the timer
	// would not start until the first unsolicited message, and a radio that
	// never sent one would never be checked at all.
	l.lastRx.Store(l.clk.Now().UnixNano())
	// A new session has carried nothing yet. Deliberately NOT set by the
	// handshake above: a radio holding stale client slots still completes that
	// exchange and then goes silent, so counting it would call a dead session
	// healthy — which is the whole failure this flag exists to catch.
	l.sessionRx.Store(false)

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
	maxBackoff := l.cfg.MaxReconnectBackoff

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
		// Capture before reconnecting, because connect() arms the next session.
		delivered := l.sessionRx.Load()
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
				// Only a session that actually CARRIED something earns a reset.
				//
				// Connecting successfully is not evidence of a working link. A
				// Meshtastic node over TCP will complete the config handshake
				// and then deliver nothing at all on that session — which is
				// what a radio does when it has not released the client slots
				// of a previous connection. Treating that as a fresh healthy
				// start meant the backoff reset every cycle, so the rx watchdog
				// closed the dead session, redialled a second later, attached,
				// and waited out another timeout. Forever.
				//
				// Worse, the loop sustained the fault: every redial consumed
				// another slot on a radio that frees them slowly, so the
				// watchdog was holding the node in exactly the state it was
				// trying to escape. Observed on the bench, where the same radio
				// recovered immediately once left alone for two minutes.
				//
				// So a connection that attaches and delivers nothing escalates
				// like a failure, because that is what it is.
				if delivered {
					backoff = l.cfg.ReconnectBackoff
				} else if backoff *= 2; backoff > maxBackoff {
					backoff = maxBackoff
				}
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
	untilAnnounce := l.cfg.AnnounceInterval/2 + l.jitter(l.cfg.AnnounceInterval)
	untilHeartbeat := l.cfg.Heartbeat
	tick := l.watchdogTick()

	for {
		select {
		case <-ctx.Done():
			return
		case <-l.clk.After(tick):
		}

		// Bound to ONE connection: after a reconnect this goroutine must exit
		// rather than start heartbeating the new one alongside its successor.
		if conn == nil || l.currentConn() != conn {
			return
		}

		if untilHeartbeat -= tick; untilHeartbeat <= 0 {
			untilHeartbeat = l.cfg.Heartbeat
			if err := conn.Heartbeat(); err != nil {
				return // the read loop will see the same failure and reconnect
			}
		}

		// A heartbeat that succeeds says nothing about whether we can still
		// HEAR the radio, so check that separately and tear the connection down
		// ourselves if not. Closing is what turns a silent stall into the
		// ordinary error the supervisor already knows how to recover from.
		if l.rxStalled() {
			l.rxStalls.Add(1)
			l.event(fmt.Sprintf("radio has sent nothing for %v; reconnecting",
				l.cfg.RxTimeout))
			conn.Close()
			return
		}

		if untilAnnounce -= tick; untilAnnounce <= 0 {
			untilAnnounce = l.cfg.AnnounceInterval
			if err := l.announce(ctx); err != nil {
				l.event("periodic announce failed: " + err.Error())
			}
		}
	}
}

// watchdogTick is how often keepalive wakes.
//
// It is not the heartbeat interval, because the two want opposite things. The
// heartbeat only has to beat the firmware's idle timeout, measured in minutes,
// and every beat costs a write. The stall check wants to be prompt, and costs
// nothing. Waking on the slower of the two would round RxTimeout up to the next
// heartbeat, so a five-minute timeout would take up to ten minutes to notice —
// which is how a setting can look configured and not be actionable.
func (l *Link) watchdogTick() time.Duration {
	tick := l.cfg.Heartbeat
	if rt := l.cfg.RxTimeout; rt > 0 && rt/3 < tick {
		tick = rt / 3
	}
	if tick <= 0 {
		tick = time.Second
	}
	return tick
}

// rxStalled reports whether the radio has gone quiet for longer than allowed.
//
// A zero lastRx means nothing has arrived on this connection yet, which is the
// state every connection starts in — treating that as a stall would have the
// link tear down a radio it has not finished listening to. connect arms the
// timer instead, so the window is measured from the attach.
func (l *Link) rxStalled() bool {
	if l.cfg.RxTimeout <= 0 {
		return false
	}
	last := l.lastRx.Load()
	if last == 0 {
		return false
	}
	return l.clk.Now().Sub(time.Unix(0, last)) > l.cfg.RxTimeout
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
		// Arm the watchdog on ANY message, before filtering to mesh packets:
		// what this proves is that the radio is still talking to us, and local
		// telemetry proves that just as well as federation traffic does.
		l.lastRx.Store(l.clk.Now().UnixNano())
		l.sessionRx.Store(true)
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
		return fmt.Sprintf("rx from=0x%08x to=0x%08x ch=%d pki=%v port=%v %s len=%d",
			p.GetFrom(), p.GetTo(), p.GetChannel(), p.GetPkiEncrypted(),
			d.GetPortnum(), kind, len(d.GetPayload()))
	})
	if d == nil {
		// An `encrypted` payload is one the RADIO could not decrypt, and it is
		// worth being precise about why, because the obvious reading is wrong
		// often enough to cost an evening.
		//
		// The obvious reading is "traffic on a channel we hold no key for" —
		// someone else's mesh, not ours, nothing to do. That is one cause. The
		// other is a DIRECT MESSAGE encrypted to a key we do not hold: current
		// Meshtastic firmware encrypts DMs to the destination's public key
		// rather than the channel key, and refuses to replace a public key it
		// has already stored for a node. So two radios that have each other's
		// stale keys — after a reflash, say — go on exchanging broadcasts
		// perfectly while every DM between them arrives as an opaque blob.
		//
		// That failure is invisible from above: the channel is fine, the RF
		// path is fine, and the only symptom is that half the protocol never
		// happens. Hence the counter — a silent drop here was exactly the gap
		// that made one federation stall look like a sync bug for two hours.
		l.undecryptable.Add(1)
		l.drop("undecryptable: wrong channel key, or a DM encrypted to a key we do not hold")
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

	// The channel index is how we ignore other people's meshes — except that a
	// direct message legitimately arrives on a different one.
	//
	// Current Meshtastic firmware does not encrypt a DM with the channel key at
	// all. It encrypts to the recipient's public key and reports the packet on
	// the PKI channel index rather than the one the app is configured for, so a
	// plain `channel != ours` test rejects every DM ever sent to this node —
	// which on the bench meant digests arrived, were answered, and the answers
	// were dropped, with the whole sync half of the protocol silently missing.
	//
	// Accepting these does not widen the trust boundary; it narrows it. A
	// channel key is shared by everyone on the channel, so "arrived on our
	// channel" proves only membership. A PKI-encrypted packet addressed to us
	// was encrypted TO OUR KEY, which is a strictly stronger claim, and the
	// pairing with To is what keeps this from being "accept anything off
	// channel": both must hold. Everything after this still applies — the
	// sender is attributed to a node ID or the datagram goes nowhere.
	if p.GetChannel() != ours && !(p.GetPkiEncrypted() && p.GetTo() == self) {
		l.wrongChannel.Add(1)
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
		l.onWhoIs(payload, p.GetFrom(), self)
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

// onWhoIs answers a peer that could not attribute something we sent.
//
// # Why the answer is a broadcast
//
// It was a unicast once, on the reasoning that the asker is the only node that
// does not know, so telling everyone wastes airtime. That reasoning was wrong
// twice over, and the way it was wrong took two hours of a stalled federation
// to see.
//
// It is not cheaper. LoRa has no unicast: a direct message is the same
// transmission every neighbour already hears, plus a routing ACK coming back,
// plus — on current firmware — public-key encryption the broadcast does not
// pay for. The saving was imaginary.
//
// And it made discovery depend on the thing discovery exists to enable. An
// announcement is what makes a node addressable at all; sending it by the
// addressed path means any fault in that path — a stale public key, a NodeDB
// that will not take a correction — is unrecoverable BY CONSTRUCTION. Every
// repair attempt uses the broken mechanism, so the mesh cannot heal, and the
// symptom is not an error but silence. Observed on the bench: two nodes, RF
// fine, broadcasts flowing both ways, every DM between them undecryptable, and
// a who-is exchange that could never once complete.
//
// A broadcast answer has neither problem, and incidentally reaches any other
// node that also missed the announcement.
func (l *Link) onWhoIs(payload []byte, from, self uint32) {
	target, err := DecodeWhoIs(payload)
	if err != nil {
		l.event(fmt.Sprintf("rejected who-is from 0x%08x: %v", from, err))
		return
	}
	if target != self {
		// Someone else's question, overheard. Answering it would be the reply
		// storm the target field exists to prevent.
		l.drop(fmt.Sprintf("who-is for 0x%08x, we are 0x%08x", target, self))
		return
	}
	// Suppress globally rather than per asker: the answer goes to everyone, so
	// a second asker moments later has already been told. The single key is
	// what makes the window global.
	if !l.answers.allow(broadcastAddr, l.clk.Now()) {
		return
	}
	if err := l.announce(context.Background()); err != nil {
		l.event(fmt.Sprintf("could not answer who-is from 0x%08x: %v", from, err))
	}
}

// askWhoIs asks an unattributable radio to announce itself.
//
// Broadcast, for the same reason the answer is (see onWhoIs): both halves of
// this exchange have to work while direct messaging does not, or discovery
// cannot repair the one fault that stops everything. A question sent by the
// mechanism it is trying to bootstrap is not a recovery path.
//
// The rate limit stays keyed by TARGET, not by the broadcast: the thing worth
// limiting is how often we pester one silent radio, and that is unchanged by
// who else can overhear the asking.
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
	// No want_ack: on a broadcast every hearer would answer it (§7.1).
	if err := l.sendFrame(context.Background(), broadcastAddr, EncodeWhoIs(radio), false); err != nil {
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

// mtuReserve is what a payload must leave unused out of meshtastic.MTU.
//
// # Why the documented maximum is not the usable one
//
// meshtastic.MTU is `Data.payload max_size:233`, and taking it literally builds
// packets the firmware will not put on the air. The payload is not the thing
// that has to fit. The ENCODED Data message does, inside what remains of a LoRa
// frame after the mesh header — around 240 bytes. A 233-byte payload encodes to
// roughly 239 of those once its portnum, field tag and two-byte length prefix
// are counted: inside the budget by a single byte, and over it the moment the
// firmware populates any of Data's other fields, which it does routinely.
//
// So the failure is not marginal or probabilistic, it is total. Observed on the
// bench: EIGHT consecutive 233-byte symbol broadcasts, in both directions,
// none of which arrived — while 44- and 105-byte frames from the same two
// radios in the same minutes were delivered without exception. No layer
// reported anything. The link counted all eight as sent, the outbox advanced
// its cursor, and the receiving side simply never saw them. Every bundle this
// project has ever transmitted over a real mesh was lost this way.
//
// # Why eight and not sixteen
//
// Sixteen was the first value, chosen when the only two data points were "233
// never arrives" and "we need some margin". It worked, and it cost more than it
// looked like it did: at a reserve of sixteen the symbol payload is 204 bytes,
// and a bundle carrying a SINGLE record packs to about 212. So every bundle,
// however small, needed two symbols — and a two-symbol block is not twice as
// fragile as a one-symbol block, it is worse than that, because both halves
// must arrive rather than either one.
//
// Eight puts the symbol payload at 212 and one-record bundles back into a
// single symbol, where any one copy that lands decodes them. That is the whole
// reason for the number: it is not a byte-shaving exercise, it is the boundary
// between "needs two of three" and "needs one of three" for the commonest
// bundle on a mesh.
//
// It still leaves roughly six bytes of margin against the estimate above, and
// it is verified rather than reasoned: symbols at this size were transmitted and
// decoded over real radios before the value was committed. Anything larger
// should be measured the same way, because the failure mode is not a warning or
// a truncation — it is total, silent, and looks exactly like a quiet mesh.
const mtuReserve = 8

// MTU is what a payload may actually be.
//
// The link adds no header of its own: it reads the frame-type byte the layer
// above already writes rather than prepending a second one, which would cost a
// byte per fountain symbol — about 15 per bundle at K=15 — for no gain the
// design's byte budget (§12.7) would forgive. The reserve below is not that
// kind of header; it is room the FIRMWARE needs and does not ask for.
func (l *Link) MTU() int { return meshtastic.MTU - mtuReserve }

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
	// Read through the CURRENT connection: these counters live in the frame
	// reader, so they reset when the radio is redialled. That is the honest
	// reading — they describe this attachment, and a reconnect genuinely is a
	// new stream — but it means a stall that reconnects also zeroes the
	// evidence of why, which is worth knowing before reading a small number as
	// good news.
	var skipped, frames uint64
	if c := l.currentConn(); c != nil {
		skipped, frames = c.Skipped(), c.Frames()
	}
	return Stats{
		Sent:          l.sent.Load(),
		Received:      l.received.Load(),
		Refused:       l.refused.Load(),
		Undecryptable: l.undecryptable.Load(),
		WrongChannel:  l.wrongChannel.Load(),
		Unattributed:  l.unattributed.Load(),
		Reconnects:    l.reconnects.Load(),
		RxStalls:      l.rxStalls.Load(),
		SerialSkipped: skipped,
		SerialFrames:  frames,
		AnnouncesSent: l.announcesSent.Load(),
		PeersKnown:    len(l.peers.bindings()),
		SinceRx:       l.sinceRx(),
	}
}

// sinceRx reports how long ago the radio last spoke, or 0 if it never has.
func (l *Link) sinceRx() time.Duration {
	last := l.lastRx.Load()
	if last == 0 {
		return 0
	}
	return l.clk.Now().Sub(time.Unix(0, last))
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
