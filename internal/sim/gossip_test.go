package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bsmp"
	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/gossiptest"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

// This file runs a real federation — real engines, real records, real
// signatures, real fountain coding — across the simulated mesh.
//
// The transport here is a TEST transport, not the shipped one. Phase 3 owns the
// Meshtastic framing; what this needs to be is faithful in the ways that matter
// to the sync protocol: every byte crosses a 233-byte MTU, is charged airtime at
// the flood multiplier, and is independently lost per receiver.

// Frame types for the test transport.
const (
	frameControl byte = 1 // a gossip control message
	frameSymbol  byte = 2 // origLen(4 BE) | fountain symbol
)

// frameOverhead is what the transport costs on top of a fountain symbol.
const frameOverhead = 5

// node is one simulated BBS instance.
type node struct {
	key    identity.NodeKey
	id     identity.NodeID
	store  *gossiptest.Store
	engine *gossip.Engine
	link   *Link
	out    *bsmp.Outbox
	in     *bsmp.Inbox
}

type decKey struct {
	from     identity.NodeID
	bundleID uint32
}

// The transport under test is the SHIPPED one.
//
// internal/bsmp used to live here as a "test transport", which meant the
// simulator proved things about code that was never going to run on a radio.
// Now the fifty-node convergence runs, the airtime budget and the invariant
// suite all exercise the real packing, fountain coding and reassembly — and a
// regression in any of it fails here first, where the failure is reproducible
// from a seed.

// federation is a whole simulated mesh of BBS instances.
type federation struct {
	net   *Network
	nodes []*node
	area  record.AreaTag
	dicts *bundle.DictionarySet
}

func newFederation(t *testing.T, cfg Config, count int, dutyCycle float64) *federation {
	t.Helper()

	dict, err := bundle.NewRawDictionary(0, []byte("meshbbs post subject from re: wrote"))
	if err != nil {
		t.Fatal(err)
	}
	dicts, err := bundle.NewDictionarySet(dict)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dicts.Close)

	net := New(cfg)
	area := record.AreaTagFor("general")
	f := &federation{net: net, area: area, dicts: dicts}

	gcfg := gossip.DefaultConfig()

	for i := 0; i < count; i++ {
		// Deterministic keys: rng.TestSecret is seeded, so the whole run
		// replays. Production uses crypto/rand and never this.
		key, err := identity.GenerateNodeKey(rng.TestSecret(uint64(i) + 1))
		if err != nil {
			t.Fatal(err)
		}
		n := &node{
			key:   key,
			id:    key.ID(),
			store: gossiptest.NewStore(area),
		}
		n.link = net.NewLink(n.id, dutyCycle)
		out, err := bsmp.NewOutbox(bsmp.Config{
			Self: n.id, Link: n.link, Dictionary: dict, LossRate: cfg.LossRate,
		})
		if err != nil {
			t.Fatal(err)
		}
		n.out = out
		n.engine = gossip.New(n.id, n.store, n.out, net.Clock(),
			net.Rand(fmt.Sprintf("engine-%d", i)), gcfg)
		in, err := bsmp.NewInbox(bsmp.InboxConfig{
			Engine: n.engine, Dictionaries: dicts, Clock: net.Clock(),
		})
		if err != nil {
			t.Fatal(err)
		}
		n.in = in
		f.nodes = append(f.nodes, n)
	}

	// Every node trusts every other node's key.
	//
	// In production this comes from NODE records propagating through the same
	// anti-entropy machinery (§6.2), which is a Phase-2 task of its own. Seeding
	// it here keeps this test about convergence rather than about bootstrap.
	for _, a := range f.nodes {
		for _, b := range f.nodes {
			a.store.TrustKey(b.id, b.key.Public)
		}
	}

	// Drive each node from the event loop. No goroutines: a goroutine here
	// would reintroduce exactly the scheduling nondeterminism §12.1 forbids.
	for i := range f.nodes {
		n := f.nodes[i]
		net.Every(5*time.Second, func() {
			n.link.Pump(func(dg link.Datagram) {
				if err := n.in.Deliver(dg); err != nil {
					t.Errorf("node %s: %v", n.id.Short(), err)
				}
			})
			if err := n.engine.Tick(); err != nil {
				t.Errorf("node %s tick: %v", n.id.Short(), err)
			}
		})
	}
	return f
}

