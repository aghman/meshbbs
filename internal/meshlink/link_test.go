package meshlink

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/governor"
	"github.com/aghman/meshbbs/internal/hammode"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"github.com/aghman/meshbbs/internal/rng"
)

func nodeKey(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// startLink attaches a Link to a radio on the mesh and connects it.
func startLink(t *testing.T, m *fakeMesh, key identity.NodeKey, radio uint32, opts ...func(*Config)) *Link {
	t.Helper()
	cfg := Config{
		Key:              key,
		Dial:             m.addRadio(radio, defaultChannels()),
		Channel:          "bbsnet",
		Governor:         Unmetered{},
		Rand:             rng.NewSeeded(uint64(radio)),
		ReconnectBackoff: 10 * time.Millisecond,
		AskInterval:      time.Millisecond,
	}
	for _, o := range opts {
		o(&cfg)
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func recvWithin(t *testing.T, l *Link, d time.Duration) (link.Datagram, bool) {
	t.Helper()
	select {
	case dg, ok := <-l.Recv():
		return dg, ok
	case <-time.After(d):
		return link.Datagram{}, false
	}
}

// waitFor polls until cond holds, for tests where the interesting event is
// produced by a goroutine reading a socket.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle has every link announce while the others are listening, and waits for
// mutual attribution.
//
// Tests need this because the connect-time announcement only reaches peers that
// are ALREADY up — the first node on a mesh announces to nobody. That is not a
// defect: §7.3's lesson about reply storms rules out having every peer answer a
// newcomer, so discovery for an established node is demand-driven through
// who-is instead (see TestUnattributedSenderIsAskedToIdentifyItself). This
// helper stands in for the periodic announcement that would do the same job
// hours later.
func settle(t *testing.T, links ...*Link) {
	t.Helper()
	for _, l := range links {
		if err := l.announce(context.Background()); err != nil {
			t.Fatalf("announce: %v", err)
		}
	}
	waitFor(t, 3*time.Second, "every link to learn every other", func() bool {
		for _, l := range links {
			for _, other := range links {
				if l == other {
					continue
				}
				if _, ok := l.peers.radioFor(other.self); !ok {
					return false
				}
			}
		}
		return true
	})
}

// The baseline: two instances learn each other's radio addresses from signed
// announcements, then exchange a datagram addressed by node ID. Nothing is
// configured on either side but the channel name.
func TestTwoInstancesLearnEachOtherAndExchange(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := startLink(t, m, ka, 0xAAAA0001)
	b := startLink(t, m, kb, 0xBBBB0002)
	settle(t, a, b)

	// Unicast, addressed by node ID and routed by the learned binding.
	if err := a.Send(context.Background(), kb.ID(), []byte{FrameControl, 'h', 'i'}); err != nil {
		t.Fatalf("send: %v", err)
	}
	dg, ok := recvWithin(t, b, 2*time.Second)
	if !ok {
		t.Fatal("b received nothing")
	}
	if dg.From != ka.ID() {
		t.Errorf("From = %s, want %s", dg.From.Short(), ka.ID().Short())
	}
	if string(dg.Data) != string([]byte{FrameControl, 'h', 'i'}) {
		t.Errorf("payload = %x", dg.Data)
	}
}

// Broadcast is the normal case on a mesh (§7.2) and must reach every peer with
// the sender attributed correctly.
func TestBroadcastReachesEveryPeer(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb, kc := nodeKey(t, 1), nodeKey(t, 2), nodeKey(t, 3)
	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)
	c := startLink(t, m, kc, 0xC3)
	settle(t, a, b, c)

	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 0x01}); err != nil {
		t.Fatalf("send: %v", err)
	}
	for name, l := range map[string]*Link{"b": b, "c": c} {
		dg, ok := recvWithin(t, l, 2*time.Second)
		if !ok {
			t.Fatalf("%s received nothing", name)
		}
		if dg.From != ka.ID() {
			t.Errorf("%s attributed the broadcast to %s", name, dg.From.Short())
		}
	}
}

