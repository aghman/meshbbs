// Package link is the federation transport abstraction (design §3).
//
// # Why this exists
//
// Meshtastic is treated as ONE link type: a datagram link with a 233-byte MTU,
// roughly 108 B/s, high loss and multi-minute latency. IP is another with a
// 1400-byte MTU and megabytes per second. Sneakernet is a third.
//
// That framing is load-bearing. If the sync protocol is designed against this
// interface from the start it works everywhere; if it is designed for IP and
// mesh is bolted on later it will never fit in 233 bytes. Design for the mesh,
// get IP for free — not the other way round.
//
// It is also what makes the deterministic simulator possible (§12.1): a fake
// Link with controllable MTU, latency, loss and flood multiplier is the only
// way to iterate on a sync protocol without deploying radios.
package link

import (
	"context"
	"errors"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
)

// Frame types occupy the first byte of every BSMP payload.
//
// # Why one registry, here
//
// The type byte is shared between layers: L1 writes it (a control message, a
// fountain symbol) and a mesh link reads it, both to price traffic by priority
// and to claim two values for its own use. One MTU is small enough that a
// second header byte — one per symbol, about fifteen per bundle — is not
// affordable purely to keep the layers from looking at each other's first byte
// (§12.7).
//
// It lives in this package rather than in either user because both a transport
// and the sync protocol above it need it, and neither should have to import the
// other to name a constant.
const (
	// FrameControl is a gossip control message: a digest, a delta request, a
	// version vector (§7.3).
	FrameControl byte = 1
	// FrameSymbol is a fountain symbol carrying part of a bundle (§7.2).
	FrameSymbol byte = 2
	// FrameAnnounce and FrameWhoIs are claimed by the mesh link, which uses
	// them to bind node IDs to radio addresses (§7.1.2). Other transports know
	// their own peers and never send these.
	FrameAnnounce byte = 3
	FrameWhoIs    byte = 4
)

// ErrClosed is returned by a Link that has been shut down.
var ErrClosed = errors.New("link closed")

// ErrTooLarge is returned when a datagram exceeds the link's MTU.
//
// It is deliberately an error rather than a silent fragmentation: fragmentation
// is L1's job (§7.2), and a link that quietly split payloads would hide the
// byte budget the whole design depends on.
var ErrTooLarge = errors.New("datagram exceeds link MTU")

// ErrNoBudget is returned when the airtime governor refuses a send.
var ErrNoBudget = errors.New("airtime budget exhausted")

// Broadcast is the peer ID meaning "every listener on this link".
//
// Broadcast is the normal case on a mesh, and the reason the reliability
// design is a fountain code rather than ARQ: one transmission serves every
// peer, and each decodes as soon as it has enough symbols (§7.2).
var Broadcast = identity.NodeID{}

// Datagram is one received payload.
type Datagram struct {
	// From is the sending node. Decoder state must be keyed on this as well as
	// the bundle ID: bundle IDs are random per sender, so two nodes can collide
	// on one and silently corrupt each other's decodes (§7.2).
	From identity.NodeID
	// Data is the payload, at most MTU bytes.
	Data []byte
	// ReceivedAt is when the link saw it, from the injected clock.
	ReceivedAt time.Time
}

// Caps describes what a transport can do.
type Caps struct {
	// Broadcast means one send reaches every peer. True for mesh, false for a
	// point-to-point IP link.
	Broadcast bool
	// Reliable means the transport itself retries. Meshtastic's want_ack is
	// limited enough that we treat the mesh as unreliable and let L1 handle it.
	Reliable bool
	// Ordered means datagrams arrive in the order sent.
	Ordered bool
	// Addressable means a specific peer can be targeted.
	Addressable bool
}

// Budget reports what a link will currently allow.
//
// This is expressed in airtime rather than bytes because airtime is superlinear
// in payload size at high spreading factors, and because what the commons pays
// is the local transmission multiplied by the flood multiplier (§1.1, §7.6).
// An IP link reports effectively unlimited budget; the mesh does not.
type Budget struct {
	// Available is how much airtime may be spent right now.
	Available time.Duration
	// PerDatagram is the estimated cost of sending one full-size datagram,
	// already multiplied by the flood multiplier where the link floods.
	PerDatagram time.Duration
	// Backpressure is true when the link wants callers to slow down.
	Backpressure bool
}

// CanSend reports whether the budget currently allows one datagram.
func (b Budget) CanSend() bool {
	return !b.Backpressure && b.Available >= b.PerDatagram
}

// Link is a federation transport.
//
// Implementations must be safe for concurrent use by one sender and one
// receiver, which is the shape the sync engine uses.
type Link interface {
	// Name identifies the transport in logs and the sysop status screen.
	Name() string

	// MTU is the usable payload bytes per datagram. 233 on Meshtastic; about
	// 1400 over IP.
	MTU() int

	// Send transmits to a peer, or to Broadcast. It returns ErrTooLarge if the
	// payload exceeds MTU, and ErrNoBudget if the governor refuses.
	Send(ctx context.Context, to identity.NodeID, payload []byte) error

	// Recv returns the channel of inbound datagrams. It is closed when the
	// link shuts down.
	Recv() <-chan Datagram

	// Budget reports the current send allowance.
	Budget() Budget

	// Caps describes the transport's properties.
	Caps() Caps

	// Close shuts the link down and closes the Recv channel.
	Close() error
}
