package iplink

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
)

// MTU is the usable payload per datagram over IP.
//
// §3 puts it at "about 1400". Larger would work on most paths, but the sync
// protocol is written against the MESH's 233 bytes and gains nothing from a
// bigger frame here — the point of the Link abstraction is that one protocol
// runs over both, and a link that quietly encouraged 64 KB messages would let
// mesh-hostile habits creep in where nobody would notice until Phase 3.
const MTU = 1400

// maxFrame guards the reader. A peer is authenticated, not trusted: §11's peer
// table has a quarantine level precisely because an authenticated node can
// still misbehave.
const maxFrame = MTU

// Link is a link.Link over mutually-authenticated TLS 1.3.
//
// # Concurrency
//
// Unlike everything in the sync engine, this type uses goroutines and a mutex.
// That is not a violation of §12.1 — that constraint is about the SIMULATION
// being replayable from a seed, and internal/link says explicitly that
// concurrency belongs at the edges, in real transports. Determinism is bought
// by testing the protocol against the simulated mesh; this is the edge.
type Link struct {
	self      identity.NodeID
	clk       clock.Clock
	inbox     chan link.Datagram
	authorize Authorizer
	cert      tls.Certificate

	mu    sync.Mutex
	peers map[identity.NodeID]net.Conn
	// closed is guarded by mu and makes Close idempotent.
	closed bool

	listener net.Listener
	wg       sync.WaitGroup
}

// Config configures an IP link.
type Config struct {
	// Key is this node's identity. The certificate is derived from it.
	Key identity.NodeKey
	// Clock is injected, per §12.1.
	Clock clock.Clock
	// Authorize decides which authenticated peers may CONNECT IN.
	//
	// A nil Authorizer rejects every inbound connection, which is the right
	// default for a listener exposed to the internet: a link that accepts any
	// node dialling it is an open relay for the federation.
	//
	// It does not gate outbound dials. Naming a peer to Dial is itself the
	// authorization decision — the caller asked for that exact node and the
	// handshake proved it got it — so a node can always reach a peer it has
	// been told to contact without also maintaining an allow list.
	Authorize Authorizer
	// Inbox bounds buffered inbound datagrams.
	Inbox int
}

// New builds an IP link that is not yet listening or connected.
func New(cfg Config) (*Link, error) {
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
	}
	if cfg.Inbox <= 0 {
		cfg.Inbox = 256
	}
	cert, err := selfSignedCert(cfg.Key, cfg.Clock.Now())
	if err != nil {
		return nil, err
	}
	return &Link{
		self:      cfg.Key.ID(),
		clk:       cfg.Clock,
		inbox:     make(chan link.Datagram, cfg.Inbox),
		cert:      cert,
		authorize: cfg.Authorize,
		peers:     map[identity.NodeID]net.Conn{},
	}, nil
}

// Listen accepts inbound connections on addr until the link is closed.
func (l *Link) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		ln.Close()
		return link.ErrClosed
	}
	l.listener = ln
	l.mu.Unlock()

	l.wg.Add(1)
	go l.acceptLoop(ln)
	return nil
}

// Addr reports the listening address, useful when binding to port 0.
func (l *Link) Addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listener == nil {
		return nil
	}
	return l.listener.Addr()
}

func (l *Link) acceptLoop(ln net.Listener) {
	defer l.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		tc := tls.Server(conn, tlsConfig(l.cert, l.authorize, nil))
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.serve(tc)
		}()
	}
}

// serve completes the handshake and reads until the connection ends.
func (l *Link) serve(tc *tls.Conn) {
	if err := tc.HandshakeContext(context.Background()); err != nil {
		tc.Close()
		return
	}
	id, err := PeerIDFromCertificate(rawCerts(tc))
	if err != nil {
		tc.Close()
		return
	}
	l.register(id, tc)
	l.readLoop(id, tc)
}

func rawCerts(tc *tls.Conn) [][]byte {
	st := tc.ConnectionState()
	out := make([][]byte, 0, len(st.PeerCertificates))
	for _, c := range st.PeerCertificates {
		out = append(out, c.Raw)
	}
	return out
}

// Dial connects to a peer at addr, requiring it to authenticate as `expect`.
//
// The expected ID is not advisory. It is checked inside the TLS handshake, so a
// substituted or man-in-the-middle peer never exchanges application data with
// us at all — and since the ID is the hash of the key, there is no name
// resolution step for an attacker to poison.
func (l *Link) Dial(ctx context.Context, expect identity.NodeID, addr string) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return link.ErrClosed
	}
	if _, already := l.peers[expect]; already {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()

	d := &net.Dialer{}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	tc := tls.Client(raw, tlsConfig(l.cert, l.authorize, &expect))
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return fmt.Errorf("handshake with %s at %s: %w", expect.Compact(), addr, err)
	}

	l.register(expect, tc)
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		l.readLoop(expect, tc)
	}()
	return nil
}