// The case the who-is exchange exists for: a peer is heard before its
// announcement is. The datagram is dropped — attributing it to the wrong node
// would corrupt a fountain decode (§7.2) — and the link asks, unprompted.
func TestUnattributedSenderIsAskedToIdentifyItself(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)

	// Swallow announcements while the links come up, so neither side learns
	// the other the easy way.
	m.setDrop(func(from uint32, pkt *meshpb.MeshPacket) bool {
		payload := pkt.GetDecoded().GetPayload()
		return len(payload) > 0 && payload[0] == FrameAnnounce
	})

	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)

	if _, ok := b.peers.radioFor(ka.ID()); ok {
		t.Fatal("b learned a despite the announcement being dropped")
	}
	m.setDrop(nil)

	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 0x42}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// b cannot attribute it, asks, a answers, and b learns.
	waitFor(t, 3*time.Second, "b to learn a from the who-is exchange", func() bool {
		_, ok := b.peers.radioFor(ka.ID())
		return ok
	})
	if b.Stats().Unattributed == 0 {
		t.Error("the unattributed packet was not counted")
	}

	// And now traffic flows.
	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 0x43}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if _, ok := recvWithin(t, b, 2*time.Second); !ok {
		t.Fatal("b received nothing after learning a")
	}
}

// Discovery must not depend on the addressing it exists to establish.
//
// The bench failure this is taken from: two nodes on one channel, RF fine,
// broadcasts flowing both ways, and every direct message between them arriving
// undecryptable because the radios held stale public keys for each other. Both
// halves of the who-is exchange were direct messages, so the one mechanism that
// could have repaired the binding was the one mechanism that could not run. The
// federation sat silent for hours with every counter reading zero.
//
// Losing all unicast is a harsh fault, and deliberately so: it is the exact
// shape of the real one, and the property worth holding is that discovery
// completes anyway.
func TestDiscoverySurvivesTotalUnicastLoss(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)

	// Swallow announcements while the links come up, so neither learns the
	// other the easy way — then leave every direct message dropped for good.
	m.setDrop(func(from uint32, pkt *meshpb.MeshPacket) bool {
		payload := pkt.GetDecoded().GetPayload()
		return len(payload) > 0 && payload[0] == FrameAnnounce
	})

	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)

	m.setDrop(func(from uint32, pkt *meshpb.MeshPacket) bool {
		return pkt.GetTo() != broadcastAddr
	})

	if _, ok := b.peers.radioFor(ka.ID()); ok {
		t.Fatal("b learned a despite the announcement being dropped")
	}

	// a broadcasts something b cannot attribute. b asks, a answers, both by
	// broadcast, and the binding is made over a mesh with no working unicast.
	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 0x42}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, 3*time.Second, "b to learn a with every unicast lost", func() bool {
		_, ok := b.peers.radioFor(ka.ID())
		return ok
	})
}

// A who-is names one radio, and only that radio may answer it. Without this the
// broadcast question would draw an answer from every node that heard it, which
// is §7.3's reply storm reached by a different route.
func TestWhoIsForAnotherRadioIsIgnored(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb, kc := nodeKey(t, 1), nodeKey(t, 2), nodeKey(t, 3)

	m.setDrop(func(from uint32, pkt *meshpb.MeshPacket) bool {
		payload := pkt.GetDecoded().GetPayload()
		return len(payload) > 0 && payload[0] == FrameAnnounce
	})
	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)
	c := startLink(t, m, kc, 0xC3)
	m.setDrop(nil)

	// b asks about a. c hears the question too, and must sit on its hands.
	beforeA, beforeC := a.Stats().AnnouncesSent, c.Stats().AnnouncesSent
	b.askWhoIs(0xA1)

	waitFor(t, 3*time.Second, "b to learn a", func() bool {
		_, ok := b.peers.radioFor(ka.ID())
		return ok
	})
	if got := a.Stats().AnnouncesSent; got == beforeA {
		t.Error("a did not answer the who-is aimed at it")
	}
	if got := c.Stats().AnnouncesSent; got != beforeC {
		t.Errorf("c answered a who-is aimed at a: announces went %d -> %d", beforeC, got)
	}
	if _, ok := b.peers.radioFor(kc.ID()); ok {
		t.Error("b learned c, which never should have spoken")
	}
}

// A radio that drops its connection — which real hardware does — must not end
// the federation. The link reconnects and keeps working.
func TestLinkReconnectsAfterTheRadioDropsOut(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)
	settle(t, a, b)

	m.dropConnection(0xA1)
	// Wait on the counter, not on the dial count: the radio is dialled and the
	// link marked connected a moment before the reconnect is tallied, so
	// waiting on the former and asserting the latter is a race in the test.
	waitFor(t, 5*time.Second, "a to reconnect", func() bool {
		return a.Stats().Reconnects > 0 && a.Connected()
	})
	if m.dialCount(0xA1) < 2 {
		t.Errorf("radio was dialled %d times, want at least 2", m.dialCount(0xA1))
	}

	if err := a.Send(context.Background(), kb.ID(), []byte{FrameControl, 'x'}); err != nil {
		t.Fatalf("send after reconnect: %v", err)
	}
	if _, ok := recvWithin(t, b, 3*time.Second); !ok {
		t.Fatal("nothing arrived after the reconnect")
	}
}

