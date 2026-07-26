package sim

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
)

func nodeID(b byte) identity.NodeID {
	var id identity.NodeID
	id[0] = b
	return id
}

// trace records everything observable about a run, so two runs can be compared
// byte for byte.
type trace struct {
	lines []string
}

func (tr *trace) add(format string, args ...any) {
	tr.lines = append(tr.lines, fmt.Sprintf(format, args...))
}

func (tr *trace) String() string { return strings.Join(tr.lines, "\n") }

// runScenario drives a fixed scenario and returns its full trace.
func runScenario(t *testing.T, seed uint64) string {
	t.Helper()
	cfg := DefaultConfig(seed)
	net := New(cfg)
	tr := &trace{}

	const n = 5
	links := make([]*Link, n)
	for i := 0; i < n; i++ {
		links[i] = net.NewLink(nodeID(byte(i+1)), 0)
	}

	// Every node broadcasts on a schedule, and drains its inbox on another.
	for i := 0; i < n; i++ {
		i := i
		net.Every(time.Duration(3+i)*time.Second, func() {
			msg := fmt.Sprintf("from-%d-at-%s", i, net.Now().Format("15:04:05.000"))
			err := links[i].Send(context.Background(), link.Broadcast, []byte(msg))
			tr.add("%s send node=%d err=%v", net.Now().Format("15:04:05.000"), i, err)
		})
		net.Every(900*time.Millisecond, func() {
			links[i].Pump(func(d link.Datagram) {
				tr.add("%s recv node=%d from=%s payload=%q",
					net.Now().Format("15:04:05.000"), i, d.From.Short(), d.Data)
			})
		})
	}

	// A node goes down and comes back, because a real mesh does that and the
	// recovery path is where sync bugs live.
	net.After(20*time.Second, func() {
		net.SetUp(nodeID(3), false)
		tr.add("%s node 3 down", net.Now().Format("15:04:05.000"))
	})
	net.After(40*time.Second, func() {
		net.SetUp(nodeID(3), true)
		tr.add("%s node 3 up", net.Now().Format("15:04:05.000"))
	})

	// And the mesh splits, then heals.
	net.After(50*time.Second, func() {
		net.Partition(nodeID(1), 1)
		net.Partition(nodeID(2), 1)
		tr.add("%s partitioned", net.Now().Format("15:04:05.000"))
	})
	net.After(70*time.Second, func() {
		net.Heal()
		tr.add("%s healed", net.Now().Format("15:04:05.000"))
	})

	net.Run(90 * time.Second)

	s := net.Stats()
	tr.add("stats datagrams=%d bytes=%d airtime=%s delivered=%d dropped=%d",
		s.Datagrams, s.Bytes, s.Airtime, s.Delivered, s.Dropped)
	return tr.String()
}

// §12.1's central promise: a failure prints its seed, and that seed reproduces
// the failure exactly. Everything else in the harness is worthless without it.
func TestSameSeedReproducesTheRunExactly(t *testing.T) {
	first := runScenario(t, 12345)

	// Guard against the test going vacuous. If the scenario ever degenerates to
	// producing almost nothing — a Send that always errors, a Pump that never
	// fires — the comparison below would still pass while proving nothing.
	if n := strings.Count(first, "\n") + 1; n < 200 {
		t.Fatalf("the scenario produced only %d trace lines; it is no longer exercising the network", n)
	}

	for i := 0; i < 5; i++ {
		again := runScenario(t, 12345)
		if first != again {
			// Show the first divergence rather than dumping both traces.
			a := strings.Split(first, "\n")
			b := strings.Split(again, "\n")
			for j := 0; j < len(a) && j < len(b); j++ {
				if a[j] != b[j] {
					t.Fatalf("run %d diverged at line %d:\n  first: %s\n  again: %s", i, j, a[j], b[j])
				}
			}
			t.Fatalf("run %d diverged in length: %d vs %d lines", i, len(a), len(b))
		}
	}
}

// A different seed must produce a different run, or the seed is being ignored
// and the test above proves nothing.
func TestDifferentSeedsDiverge(t *testing.T) {
	a := runScenario(t, 1)
	b := runScenario(t, 2)
	if a == b {
		t.Fatal("two different seeds produced identical runs; the seed is not being used")
	}
}