// publish has a node author and broadcast records.
func (f *federation) publish(t *testing.T, n *node, count int) []*record.Record {
	t.Helper()
	start := n.store.Vector(f.area).Get(n.id)
	var recs []*record.Record
	for i := 1; i <= count; i++ {
		rec, err := record.New(n.key, record.Record{
			Origin: n.id,
			Seq:    start + uint64(i),
			TS:     uint32(f.net.Now().Unix()),
			Type:   record.TypePost,
			Area:   f.area,
			Body:   []byte(fmt.Sprintf("post %d from %s", i, n.id.Short())),
		})
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	if err := n.engine.Publish(f.area, recs); err != nil {
		t.Fatal(err)
	}
	return recs
}

// agree reports whether every node holds an identical set of record IDs.
//
// Comparing the records themselves rather than the version vectors is
// deliberate: two vectors can agree while the underlying logs differ, so
// asserting on vectors would let real divergence pass.
func (f *federation) agree() bool {
	want := f.nodes[0].store.IDs(f.area)
	for _, n := range f.nodes[1:] {
		got := n.store.IDs(f.area)
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
	}
	return true
}

// convergedOn reports whether every node agrees AND holds the expected number
// of records.
//
// The count is not decoration. Agreement alone is trivially true on an empty
// network — every node holds nothing, so every node agrees — and a RunUntil on
// that predicate returns at t=0 having proved exactly nothing. This test file
// did that on its first run, and reported six nodes converged in zero seconds
// with zero records each.
func (f *federation) convergedOn(want int) func() bool {
	return func() bool {
		for _, n := range f.nodes {
			if n.store.Total() != want {
				return false
			}
		}
		return f.agree()
	}
}

func (f *federation) report(t *testing.T) {
	t.Helper()
	for _, n := range f.nodes {
		s := n.engine.Stats()
		t.Logf("  %s: %3d records, digests %d sent/%d suppressed/%d heard, "+
			"vecreq %d sent/%d dropped, ranges %d, symbols %d, airtime %s",
			n.id.Short(), n.store.Total(),
			s.DigestsSent, s.DigestsSuppressed, s.DigestsHeard,
			s.VectorReqsSent, s.VectorReqsDropped, s.RangeReqsSent,
			n.out.Stats().SymbolsSent, n.link.Spent().Round(time.Second))
	}
	st := f.net.Stats()
	t.Logf("  network: %d datagrams, %d bytes, %s of channel time, %d delivered, %d dropped",
		st.Datagrams, st.Bytes, st.Airtime.Round(time.Second), st.Delivered, st.Dropped)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// The baseline: content published on one node reaches every other node.
func TestFederationConvergesOnALossyMesh(t *testing.T) {
	cfg := DefaultConfig(1)
	cfg.LossRate = 0.15
	f := newFederation(t, cfg, 6, 0)

	f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 5) })

	ok := f.net.RunUntil(6*time.Hour, f.convergedOn(5))
	f.report(t)
	if !ok {
		t.Fatal("the federation did not converge within six simulated hours")
	}
	for _, n := range f.nodes {
		if got := n.store.Total(); got != 5 {
			t.Errorf("node %s holds %d records, want 5", n.id.Short(), got)
		}
	}
	t.Logf("converged after %s of simulated time", f.net.Now().Sub(cfg.Start).Round(time.Second))
}

// Every node publishing at once is the realistic case, and the one where a
// naive design produces a storm.
func TestFederationConvergesWithEveryNodePublishing(t *testing.T) {
	cfg := DefaultConfig(7)
	cfg.LossRate = 0.20
	f := newFederation(t, cfg, 8, 0)

	for i := range f.nodes {
		n := f.nodes[i]
		f.net.After(time.Duration(i+1)*90*time.Second, func() { f.publish(t, n, 3) })
	}

	ok := f.net.RunUntil(12*time.Hour, f.convergedOn(8*3))
	f.report(t)
	if !ok {
		t.Fatal("the federation did not converge within twelve simulated hours")
	}
	want := 8 * 3
	for _, n := range f.nodes {
		if got := n.store.Total(); got != want {
			t.Errorf("node %s holds %d records, want %d", n.id.Short(), got, want)
		}
	}
}

// §7.3's stated failure behaviour: "a node offline for a week comes back,
// broadcasts a digest showing stale rolling hashes, and peers backfill it".
//
// This is the property that makes anti-entropy the right choice over a
// FidoNet-style polling session — there is no session to have expired, and no
// state machine that can wedge.
func TestNodeOfflineForAWeekIsBackfilled(t *testing.T) {
	cfg := DefaultConfig(11)
	cfg.LossRate = 0.10
	f := newFederation(t, cfg, 5, 0)

	absent := f.nodes[4]
	f.net.SetUp(absent.id, false)

	// A week of activity it misses entirely.
	for day := 0; day < 7; day++ {
		d := day
		f.net.After(time.Duration(d)*24*time.Hour+time.Hour, func() {
			f.publish(t, f.nodes[d%4], 2)
		})
	}
	f.net.Run(7 * 24 * time.Hour)

	if got := absent.store.Total(); got != 0 {
		t.Fatalf("the offline node somehow holds %d records", got)
	}
	others := f.nodes[0].store.Total()
	t.Logf("after a week offline: peers hold %d records, the absent node holds 0", others)

	// It comes back.
	f.net.SetUp(absent.id, true)
	ok := f.net.RunUntil(24*time.Hour, f.convergedOn(others))
	f.report(t)
	if !ok {
		t.Fatalf("a node returning after a week was not backfilled within a simulated day "+
			"(holds %d of %d records)", absent.store.Total(), others)
	}
	t.Logf("backfilled to %d records", absent.store.Total())
}