// The channel index says "this is our mesh" for broadcasts, but a direct
// message does not arrive on our channel at all: firmware encrypts a DM to the
// recipient's public key and reports it on the PKI channel index. Rejecting on
// the index alone therefore discards every DM ever sent to this node.
//
// On the bench that was the whole sync half of the protocol: digests arrived as
// broadcasts and were answered, and every answer was dropped unread. The one
// visible symptom was a counter this test's subject increments.
func TestDirectMessagesAreAcceptedOffChannel(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)
	settle(t, a, b)

	pkt := func(ch uint32, to uint32, pki bool) *meshpb.MeshPacket {
		return &meshpb.MeshPacket{
			From: 0xA1, To: to, Channel: ch, PkiEncrypted: pki,
			PayloadVariant: &meshpb.MeshPacket_Decoded{Decoded: &meshpb.Data{
				Portnum: meshpb.PortNum_PRIVATE_APP,
				Payload: []byte{FrameControl, 'x'},
			}},
		}
	}

	for _, tc := range []struct {
		name string
		pkt  *meshpb.MeshPacket
		want bool
	}{
		{"on our channel", pkt(1, broadcastAddr, false), true},
		{"off channel, not a DM", pkt(0, broadcastAddr, false), false},
		{"off channel, PKI DM to us", pkt(0, 0xB2, true), true},
		// The pairing matters: PKI alone must not be a way past the filter, or
		// anything off-channel could get in by setting one flag.
		{"off channel, PKI DM to someone else", pkt(0, 0xC3, true), false},
		{"off channel, addressed to us but not PKI", pkt(0, 0xB2, false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := b.Stats().WrongChannel
			b.deliver(tc.pkt)
			_, got := recvWithin(t, b, 200*time.Millisecond)
			if got != tc.want {
				t.Errorf("delivered=%v, want %v", got, tc.want)
			}
			if dropped := b.Stats().WrongChannel > before; dropped == tc.want {
				t.Errorf("wrong_channel counter moved=%v with delivered=%v", dropped, got)
			}
		})
	}
}

// A connection can fail in one direction only, and nothing that writes will
// notice. Conn.Heartbeat is a write: it proves the host can reach the radio and
// says nothing about whether the radio can still reach the host.
//
// Taken from the bench, where a node stopped receiving — its own radio's
// telemetry included — and went on transmitting digests for twenty-four minutes
// while its peer answered into a void. The supervisor never acted because it
// only reconnects on a read ERROR, and a read that never returns is not one.
func TestOneWayConnectionForcesAReconnect(t *testing.T) {
	m := newFakeMesh(t)
	a := startLink(t, m, nodeKey(t, 1), 0xA1, func(c *Config) {
		c.Heartbeat = 20 * time.Millisecond
		c.RxTimeout = 60 * time.Millisecond
		c.ConnectTimeout = 100 * time.Millisecond
	})
	waitFor(t, 2*time.Second, "a to connect", a.Connected)
	dialsBefore := m.dialCount(0xA1)

	// The radio goes one-way: it accepts everything, delivers nothing.
	m.setDeaf(0xA1, true)

	waitFor(t, 3*time.Second, "the watchdog to notice the silence", func() bool {
		return a.Stats().RxStalls > 0
	})
	// Wait on the redial rather than asserting it: the watchdog only closes the
	// connection, and the supervisor still has a backoff to serve before it
	// dials again. Asserting straight after the counter moves is a race.
	waitFor(t, 3*time.Second, "the supervisor to redial", func() bool {
		return m.dialCount(0xA1) > dialsBefore
	})

	// And it recovers on its own once the radio is answering again — detecting
	// the stall is only half the job if the link cannot come back from it.
	m.setDeaf(0xA1, false)
	waitFor(t, 5*time.Second, "a to reconnect", a.Connected)
}

