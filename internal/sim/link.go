package sim

import (
	"context"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
)

// Link is a link.Link backed by the simulated network.
//
// The receive channel is buffered and filled SYNCHRONOUSLY from the event
// loop, and drained synchronously by Pump. That is deliberate: a real transport
// has a goroutine feeding that channel, but a goroutine here would reintroduce
// exactly the scheduling nondeterminism §12.1 forbids. Same interface, no
// concurrency.
type Link struct {
	net    *Network
	self   identity.NodeID
	inbox  chan link.Datagram
	closed bool

	// budget tracks what this node has spent, so the airtime invariant can be
	// asserted per node rather than only network-wide.
	spent time.Duration
	ceil  time.Duration
}

// NewLink attaches a node to the network and returns its Link.
//
// ceilPerRun bounds the airtime this node may spend; zero means unlimited,
// which is right for an IP link and wrong for a mesh.
func (n *Network) NewLink(self identity.NodeID, ceilPerRun time.Duration) *Link {
	l := &Link{
		net:   n,
		self:  self,
		inbox: make(chan link.Datagram, 4096),
		ceil:  ceilPerRun,
	}
	n.AddNode(self, func(from identity.NodeID, payload []byte, at time.Time) {
		if l.closed {
			return
		}
		select {
		case l.inbox <- link.Datagram{From: from, Data: payload, ReceivedAt: at}:
		default:
			// A full inbox means the node is not draining. Dropping is the
			// honest simulation of a receive queue overflowing, and silently
			// growing the buffer would hide a real backpressure bug.
			n.dropped++
		}
	})
	return l
}

func (l *Link) Name() string { return "sim" }
func (l *Link) MTU() int     { return l.net.cfg.MTU }

// Send transmits, charging airtime and refusing when over budget.
func (l *Link) Send(ctx context.Context, to identity.NodeID, payload []byte) error {
	if l.closed {
		return link.ErrClosed
	}
	if len(payload) > l.MTU() {
		return link.ErrTooLarge
	}
	cost := l.cost(len(payload))
	if l.ceil > 0 && l.spent+cost > l.ceil {
		return link.ErrNoBudget
	}
	l.spent += cost
	l.net.send(l.self, to, payload)
	return nil
}

func (l *Link) cost(n int) time.Duration {
	return time.Duration(float64(n) * float64(l.net.cfg.AirtimePerByte) * l.net.cfg.FloodMultiplier)
}

func (l *Link) Recv() <-chan link.Datagram { return l.inbox }

func (l *Link) Budget() link.Budget {
	avail := time.Duration(0)
	if l.ceil > 0 {
		avail = l.ceil - l.spent
		if avail < 0 {
			avail = 0
		}
	} else {
		avail = time.Hour
	}
	return link.Budget{
		Available:    avail,
		PerDatagram:  l.cost(l.MTU()),
		Backpressure: l.ceil > 0 && avail < l.cost(l.MTU()),
	}
}

func (l *Link) Caps() link.Caps {
	return link.Caps{Broadcast: true, Reliable: false, Ordered: false, Addressable: true}
}

func (l *Link) Close() error {
	if !l.closed {
		l.closed = true
		close(l.inbox)
	}
	return nil
}

// Spent reports the airtime this node has used.
func (l *Link) Spent() time.Duration { return l.spent }

// Pump drains the inbox, handing each datagram to fn.
//
// Called from the event loop rather than a goroutine, so delivery order is
// exactly the order the network queued.
func (l *Link) Pump(fn func(link.Datagram)) int {
	n := 0
	for {
		select {
		case d, ok := <-l.inbox:
			if !ok {
				return n
			}
			fn(d)
			n++
		default:
			return n
		}
	}
}