// A mesh that splits and heals must converge, with no session to have died and
// no handshake to redo.
func TestPartitionedFederationHealsAndConverges(t *testing.T) {
	cfg := DefaultConfig(13)
	cfg.LossRate = 0.10
	f := newFederation(t, cfg, 6, 0)

	// Split three and three.
	for i, n := range f.nodes {
		f.net.Partition(n.id, i%2)
	}

	// Both halves keep working, unaware of each other.
	f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 4) })
	f.net.After(2*time.Minute, func() { f.publish(t, f.nodes[1], 4) })
	f.net.Run(8 * time.Hour)

	left, right := f.nodes[0].store.Total(), f.nodes[1].store.Total()
	t.Logf("while partitioned: one side holds %d records, the other %d", left, right)
	if f.agree() {
		t.Fatal("the two partitions converged while separated, which means the partition did not hold")
	}

	f.net.Heal()
	ok := f.net.RunUntil(24*time.Hour, f.convergedOn(8))
	f.report(t)
	if !ok {
		t.Fatalf("a healed partition did not converge within a simulated day "+
			"(sides hold %d and %d records)", f.nodes[0].store.Total(), f.nodes[1].store.Total())
	}
	for _, n := range f.nodes {
		if got := n.store.Total(); got != 8 {
			t.Errorf("node %s holds %d records after healing, want 8", n.id.Short(), got)
		}
	}
}

// Convergence must survive an airtime ceiling, because a real node has one.
// The governor refusing sends must delay convergence, never prevent it.
func TestConvergesUnderAnAirtimeCeiling(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.LossRate = 0.10
	// A 0.1% duty cycle: about 3.6 seconds of airtime per hour, which is well
	// under the cost of one bundle. The initial push cannot complete in one go,
	// so the governor refuses and anti-entropy has to finish the job.
	f := newFederation(t, cfg, 5, 0.001)

	f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 4) })

	ok := f.net.RunUntil(30*24*time.Hour, f.convergedOn(4))
	f.report(t)

	refused := 0
	for _, n := range f.nodes {
		refused += n.out.Stats().Refused
	}
	t.Logf("the governor refused %d transmissions", refused)
	if refused == 0 {
		t.Error("no send was ever refused, so this test did not exercise the governor")
	}
	if !ok {
		t.Fatal("a federation under an airtime ceiling failed to converge")
	}
}

// Anti-entropy is idempotent by construction, so running it for a long time on
// a converged mesh must not accumulate work or traffic. A protocol that chatters
// forever on a quiet network is one that cannot be deployed on a shared band.
func TestAConvergedMeshGoesNearlySilent(t *testing.T) {
	cfg := DefaultConfig(23)
	cfg.LossRate = 0.05
	f := newFederation(t, cfg, 6, 0)

	f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 4) })
	if !f.net.RunUntil(12*time.Hour, f.convergedOn(4)) {
		t.Fatal("failed to converge before measuring the quiet period")
	}

	before := f.net.Stats()
	quietStart := f.net.Now()
	f.net.Run(7 * 24 * time.Hour) // a silent week
	after := f.net.Stats()

	airtime := after.Airtime - before.Airtime
	elapsed := f.net.Now().Sub(quietStart)
	util := float64(airtime) / float64(elapsed)

	f.report(t)
	t.Logf("a converged 6-node mesh over a silent week: %d datagrams, %s of channel time, %.3f%% utilisation",
		after.Datagrams-before.Datagrams, airtime.Round(time.Second), util*100)

	// §7.3 budgets 1% of the channel for control traffic across the whole mesh.
	if util > 0.01 {
		t.Errorf("a converged idle mesh used %.3f%% of the channel, over the 1%% control budget", util*100)
	}
	// And it must still converge: something has to keep the heartbeat alive, or
	// a node that falls behind later is never noticed.
	if !f.agree() {
		t.Error("the mesh diverged during the quiet week")
	}
	if after.Datagrams == before.Datagrams {
		t.Error("the mesh went completely silent; a returning node would never be noticed")
	}
}

