// Package sim is the deterministic simulation harness (design §12.1).
//
// # Why this is the centrepiece
//
// You cannot debug a sync protocol by deploying radios. A single failed
// reconciliation takes minutes to observe, the trigger is a specific
// interleaving of loss and timing, and reproducing it means recreating RF
// conditions nobody controls. So the whole network runs in one process, on a
// virtual clock, with every source of nondeterminism driven by a single seed.
//
// A failure prints its seed; rerunning that seed reproduces it exactly. A
// thirty-day soak runs in milliseconds, and a three-hour digest interval
// becomes testable at all — which against a real clock it is not.
//
// # What makes it deterministic
//
// Four rules, all of which the rest of the codebase already follows because
// §12.1 made them Phase 0 constraints:
//
//  1. Time comes from a virtual clock, never the wall clock.
//  2. Randomness comes from one seeded source, consumed in a fixed order.
//  3. The event loop is SINGLE-THREADED. Goroutine scheduling is the one
//     nondeterminism Go will not let you seed, so the simulation never starts
//     one; concurrency belongs at the edges, in real transports.
//  4. Nothing iterates a map where the order is observable.
package sim

import (
	"container/heap"
	"fmt"
	"sort"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

// Config describes the simulated medium.
type Config struct {
	// Seed drives every random decision. The same seed reproduces the run.
	Seed uint64

	// MTU is the usable payload per datagram. 233 for Meshtastic.
	MTU int

	// LossRate is the independent per-receiver probability a datagram is lost.
	//
	// Independent per receiver is the important part: it is what makes ARQ
	// scale badly and fountain coding scale well (§7.2), and a simulator that
	// dropped a packet for everyone at once would hide that entirely.
	LossRate float64

	// MinLatency and MaxLatency bound delivery delay.
	MinLatency time.Duration
	MaxLatency time.Duration

	// DuplicateRate is the probability a delivered datagram arrives twice.
	// Flooding meshes do this routinely, and it is why dedup must be exact.
	DuplicateRate float64

	// ReorderRate is the probability a datagram's delivery time is perturbed
	// enough to overtake an earlier one.
	ReorderRate float64

	// FloodMultiplier is R from §1.1: how many transmissions the channel
	// actually pays for one origination. Airtime is charged R times.
	FloodMultiplier float64

	// AirtimePerByte is used to charge the airtime budget.
	AirtimePerByte time.Duration

	// Start is the virtual clock's initial time.
	Start time.Time
}

// DefaultConfig is a LongFast-like mesh: lossy, slow, and flooding.
func DefaultConfig(seed uint64) Config {
	return Config{
		Seed:            seed,
		MTU:             233,
		LossRate:        0.10,
		MinLatency:      500 * time.Millisecond,
		MaxLatency:      4 * time.Second,
		DuplicateRate:   0.05,
		ReorderRate:     0.10,
		FloodMultiplier: 4,
		// LongFast (SF11 / BW250) puts a full 233-byte packet at roughly two
		// seconds on air, so a byte costs about 2s/233 ≈ 8.6ms.
		//
		// This is a linear approximation of something that is not linear —
		// airtime has a fixed preamble cost and quantises to symbol boundaries —
		// but the number that matters here is the order of magnitude. At ~108 B/s
		// the budget is the binding constraint on the whole design (§1.1), and
		// modelling it as free is how you end up with a protocol that only works
		// over IP.
		AirtimePerByte: 8600 * time.Microsecond,
		Start:          time.Unix(1_700_000_000, 0).UTC(),
	}
}

// Handler receives datagrams delivered to a node.
//
// It is called synchronously from the event loop, so implementations must not
// block or start goroutines — doing either reintroduces the nondeterminism the
// harness exists to remove.
type Handler func(from identity.NodeID, payload []byte, at time.Time)

// Network is the simulated medium and its event loop.
type Network struct {
	cfg   Config
	clock *clock.Virtual
	rnd   rng.Source

	nodes   []identity.NodeID
	handler map[identity.NodeID]Handler
	up      map[identity.NodeID]bool
	// partition assigns each node a group; nodes in different groups cannot
	// hear each other. Group 0 is "everyone".
	partition map[identity.NodeID]int

	queue  eventQueue
	nextID uint64

	// Stats, for asserting the airtime invariant (§12.3).
	sentDatagrams int
	sentBytes     int
	airtime       time.Duration
	delivered     int
	dropped       int
}

// event is one scheduled delivery or timer.
type event struct {
	at  time.Time
	seq uint64 // insertion order, for deterministic tie-breaking
	run func()
}

// eventQueue is a binary heap ordered by (time, insertion order).
//
// A heap rather than a re-sorted slice because a thirty-day soak schedules
// millions of events, and re-sorting the whole queue per step would make the
// harness quadratic — slow enough that nobody runs the long simulations, which
// are the ones that find the bugs.
//
// Note that heap ordering is not stable, and does not need to be: `seq` makes
// Less a total order, so there are no ties left for stability to decide.
type eventQueue []event

func (q eventQueue) Len() int { return len(q) }
func (q eventQueue) Less(i, j int) bool {
	if !q[i].at.Equal(q[j].at) {
		return q[i].at.Before(q[j].at)
	}
	// Ties break by insertion order, never by map or slice accident. Without
	// this two events at the same virtual instant could run in either order and
	// the run would not be reproducible.
	return q[i].seq < q[j].seq
}
func (q eventQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *eventQueue) Push(x any) { *q = append(*q, x.(event)) }

func (q *eventQueue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	*q = old[:n-1]
	return e
}

// New builds a simulated network.
func New(cfg Config) *Network {
	if cfg.MTU == 0 {
		cfg.MTU = 233
	}
	if cfg.Start.IsZero() {
		cfg.Start = time.Unix(1_700_000_000, 0).UTC()
	}
	return &Network{
		cfg:       cfg,
		clock:     clock.NewVirtual(cfg.Start),
		rnd:       rng.NewSeeded(cfg.Seed),
		handler:   map[identity.NodeID]Handler{},
		up:        map[identity.NodeID]bool{},
		partition: map[identity.NodeID]int{},
	}
}

// Clock returns the virtual clock every node should use.
func (n *Network) Clock() clock.Clock { return n.clock }

// Now is the current virtual time.
func (n *Network) Now() time.Time { return n.clock.Now() }

// Rand returns a derived random source for a subsystem.
//
// Derived rather than shared so that adding a call in one place does not shift
// every other subsystem's sequence, which would make runs fragile to unrelated
// changes.
func (n *Network) Rand(label string) rng.Source { return n.rnd.Derive(label) }

// AddNode registers a node and its delivery handler.
func (n *Network) AddNode(id identity.NodeID, h Handler) {
	if _, exists := n.handler[id]; exists {
		panic(fmt.Sprintf("node %s added twice", id))
	}
	n.nodes = append(n.nodes, id)
	// Keep the node list sorted so any iteration over it is deterministic.
	sort.Slice(n.nodes, func(i, j int) bool {
		for b := 0; b < identity.NodeIDLen; b++ {
			if n.nodes[i][b] != n.nodes[j][b] {
				return n.nodes[i][b] < n.nodes[j][b]
			}
		}
		return false
	})
	n.handler[id] = h
	n.up[id] = true
	n.partition[id] = 0
}

// Nodes returns the registered node IDs, sorted.
func (n *Network) Nodes() []identity.NodeID {
	return append([]identity.NodeID(nil), n.nodes...)
}

// SetUp brings a node up or down. A node that is down neither sends nor
// receives, which models a crash or a flat battery.
func (n *Network) SetUp(id identity.NodeID, up bool) { n.up[id] = up }

// IsUp reports whether a node is running.
func (n *Network) IsUp(id identity.NodeID) bool { return n.up[id] }

// Partition assigns a node to a group. Nodes in different groups cannot hear
// each other, which models a mesh splitting in two.
func (n *Network) Partition(id identity.NodeID, group int) { n.partition[id] = group }

// Heal returns every node to one partition.
func (n *Network) Heal() {
	for _, id := range n.nodes {
		n.partition[id] = 0
	}
}

// At schedules a function to run at a virtual time.
func (n *Network) At(when time.Time, fn func()) {
	n.nextID++
	heap.Push(&n.queue, event{at: when, seq: n.nextID, run: fn})
}

// After schedules a function to run after a virtual delay.
func (n *Network) After(d time.Duration, fn func()) { n.At(n.clock.Now().Add(d), fn) }

// Every schedules a repeating function until the run ends.
//
// The interval is virtual, so a three-hour digest cycle costs nothing to
// simulate — which is the point, since §7.3 scales that interval with peer
// count and the resulting behaviour is exactly what needs testing.
func (n *Network) Every(interval time.Duration, fn func()) {
	var tick func()
	tick = func() {
		fn()
		n.After(interval, tick)
	}
	n.After(interval, tick)
}

// send queues delivery of a payload from one node.
//
// This is where the medium's character lives: independent per-receiver loss,
// duplication, reordering and latency. Every random draw happens here, in a
// fixed order over the sorted node list, so the sequence is reproducible.
func (n *Network) send(from identity.NodeID, to identity.NodeID, payload []byte) {
	if !n.up[from] {
		return
	}

	n.sentDatagrams++
	n.sentBytes += len(payload)
	// Airtime is charged R times: one origination costs the channel R
	// transmissions once relays rebroadcast it (§1.1).
	cost := time.Duration(float64(len(payload)) * float64(n.cfg.AirtimePerByte) * n.cfg.FloodMultiplier)
	n.airtime += cost

	broadcast := to == (identity.NodeID{})
	for _, peer := range n.nodes {
		if peer == from {
			continue
		}
		if !broadcast && peer != to {
			continue
		}
		if !n.up[peer] {
			n.dropped++
			continue
		}
		if n.partition[peer] != n.partition[from] {
			n.dropped++
			continue
		}

		// Independent loss per receiver.
		if n.rnd.Float64() < n.cfg.LossRate {
			n.dropped++
			continue
		}

		delay := n.latency()
		if n.rnd.Float64() < n.cfg.ReorderRate {
			// Perturb enough to overtake a neighbour, which is what reordering
			// looks like from the application's point of view.
			delay += time.Duration(n.rnd.IntN(2000)-1000) * time.Millisecond
			if delay < 0 {
				delay = 0
			}
		}

		// Copy per receiver. Handing every node the same backing array would let
		// one node's parser mutate what the others are about to see, and that
		// aliasing is not something a radio can do.
		copied := append([]byte(nil), payload...)
		target := peer
		n.After(delay, func() { n.deliver(from, target, copied) })

		if n.rnd.Float64() < n.cfg.DuplicateRate {
			dup := append([]byte(nil), payload...)
			n.After(delay+n.latency(), func() { n.deliver(from, target, dup) })
		}
	}
}

func (n *Network) latency() time.Duration {
	span := n.cfg.MaxLatency - n.cfg.MinLatency
	if span <= 0 {
		return n.cfg.MinLatency
	}
	return n.cfg.MinLatency + time.Duration(n.rnd.IntN(int(span/time.Millisecond)+1))*time.Millisecond
}

// deliver hands a datagram to a node, if it is still up when it arrives.
//
// The liveness check is here rather than at send time on purpose: a node that
// goes down while a packet is in flight never hears it, and that window is
// exactly where a sync engine forgets it still owes someone data.
func (n *Network) deliver(from, to identity.NodeID, payload []byte) {
	if !n.up[to] {
		n.dropped++
		return
	}
	h := n.handler[to]
	if h == nil {
		n.dropped++
		return
	}
	n.delivered++
	h(from, payload, n.clock.Now())
}

// Run advances the virtual clock, processing events until the queue empties or
// `limit` of virtual time has passed.
//
// Single-threaded and synchronous: every handler runs to completion before the
// next event, so there is no interleaving to be lucky or unlucky about.
func (n *Network) Run(limit time.Duration) {
	n.RunUntil(limit, func() bool { return false })
}

// step runs the next due event. It reports whether one ran.
func (n *Network) step(deadline time.Time) bool {
	if len(n.queue) == 0 || n.queue[0].at.After(deadline) {
		return false
	}
	next := heap.Pop(&n.queue).(event)
	if next.at.After(n.clock.Now()) {
		n.clock.Set(next.at)
	}
	next.run()
	return true
}

// RunUntil advances until cond holds or the limit passes. It reports whether
// cond became true.
//
// This is how convergence is asserted: run until every node agrees, or give up
// and report how far apart they were.
func (n *Network) RunUntil(limit time.Duration, cond func() bool) bool {
	deadline := n.clock.Now().Add(limit)
	if cond() {
		return true
	}
	for n.step(deadline) {
		if cond() {
			return true
		}
	}
	// Advance to the deadline even when the queue drained early, so that
	// consecutive Run calls compose: two Run(10s) calls span twenty seconds
	// whether or not anything happened in the first ten.
	if n.clock.Now().Before(deadline) {
		n.clock.Set(deadline)
	}
	return cond()
}

// Stats reports what the medium carried.
type Stats struct {
	Datagrams int
	Bytes     int
	Airtime   time.Duration
	Delivered int
	Dropped   int
	Pending   int
}

// Stats returns transmission statistics, for the airtime invariant (§12.3).
func (n *Network) Stats() Stats {
	return Stats{
		Datagrams: n.sentDatagrams,
		Bytes:     n.sentBytes,
		Airtime:   n.airtime,
		Delivered: n.delivered,
		Dropped:   n.dropped,
		Pending:   len(n.queue),
	}
}