// The virtual clock is what makes long intervals testable. §7.3 scales the
// digest interval with peer count into hours; against a real clock that is
// untestable, and here it costs nothing.
func TestLongIntervalsCostNothing(t *testing.T) {
	net := New(DefaultConfig(1))
	l := net.NewLink(nodeID(1), 0)
	_ = l

	fired := 0
	net.Every(3*time.Hour, func() { fired++ })

	wall := time.Now()
	net.Run(30 * 24 * time.Hour) // thirty days
	elapsed := time.Since(wall)

	if fired < 200 {
		t.Fatalf("a three-hour timer fired only %d times over thirty simulated days", fired)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("thirty simulated days took %s of real time", elapsed)
	}
	t.Logf("thirty simulated days, %d three-hour ticks, %s of real time", fired, elapsed.Round(time.Millisecond))
}

// Ties at the same virtual instant must break by insertion order. Without that
// two events scheduled for one moment could run either way round and the run
// would not be reproducible.
func TestSimultaneousEventsRunInInsertionOrder(t *testing.T) {
	for attempt := 0; attempt < 20; attempt++ {
		net := New(DefaultConfig(7))
		var order []int
		at := net.Now().Add(time.Second)
		for i := 0; i < 10; i++ {
			i := i
			net.At(at, func() { order = append(order, i) })
		}
		net.Run(2 * time.Second)

		for i, got := range order {
			if got != i {
				t.Fatalf("attempt %d: events at one instant ran out of order: %v", attempt, order)
			}
		}
	}
}

// Independent per-receiver loss is the property that makes ARQ scale badly and
// fountain coding scale well (§7.2). A simulator that dropped a packet for
// everyone at once would hide that entirely.
func TestLossIsIndependentPerReceiver(t *testing.T) {
	cfg := DefaultConfig(99)
	cfg.LossRate = 0.5
	cfg.DuplicateRate = 0
	net := New(cfg)

	const receivers = 8
	sender := net.NewLink(nodeID(1), 0)
	got := make([]int, receivers)
	recv := make([]*Link, receivers)
	for i := 0; i < receivers; i++ {
		i := i
		recv[i] = net.NewLink(nodeID(byte(i+2)), 0)
		net.Every(500*time.Millisecond, func() {
			recv[i].Pump(func(link.Datagram) { got[i]++ })
		})
	}

	const sends = 60
	for i := 0; i < sends; i++ {
		net.After(time.Duration(i)*time.Second, func() {
			_ = sender.Send(context.Background(), link.Broadcast, []byte("hello"))
		})
	}
	net.Run(120 * time.Second)

	// Every receiver should have got a different subset. If they all got the
	// same count the loss is correlated, not independent.
	distinct := map[int]bool{}
	for _, c := range got {
		distinct[c] = true
	}
	t.Logf("per-receiver arrivals out of %d sends: %v", sends, got)
	if len(distinct) < 3 {
		t.Errorf("receivers saw near-identical arrival counts %v; loss is not independent", got)
	}
	for i, c := range got {
		if c == 0 || c == sends {
			t.Errorf("receiver %d got %d of %d — loss rate looks wrong", i, c, sends)
		}
	}
}

// §1.1: airtime is charged R times, because one origination costs the channel R
// transmissions once relays rebroadcast it. Budgeting the local transmission
// alone understates the cost to the commons by that factor.
func TestAirtimeIsChargedWithTheFloodMultiplier(t *testing.T) {
	cfg := DefaultConfig(3)
	cfg.FloodMultiplier = 4
	net := New(cfg)
	l := net.NewLink(nodeID(1), 0)
	net.NewLink(nodeID(2), 0)

	payload := make([]byte, 233)
	if err := l.Send(context.Background(), link.Broadcast, payload); err != nil {
		t.Fatal(err)
	}
	net.Run(10 * time.Second)

	single := time.Duration(233) * cfg.AirtimePerByte
	got := net.Stats().Airtime
	want := time.Duration(float64(single) * 4)

	if got != want {
		t.Fatalf("one 233-byte broadcast charged %s; with R=4 it should cost the channel %s",
			got, want)
	}
	t.Logf("one full packet: %s locally, %s charged to the channel at R=4", single, got)
}