// Duplicate delivery is routine on a flooding mesh, so applying a record twice
// must be a no-op. If it were not, version vectors would advance on a
// re-delivery and the log would diverge from the vector describing it.
func TestDuplicateDeliveryIsIdempotent(t *testing.T) {
	cfg := DefaultConfig(29)
	cfg.LossRate = 0.05
	cfg.DuplicateRate = 0.60 // absurd, deliberately
	f := newFederation(t, cfg, 4, 0)

	f.net.After(time.Minute, func() { f.publish(t, f.nodes[0], 6) })
	ok := f.net.RunUntil(12*time.Hour, f.convergedOn(6))
	f.report(t)
	if !ok {
		t.Fatal("failed to converge with heavy duplication")
	}
	for _, n := range f.nodes {
		if got := n.store.Total(); got != 6 {
			t.Errorf("node %s holds %d records, want 6 — duplicates were counted as new",
				n.id.Short(), got)
		}
		if n.store.Rejected != 0 {
			t.Errorf("node %s rejected %d records; duplicates should be silently ignored, not refused",
				n.id.Short(), n.store.Rejected)
		}
	}
}

// Fifty instances is the scale [D2] commits to, and the number every airtime
// argument in §7.3 is computed against. Simulating it is the only way to know
// whether the four mitigations actually compose.
func TestFiftyNodeFederationConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("fifty nodes over simulated days")
	}
	cfg := DefaultConfig(31)
	cfg.LossRate = 0.15
	f := newFederation(t, cfg, 50, 0)

	// Ten nodes each publish two records, spread over the first few hours.
	const publishers, each = 10, 2
	for i := 0; i < publishers; i++ {
		n := f.nodes[i]
		f.net.After(time.Duration(i+1)*20*time.Minute, func() { f.publish(t, n, each) })
	}

	want := publishers * each
	ok := f.net.RunUntil(7*24*time.Hour, f.convergedOn(want))

	st := f.net.Stats()
	elapsed := f.net.Now().Sub(cfg.Start)
	util := float64(st.Airtime) / float64(elapsed)

	var digests, suppressed, vecreqs, dropped, symbols int
	for _, n := range f.nodes {
		s := n.engine.Stats()
		digests += s.DigestsSent
		suppressed += s.DigestsSuppressed
		vecreqs += s.VectorReqsSent
		dropped += s.VectorReqsDropped
		symbols += n.out.Stats().SymbolsSent
	}

	t.Logf("50 nodes, %d records, converged=%v after %s of simulated time", want, ok, elapsed.Round(time.Minute))
	t.Logf("  digests: %d sent, %d suppressed (%.0f%% of beats)", digests, suppressed,
		100*float64(suppressed)/float64(digests+suppressed))
	t.Logf("  requests: %d vector requests sent, %d dropped by suppression", vecreqs, dropped)
	t.Logf("  fountain symbols: %d", symbols)
	t.Logf("  channel: %s of airtime over %s = %.3f%% utilisation",
		st.Airtime.Round(time.Second), elapsed.Round(time.Hour), util*100)

	if !ok {
		holdings := map[int]int{}
		for _, n := range f.nodes {
			holdings[n.store.Total()]++
		}
		t.Fatalf("fifty nodes did not converge within a simulated week; holdings: %v", holdings)
	}

	// §1.1 gives federation a 5% share of the channel. The whole point of the
	// digest-storm analysis is that a naive design blows through it at this
	// scale; the mitigations have to bring it back under.
	if util > 0.05 {
		t.Errorf("fifty nodes used %.2f%% of the channel, over the 5%% federation budget", util*100)
	}
}

// §12.1's promise applies to the whole stack, not just the medium: a failing
// federation run must replay exactly from its seed. Engines, schedulers, jitter,
// fountain symbol selection and record signing all have to be deterministic for
// that to hold.
func TestFederationRunIsReproducible(t *testing.T) {
	run := func() string {
		cfg := DefaultConfig(99)
		cfg.LossRate = 0.20
		f := newFederation(t, cfg, 6, 0)
		for i := 0; i < 3; i++ {
			n := f.nodes[i]
			f.net.After(time.Duration(i+1)*10*time.Minute, func() { f.publish(t, n, 2) })
		}
		f.net.RunUntil(24*time.Hour, f.convergedOn(6))

		out := fmt.Sprintf("t=%s ", f.net.Now().Sub(cfg.Start))
		for _, n := range f.nodes {
			s := n.engine.Stats()
			out += fmt.Sprintf("|%s d%d/%d v%d/%d r%d s%d a%s",
				n.id.Short(), s.DigestsSent, s.DigestsSuppressed,
				s.VectorReqsSent, s.VectorReqsDropped, s.RangeReqsSent,
				n.out.Stats().SymbolsSent, n.link.Spent())
		}
		st := f.net.Stats()
		return out + fmt.Sprintf(" ||net %d/%d/%s", st.Datagrams, st.Delivered, st.Airtime)
	}

	first := run()
	for i := 0; i < 3; i++ {
		if again := run(); again != first {
			t.Fatalf("run %d diverged:\n first: %s\n again: %s", i, first, again)
		}
	}
	t.Logf("reproducible: %s", first)
}
