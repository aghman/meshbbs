package iplink

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
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

// newLink builds a listening link that accepts the given peers.
func newLink(t *testing.T, key identity.NodeKey, allowed ...identity.NodeID) *Link {
	t.Helper()
	l, err := New(Config{Key: key, Authorize: AllowList(allowed...)})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
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

// The baseline: two nodes authenticate each other by key alone and exchange
// datagrams. No CA, no name resolution, nothing to configure but the peer's ID.
func TestTwoNodesAuthenticateAndExchange(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := newLink(t, ka, kb.ID())
	b := newLink(t, kb, ka.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Dial(ctx, kb.ID(), b.Addr().String()); err != nil {
		t.Fatalf("dial: %v", err)
	}

	if err := a.Send(ctx, kb.ID(), []byte("hello from a")); err != nil {
		t.Fatal(err)
	}
	dg, ok := recvWithin(t, b, 5*time.Second)
	if !ok {
		t.Fatal("b received nothing")
	}
	if dg.From != ka.ID() {
		t.Errorf("datagram is from %s, want %s", dg.From.Compact(), ka.ID().Compact())
	}
	if string(dg.Data) != "hello from a" {
		t.Errorf("payload is %q", dg.Data)
	}

	// The reverse direction rides the same connection: b never dialled a.
	if err := b.Send(ctx, ka.ID(), []byte("hello from b")); err != nil {
		t.Fatal(err)
	}
	back, ok := recvWithin(t, a, 5*time.Second)
	if !ok {
		t.Fatal("a received nothing on the return path")
	}
	if string(back.Data) != "hello from b" || back.From != kb.ID() {
		t.Errorf("return datagram is %q from %s", back.Data, back.From.Compact())
	}
}

// The security property the whole design rests on: identity is the key.
//
// A peer is authenticated because it proved possession of a private key whose
// public half hashes to the node ID we expected. There is no name to spoof and
// no authority to compromise.
func TestPeerIdentityComesFromTheKey(t *testing.T) {
	k := nodeKey(t, 7)
	cert, err := selfSignedCert(k, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	got, err := PeerIDFromCertificate(cert.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	if got != k.ID() {
		t.Errorf("certificate authenticates %s, want %s", got.Compact(), k.ID().Compact())
	}

	if _, err := PeerIDFromCertificate(nil); !errors.Is(err, ErrNoPeerCertificate) {
		t.Errorf("an absent certificate gave %v", err)
	}
	if _, err := PeerIDFromCertificate([][]byte{{0x30, 0x00}}); err == nil {
		t.Error("a malformed certificate was accepted")
	}
}

// A node not on the allow list is refused, and refused during the handshake so
// it never sends application data.
func TestUnauthorizedPeerIsRefused(t *testing.T) {
	ka, kb, stranger := nodeKey(t, 1), nodeKey(t, 2), nodeKey(t, 99)

	// b allows only a.
	b := newLink(t, kb, ka.ID())
	s := newLink(t, stranger, kb.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Dial(ctx, kb.ID(), b.Addr().String())
	if err == nil {
		// The client handshake can complete before the server's rejection
		// arrives, so also confirm no data flows.
		if sendErr := s.Send(ctx, kb.ID(), []byte("let me in")); sendErr == nil {
			if _, got := recvWithin(t, b, time.Second); got {
				t.Fatal("an unauthorized node's datagram was delivered")
			}
		}
		t.Log("handshake returned nil, but no data was delivered")
		return
	}
	t.Logf("refused: %v", err)
	if _, got := recvWithin(t, b, 500*time.Millisecond); got {
		t.Error("an unauthorized node delivered a datagram anyway")
	}
}

// A nil Authorizer must reject everyone. A listener exposed to the internet
// that accepts any node which dials it is an open relay for the federation.
func TestNilAuthorizerRejectsEveryone(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)

	b, err := New(Config{Key: kb}) // no Authorize
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	a := newLink(t, ka, kb.ID())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_ = a.Dial(ctx, kb.ID(), b.Addr().String())
	_ = a.Send(ctx, kb.ID(), []byte("hello"))
	if _, got := recvWithin(t, b, time.Second); got {
		t.Error("a link with no Authorizer accepted a peer; the default must be closed")
	}
}

// The other half of the default-closed rule: a nil Authorizer must still allow
// OUTBOUND dials. Naming a peer to dial is itself the authorization decision,
// and requiring an allow list as well would mean a node could not reach a peer
// it had explicitly been told to contact.
func TestNilAuthorizerStillAllowsOutboundDials(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)

	// a has no allow list at all.
	a, err := New(Config{Key: ka})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	b := newLink(t, kb, ka.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Dial(ctx, kb.ID(), b.Addr().String()); err != nil {
		t.Fatalf("a link with no allow list could not dial out: %v", err)
	}
	if err := a.Send(ctx, kb.ID(), []byte("outbound")); err != nil {
		t.Fatal(err)
	}
	if _, ok := recvWithin(t, b, 5*time.Second); !ok {
		t.Error("the outbound datagram was not delivered")
	}
}

// Dialling checks the expected identity INSIDE the handshake, so a substituted
// peer never exchanges application data with us.
func TestDialRejectsTheWrongIdentity(t *testing.T) {
	ka, kb, kc := nodeKey(t, 1), nodeKey(t, 2), nodeKey(t, 3)

	// b is listening and would happily accept a...
	b := newLink(t, kb, ka.ID())
	a := newLink(t, ka, kb.ID(), kc.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// ...but a dials b's address expecting c.
	err := a.Dial(ctx, kc.ID(), b.Addr().String())
	if err == nil {
		t.Fatal("dialling expecting node C succeeded against node B")
	}
	t.Logf("refused: %v", err)
	if !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("error does not wrap ErrUnknownPeer: %v", err)
	}
	if len(a.Peers()) != 0 {
		t.Errorf("a registered a peer anyway: %v", a.Peers())
	}
}

// Broadcast fans out. Caps().Broadcast is false because it costs N sends here
// rather than one — that is the flag's meaning, not that it is unavailable.
func TestBroadcastFansOutAndIsNotFree(t *testing.T) {
	ka, kb, kc := nodeKey(t, 1), nodeKey(t, 2), nodeKey(t, 3)
	a := newLink(t, ka, kb.ID(), kc.ID())
	b := newLink(t, kb, ka.ID())
	c := newLink(t, kc, ka.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Dial(ctx, kb.ID(), b.Addr().String()); err != nil {
		t.Fatal(err)
	}
	if err := a.Dial(ctx, kc.ID(), c.Addr().String()); err != nil {
		t.Fatal(err)
	}

	if err := a.Send(ctx, link.Broadcast, []byte("to everyone")); err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]*Link{"b": b, "c": c} {
		dg, ok := recvWithin(t, l, 5*time.Second)
		if !ok {
			t.Errorf("%s received nothing from the broadcast", name)
			continue
		}
		if string(dg.Data) != "to everyone" {
			t.Errorf("%s got %q", name, dg.Data)
		}
	}

	// The capability must report the truth, because a governor reading it
	// decides whether fountain coding's economics apply (§7.2).
	if caps := a.Caps(); caps.Broadcast {
		t.Error("Caps reports free broadcast; over IP it costs one send per peer")
	} else if !caps.Reliable || !caps.Ordered || !caps.Addressable {
		t.Errorf("Caps understates TCP: %+v", caps)
	}
}

// The MTU is enforced in both directions: a caller cannot send an oversized
// datagram, and a peer cannot make us allocate one.
func TestMTUIsEnforcedBothWays(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := newLink(t, ka, kb.ID())
	b := newLink(t, kb, ka.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Dial(ctx, kb.ID(), b.Addr().String()); err != nil {
		t.Fatal(err)
	}

	if err := a.Send(ctx, kb.ID(), make([]byte, MTU+1)); err != link.ErrTooLarge {
		t.Errorf("an oversized payload gave %v, want ErrTooLarge", err)
	}
	// Exactly the MTU is fine.
	if err := a.Send(ctx, kb.ID(), make([]byte, MTU)); err != nil {
		t.Errorf("a full-MTU payload was refused: %v", err)
	}
	if _, ok := recvWithin(t, b, 5*time.Second); !ok {
		t.Error("a full-MTU datagram was not delivered")
	}

	// And a hostile frame header is refused rather than allocated. An
	// authenticated peer is not a trusted one — §11 has a quarantine level for
	// exactly this reason.
	t.Run("oversized frame header drops the connection", func(t *testing.T) {
		raw, err := net.Dial("tcp", b.Addr().String())
		if err != nil {
			t.Skip("cannot open a raw connection")
		}
		defer raw.Close()
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 1<<30) // a gigabyte
		_, _ = raw.Write(hdr[:])
		if _, ok := recvWithin(t, b, 500*time.Millisecond); ok {
			t.Error("a gigabyte frame header produced a datagram")
		}
	})
}

// Sending to a peer we have no connection to must fail loudly rather than
// silently succeeding into nothing.
func TestSendWithoutAConnectionFails(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := newLink(t, ka, kb.ID())

	err := a.Send(context.Background(), kb.ID(), []byte("nobody there"))
	if err == nil {
		t.Error("sending to an unconnected peer reported success")
	}
	if err := a.Send(context.Background(), link.Broadcast, []byte("nobody at all")); err == nil {
		t.Error("broadcasting with no peers reported success")
	}
}

// Close must be idempotent, must close Recv, and must not deadlock.
func TestCloseIsCleanAndIdempotent(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := newLink(t, ka, kb.ID())
	b := newLink(t, kb, ka.ID())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Dial(ctx, kb.ID(), b.Addr().String()); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.Close()
		a.Close() // idempotent
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked")
	}

	if _, open := <-a.Recv(); open {
		t.Error("Recv delivered a value after Close")
	}
	if err := a.Send(ctx, kb.ID(), []byte("after close")); err != link.ErrClosed {
		t.Errorf("sending after Close gave %v, want ErrClosed", err)
	}
}

// An IP link has no duty cycle, so the governor that dominates every mesh
// decision simply does not apply — but it must still answer in the same units,
// so callers keep one code path across transports.
func TestBudgetIsUnconstrained(t *testing.T) {
	a := newLink(t, nodeKey(t, 1))
	b := a.Budget()
	if !b.CanSend() {
		t.Error("an IP link reported it cannot send")
	}
	if b.Backpressure {
		t.Error("an IP link reported backpressure")
	}
	if a.Name() != "ip" || a.MTU() != MTU {
		t.Errorf("Name=%q MTU=%d", a.Name(), a.MTU())
	}
}

// Many datagrams in sequence, to shake out framing errors that a single
// round trip would not reveal.
func TestManyDatagramsSurviveFraming(t *testing.T) {
	ka, kb := nodeKey(t, 1), nodeKey(t, 2)
	a := newLink(t, ka, kb.ID())
	b, err := New(Config{Key: kb, Authorize: AllowList(ka.ID()), Inbox: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Dial(ctx, kb.ID(), b.Addr().String()); err != nil {
		t.Fatal(err)
	}

	const n = 500
	for i := 0; i < n; i++ {
		// Vary the length so a framing bug cannot hide behind a fixed size.
		payload := make([]byte, 1+i%1000)
		payload[0] = byte(i)
		if err := a.Send(ctx, kb.ID(), payload); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}

	got := 0
	for got < n {
		dg, ok := recvWithin(t, b, 5*time.Second)
		if !ok {
			break
		}
		wantLen := 1 + got%1000
		if len(dg.Data) != wantLen || dg.Data[0] != byte(got) {
			t.Fatalf("datagram %d: got %d bytes starting %d, want %d bytes starting %d",
				got, len(dg.Data), dg.Data[0], wantLen, byte(got))
		}
		got++
	}
	if got != n {
		t.Errorf("received %d of %d datagrams", got, n)
	}
}
