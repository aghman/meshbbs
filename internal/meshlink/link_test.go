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
	m.setDrop(func(from uint32, payload []byte) bool {
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

	if a.MTU() != 233 {
		t.Errorf("MTU = %d, want 233 (§1)", a.MTU())
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