func (l *Link) register(id identity.NodeID, conn net.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		conn.Close()
		return
	}
	if old, exists := l.peers[id]; exists && old != conn {
		old.Close()
	}
	l.peers[id] = conn
}

func (l *Link) drop(id identity.NodeID, conn net.Conn) {
	l.mu.Lock()
	if cur, ok := l.peers[id]; ok && cur == conn {
		delete(l.peers, id)
	}
	l.mu.Unlock()
	conn.Close()
}

// readLoop reads length-prefixed frames until the connection ends.
func (l *Link) readLoop(id identity.NodeID, conn net.Conn) {
	defer l.drop(id, conn)

	var hdr [4]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > maxFrame {
			// A frame outside the MTU is either a bug or a peer probing for a
			// large allocation. Either way the connection is not trustworthy.
			return
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}

		dg := link.Datagram{From: id, Data: buf, ReceivedAt: l.clk.Now()}
		select {
		case l.inbox <- dg:
		default:
			// A full inbox means the consumer is not draining. Dropping is
			// honest: silently growing the buffer would turn a backpressure bug
			// into an out-of-memory one, and this link is datagram-oriented, so
			// losing one is a case the sync protocol already handles.
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return
			}
		}
	}
}

func (l *Link) Name() string { return "ip" }
func (l *Link) MTU() int     { return MTU }

// Send transmits to one peer, or fans out to every connected peer for
// link.Broadcast.
//
// # Broadcast costs N here, and that inverts an argument
//
// On the mesh, one broadcast reaches everyone: that is precisely why §7.2
// chooses a fountain code over ARQ, since one transmission serves every peer at
// once. Over IP there is no such thing — a "broadcast" is N separate sends and
// costs N times as much.
//
// Caps().Broadcast is therefore false, which is not a statement that broadcast
// is unavailable but that it is not FREE. A governor reading that capability
// knows the fountain code's economics do not apply here, and that unicast
// repair to a single lagging peer is the cheaper move over IP even though it is
// the more expensive one over the mesh.
func (l *Link) Send(ctx context.Context, to identity.NodeID, payload []byte) error {
	if len(payload) > MTU {
		return link.ErrTooLarge
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return link.ErrClosed
	}
	var targets []net.Conn
	if to == link.Broadcast {
		for _, c := range l.peers {
			targets = append(targets, c)
		}
	} else if c, ok := l.peers[to]; ok {
		targets = append(targets, c)
	}
	l.mu.Unlock()

	if len(targets) == 0 {
		return fmt.Errorf("no connection to %s", to.Compact())
	}

	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)

	var firstErr error
	for _, c := range targets {
		if dl, ok := ctx.Deadline(); ok {
			_ = c.SetWriteDeadline(dl)
		}
		if _, err := c.Write(frame); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (l *Link) Recv() <-chan link.Datagram { return l.inbox }

// Budget reports effectively unlimited airtime.
//
// An IP link has no duty cycle and no shared band to be a good neighbour on, so
// the governor that dominates every decision on the mesh (§1.1, §7.6) simply
// does not apply. Reporting a large allowance rather than a special "unlimited"
// value keeps callers on one code path: the sync engine asks the same question
// of every link and gets an answer in the same units.
func (l *Link) Budget() link.Budget {
	return link.Budget{
		Available:   time.Hour,
		PerDatagram: 0,
	}
}

// Caps describes the IP transport.
func (l *Link) Caps() link.Caps {
	return link.Caps{
		// False because a broadcast costs N sends here, not one. See Send.
		Broadcast: false,
		// TCP retransmits, so L1's fountain coding is redundant over IP — a
		// caller reading this knows it can skip the repair symbols entirely.
		Reliable:    true,
		Ordered:     true,
		Addressable: true,
	}
}

// Peers lists currently connected nodes, sorted.
func (l *Link) Peers() []identity.NodeID {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]identity.NodeID, 0, len(l.peers))
	for id := range l.peers {
		out = append(out, id)
	}
	sortNodeIDs(out)
	return out
}

func sortNodeIDs(ids []identity.NodeID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && less(ids[j], ids[j-1]); j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

func less(a, b identity.NodeID) bool {
	for i := 0; i < identity.NodeIDLen; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Close shuts the link down and closes the Recv channel.
func (l *Link) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	ln := l.listener
	conns := make([]net.Conn, 0, len(l.peers))
	for _, c := range l.peers {
		conns = append(conns, c)
	}
	l.peers = map[identity.NodeID]net.Conn{}
	l.mu.Unlock()

	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	l.wg.Wait()
	close(l.inbox)
	return nil
}

// Compile-time proof that this satisfies the interface the sync engine uses.
// The whole design rests on one protocol running over mesh, IP and sneakernet
// alike (§3), and an assertion here is what keeps that honest.
var _ link.Link = (*Link)(nil)