// A link over its ceiling must refuse rather than transmit. This is the
// governor's contract, and the reason Budget is expressed in airtime.
func TestBudgetRefusesWhenExhausted(t *testing.T) {
	cfg := DefaultConfig(4)
	cfg.FloodMultiplier = 1
	net := New(cfg)

	perPacket := time.Duration(233) * cfg.AirtimePerByte
	l := net.NewLink(nodeID(1), perPacket*3) // room for exactly three
	net.NewLink(nodeID(2), 0)

	payload := make([]byte, 233)
	sent := 0
	for i := 0; i < 10; i++ {
		if err := l.Send(context.Background(), link.Broadcast, payload); err == nil {
			sent++
		} else if err != link.ErrNoBudget {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if sent != 3 {
		t.Fatalf("sent %d packets against a three-packet budget", sent)
	}
	if b := l.Budget(); b.CanSend() {
		t.Error("budget still reports it can send after being exhausted")
	}
}

func TestOversizedDatagramRefused(t *testing.T) {
	net := New(DefaultConfig(5))
	l := net.NewLink(nodeID(1), 0)
	if err := l.Send(context.Background(), link.Broadcast, make([]byte, 500)); err != link.ErrTooLarge {
		t.Fatalf("a 500-byte payload on a 233-byte link gave %v", err)
	}
}

// A node that is down neither sends nor receives, and traffic addressed to it
// is simply lost — which is what a flat battery looks like.
func TestDownNodesAreSilent(t *testing.T) {
	net := New(DefaultConfig(6))
	a := net.NewLink(nodeID(1), 0)
	b := net.NewLink(nodeID(2), 0)

	received := 0
	net.Every(time.Second, func() {
		b.Pump(func(link.Datagram) { received++ })
	})

	net.SetUp(nodeID(2), false)
	for i := 0; i < 5; i++ {
		net.After(time.Duration(i)*time.Second, func() {
			_ = a.Send(context.Background(), link.Broadcast, []byte("hello"))
		})
	}
	net.Run(20 * time.Second)
	if received != 0 {
		t.Fatalf("a node that is down received %d datagrams", received)
	}

	net.SetUp(nodeID(2), true)
	net.After(time.Second, func() {
		_ = a.Send(context.Background(), link.Broadcast, []byte("back"))
	})
	net.Run(20 * time.Second)
	if received == 0 {
		t.Fatal("a node that came back up received nothing")
	}
}

// Partitions split the mesh; healing reunites it. This is the scenario §7.3
// says anti-entropy must recover from without a session or a handshake.
func TestPartitionAndHeal(t *testing.T) {
	cfg := DefaultConfig(8)
	cfg.LossRate = 0
	cfg.DuplicateRate = 0
	net := New(cfg)

	a := net.NewLink(nodeID(1), 0)
	b := net.NewLink(nodeID(2), 0)

	got := 0
	net.Every(500*time.Millisecond, func() {
		b.Pump(func(link.Datagram) { got++ })
	})

	net.Partition(nodeID(1), 1) // a alone, b in group 0
	net.After(time.Second, func() {
		_ = a.Send(context.Background(), link.Broadcast, []byte("across the divide"))
	})
	net.Run(20 * time.Second)
	if got != 0 {
		t.Fatalf("a partitioned node received %d datagrams", got)
	}

	net.Heal()
	net.After(time.Second, func() {
		_ = a.Send(context.Background(), link.Broadcast, []byte("reunited"))
	})
	net.Run(20 * time.Second)
	if got == 0 {
		t.Fatal("healing the partition did not restore delivery")
	}
}

// Flooding delivers the same datagram by several paths, so duplicates are
// normal and dedup must be exact. The simulator has to produce them or that
// code path is never exercised.
func TestDuplicatesAreProduced(t *testing.T) {
	cfg := DefaultConfig(10)
	cfg.LossRate = 0
	cfg.DuplicateRate = 0.5
	net := New(cfg)

	a := net.NewLink(nodeID(1), 0)
	b := net.NewLink(nodeID(2), 0)

	got := 0
	net.Every(500*time.Millisecond, func() {
		b.Pump(func(link.Datagram) { got++ })
	})

	const sends = 40
	for i := 0; i < sends; i++ {
		net.After(time.Duration(i)*time.Second, func() {
			_ = a.Send(context.Background(), link.Broadcast, []byte("x"))
		})
	}
	net.Run(120 * time.Second)

	if got <= sends {
		t.Fatalf("with a 50%% duplicate rate, %d sends produced only %d arrivals", sends, got)
	}
	t.Logf("%d sends produced %d arrivals at a 50%% duplicate rate", sends, got)
}