// The stall check has to wake more often than the heartbeat, or RxTimeout is
// rounded up to the next beat and a setting that reads as five minutes takes
// ten to act. Configured is not the same as actionable.
func TestWatchdogWakesFasterThanTheHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heartbeat time.Duration
		rxTimeout time.Duration
		want      time.Duration
	}{
		{"tick follows the timeout when it is the tighter one", 5 * time.Minute, 5 * time.Minute, 100 * time.Second},
		{"never slower than needed to catch the timeout", 5 * time.Minute, time.Minute, 20 * time.Second},
		{"heartbeat caps it when the timeout is loose", time.Minute, time.Hour, time.Minute},
		{"disabled watchdog just uses the heartbeat", time.Minute, -1, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &Link{cfg: Config{Heartbeat: tc.heartbeat, RxTimeout: tc.rxTimeout}}
			if got := l.watchdogTick(); got != tc.want {
				t.Errorf("watchdogTick() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Turning the watchdog off has to be sayable. Zero is what an unset Config
// field looks like, so the link reads it as "give me the default" — which means
// a sysop writing rx_timeout_secs = 0 would otherwise get the default back
// rather than the disabling they asked for.
func TestTheWatchdogCanBeTurnedOff(t *testing.T) {
	l, err := New(Config{
		Key: nodeKey(t, 1), Channel: "bbsnet", Rand: rng.NewSeeded(1),
		Dial:      func(context.Context) (*meshtastic.Conn, error) { return nil, errors.New("unused") },
		RxTimeout: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if l.cfg.RxTimeout > 0 {
		t.Errorf("a negative RxTimeout became %v, want it left off", l.cfg.RxTimeout)
	}
	// Stale by any measure, and still not a stall, because the check is off.
	l.lastRx.Store(time.Now().Add(-24 * time.Hour).UnixNano())
	if l.rxStalled() {
		t.Error("a disabled watchdog reported a stall")
	}
}

// A quiet mesh is not a broken one. The watchdog is armed by any message from
// the radio, including its own local telemetry, so a link with nothing to say
// must not tear itself down on a schedule.
func TestAQuietRadioIsNotMistakenForADeadOne(t *testing.T) {
	m := newFakeMesh(t)
	a := startLink(t, m, nodeKey(t, 1), 0xA1, func(c *Config) {
		c.Heartbeat = 10 * time.Millisecond
		c.RxTimeout = 50 * time.Millisecond
	})
	waitFor(t, 2*time.Second, "a to connect", a.Connected)

	// No peers, no traffic — but the radio is still answering, which is what
	// the watchdog actually measures.
	b := startLink(t, m, nodeKey(t, 2), 0xB2)
	for i := 0; i < 10; i++ {
		if err := b.announce(context.Background()); err != nil {
			t.Fatalf("announce: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := a.Stats().RxStalls; n != 0 {
		t.Errorf("watchdog fired %d times on a live but quiet link", n)
	}
}

// A radio does not stop hearing traffic while it dumps its configuration, and
// on a reconnect that traffic is federation traffic. Holding those packets is
// only useful if they are then filtered against the RIGHT channel — the index
// is not known until the dump finishes.
func TestPacketsArrivingDuringTheHandshakeAreKept(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)

	// An announcement from a, presented to b's radio mid-handshake, on the BBS
	// channel (index 1) rather than the primary.
	// Dated now, not at a fixed instant: the link runs on the real clock here,
	// and the peer table rejects an announcement more than a day ahead of it.
	announce := EncodeAnnounce(ka, 0xA1, time.Now())
	m.addRadio(0xA1, defaultChannels())
	dial := m.addRadio(0xB2, defaultChannels())
	m.injectDuringConfig(0xB2, &meshpb.MeshPacket{
		From:    0xA1,
		To:      broadcastAddr,
		Channel: 1,
		PayloadVariant: &meshpb.MeshPacket_Decoded{Decoded: &meshpb.Data{
			Portnum: meshpb.PortNum_PRIVATE_APP,
			Payload: announce,
		}},
	})

	b, err := New(Config{
		Key: kb, Dial: dial, Channel: "bbsnet", Governor: Unmetered{},
		Rand: rng.NewSeeded(2), ReconnectBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })

	waitFor(t, 2*time.Second, "b to have kept the packet from the handshake", func() bool {
		_, ok := b.peers.radioFor(ka.ID())
		return ok
	})
}

func TestSendRejections(t *testing.T) {
	m := newFakeMesh(t)
	ka := nodeKey(t, 1)
	a := startLink(t, m, ka, 0xA1)
	unknown := nodeKey(t, 99).ID()

	if err := a.Send(context.Background(), link.Broadcast, make([]byte, 234)); !errors.Is(err, link.ErrTooLarge) {
		t.Errorf("oversize: err = %v, want ErrTooLarge", err)
	}
	if err := a.Send(context.Background(), unknown, []byte{FrameControl}); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("unknown peer: err = %v, want ErrUnknownPeer", err)
	}
	// Nothing above the link may forge a binding by writing an announcement.
	for _, reserved := range []byte{FrameAnnounce, FrameWhoIs} {
		if err := a.Send(context.Background(), link.Broadcast, []byte{reserved, 1, 2}); !errors.Is(err, ErrReservedFrame) {
			t.Errorf("frame %d: err = %v, want ErrReservedFrame", reserved, err)
		}
	}
}

// §7.6 makes the governor civic infrastructure. A link without one must not
// transmit at all — the permissive default is the dangerous one.
func TestNoGovernorMeansNoTransmission(t *testing.T) {
	m := newFakeMesh(t)
	ka := nodeKey(t, 1)
	a := startLink(t, m, ka, 0xA1, func(c *Config) { c.Governor = nil })

	err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 1})
	if !errors.Is(err, link.ErrNoBudget) {
		t.Fatalf("err = %v, want ErrNoBudget", err)
	}
	if !a.Budget().Backpressure {
		t.Error("Budget() should report backpressure with no governor")
	}
}

// refusing is a Governor that declines everything, as a real one does when the
// window is spent.
type refusing struct{}

func (refusing) Allow(int, governor.Class) bool { return false }
func (refusing) Budget() link.Budget            { return link.Budget{Backpressure: true} }

func TestGovernorRefusalIsReportedAndCounted(t *testing.T) {
	m := newFakeMesh(t)
	ka := nodeKey(t, 1)
	a := startLink(t, m, ka, 0xA1, func(c *Config) { c.Governor = refusing{} })

	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 1}); !errors.Is(err, link.ErrNoBudget) {
		t.Fatalf("err = %v, want ErrNoBudget", err)
	}
	if a.Stats().Refused != 1 {
		t.Errorf("Refused = %d, want 1", a.Stats().Refused)
	}
	// The announcement still went out: it is what makes this node addressable,
	// and a budget that silenced it would leave the instance unreachable.
	if a.Stats().AnnouncesSent == 0 {
		t.Error("the connect-time announcement was suppressed")
	}
}

// The channel is named, not numbered, because indices are a local arrangement.
// A missing channel must say so in terms a sysop can act on.
func TestMissingChannelIsAnActionableError(t *testing.T) {
	m := newFakeMesh(t)
	ka := nodeKey(t, 1)
	l, err := New(Config{
		Key:      ka,
		Dial:     m.addRadio(0xA1, defaultChannels()),
		Channel:  "notthere",
		Governor: Unmetered{},
		Rand:     rng.NewSeeded(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Start(context.Background())
	if err == nil {
		t.Fatal("started against a radio with no such channel")
	}
	if !strings.Contains(err.Error(), "bbsnet") || !strings.Contains(err.Error(), "Meshtastic app") {
		t.Errorf("error does not tell the sysop what to do: %v", err)
	}
}

func TestCapsAndMTU(t *testing.T) {
	m := newFakeMesh(t)
	a := startLink(t, m, nodeKey(t, 1), 0xA1)

	// Deliberately NOT meshtastic.MTU. That constant is Data.payload's
	// documented maximum, and a frame built to it does not reach the air: what
	// has to fit is the encoded Data message, inside a LoRa frame with about
	// 240 bytes left after the mesh header. Eight consecutive full-size symbol
	// broadcasts were lost on the bench, in both directions, with no error
	// anywhere, before this reserve existed.
	//
	// The assertion is on the headroom rather than on 217, so that tuning the
	// reserve does not have to come here — but shrinking it to nothing would.
	if got, ceiling := a.MTU(), meshtastic.MTU; got >= ceiling {
		t.Errorf("MTU = %d, want strictly below the protocol maximum %d", got, ceiling)
	}
	if got, want := meshtastic.MTU-a.MTU(), 8; got < want {
		t.Errorf("MTU reserve is %d bytes, want at least %d for the Data wrapper", got, want)
	}
	if a.MTU() < 200 {
		t.Errorf("MTU = %d: the reserve has eaten more than it should", a.MTU())
	}
	caps := a.Caps()
	if !caps.Broadcast {
		t.Error("Broadcast must be true: one transmission reaches everyone (§7.2)")
	}
	if caps.Reliable {
		t.Error("Reliable must be false: want_ack is not a reliable transport (§7.1)")
	}
	if !caps.Addressable {
		t.Error("Addressable must be true")
	}
}

// Traffic on another channel, or another app's portnum, belongs to someone else
// sharing the same radio.
func TestOtherChannelsAndPortnumsAreIgnored(t *testing.T) {
	m := newFakeMesh(t)
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := startLink(t, m, ka, 0xA1)
	b := startLink(t, m, kb, 0xB2)
	settle(t, a, b)

	// Same payload, wrong channel index.
	a.mu.Lock()
	a.chanIdx = 0
	a.mu.Unlock()
	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 9}); err != nil {
		t.Fatal(err)
	}
	if _, ok := recvWithin(t, b, 300*time.Millisecond); ok {
		t.Error("b accepted a packet from a different channel")
	}
}

func TestCloseIsIdempotentAndClosesRecv(t *testing.T) {
	m := newFakeMesh(t)
	a := startLink(t, m, nodeKey(t, 1), 0xA1)

	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, ok := <-a.Recv(); ok {
		t.Error("Recv channel is still open after Close")
	}
	if err := a.Send(context.Background(), link.Broadcast, []byte{FrameSymbol, 1}); err == nil {
		t.Error("Send succeeded after Close")
	}
}

// Packet IDs must not repeat across restarts, even when everything else is
// deterministically seeded.
//
// Meshtastic suppresses duplicates by (sender, packet_id) for minutes, so a
// node that replays IDs after a restart is silently unreachable — which is
// exactly what the first two-radio bring-up hit: two links with fixed seeds
// found each other on the first run and never again.
func TestPacketIDsDoNotRepeatAcrossRestarts(t *testing.T) {
	ids := func() []uint32 {
		m := newFakeMesh(t)
		l := startLink(t, m, nodeKey(t, 1), 0xA1)
		var out []uint32
		for i := 0; i < 8; i++ {
			out = append(out, l.nextID())
		}
		return out
	}

	first, second := ids(), ids()
	same := 0
	for i := range first {
		if first[i] == second[i] {
			same++
		}
	}
	if same == len(first) {
		t.Fatal("two links with identical config produced identical packet IDs; " +
			"every peer would drop the second run's traffic as duplicates")
	}
}

// But an explicitly supplied source is still honoured, so a test that wants
// reproducible IDs can have them.
func TestPacketIDsCanBeInjected(t *testing.T) {
	m := newFakeMesh(t)
	a := startLink(t, m, nodeKey(t, 1), 0xA1, func(c *Config) { c.PacketIDs = rng.NewSeeded(99) })
	b := startLink(t, m, nodeKey(t, 2), 0xB2, func(c *Config) { c.PacketIDs = rng.NewSeeded(99) })
	if a.nextID() != b.nextID() {
		t.Error("an injected packet-ID source was not used")
	}
}

// §8.3: a ham-mode node may not run an encrypted channel, and the instance
// refuses to start rather than transmitting on one. The check needs the radio's
// answer to two questions, so it happens at connect.
func TestHamModeRefusesAnEncryptedChannel(t *testing.T) {
	m := newFakeMesh(t)
	ka := nodeKey(t, 1)

	l, err := New(Config{
		Key:      ka,
		Dial:     m.addLicensedRadio(0xA1, defaultChannels()),
		Channel:  "bbsnet",
		Governor: Unmetered{},
		Rand:     rng.NewSeeded(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = l.Start(context.Background())
	if err == nil {
		l.Close()
		t.Fatal("started in ham mode with an encrypted channel")
	}
	if !errors.Is(err, hammode.ErrEncryptedChannel) {
		t.Fatalf("err = %v, want ErrEncryptedChannel", err)
	}

	// With the override, it starts — and the banner says what was accepted.
	var events []string
	l2, err := New(Config{
		Key:            ka,
		Dial:           m.addLicensedRadio(0xA2, defaultChannels()),
		Channel:        "bbsnet",
		Governor:       Unmetered{},
		Rand:           rng.NewSeeded(2),
		Part97Override: true,
		OnEvent:        func(s string) { events = append(events, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l2.Start(context.Background()); err != nil {
		t.Fatalf("the override did not permit startup: %v", err)
	}
	defer l2.Close()

	if !l2.Part97().Licensed {
		t.Error("the link did not notice ham mode")
	}
	if l2.Part97().Restricted() {
		t.Error("still restricted despite the override")
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, "Part 97") {
		t.Errorf("no Part 97 banner was logged at startup:\n%s", joined)
	}
}
