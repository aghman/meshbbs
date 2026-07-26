package sim

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/gossiptest"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"lukechampine.com/blake3"
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
	out    *outbox

	// decoders is fountain state keyed by (sender, bundleID). Keying on sender
	// as well as bundle ID is required, not optional: bundle IDs are random
	// per sender, so two nodes will eventually collide on one and silently
	// corrupt each other's decodes (§7.2).
	decoders map[decKey]*fountain.Decoder
	// origLens carries each block's unpadded length, which the symbol header
	// does not.
	origLens map[decKey]int
	// doneBundles suppresses re-applying a block we already decoded, since
	// symbols keep arriving after the decode completes.
	doneBundles map[decKey]bool
}

type decKey struct {
	from     identity.NodeID
	bundleID uint32
}

// outbox implements gossip.Outbox over a sim Link.
type outbox struct {
	link *Link
	rnd  rng.Source
	dict *bundle.Dictionary
	loss float64

	// SymbolsSent counts fountain symbols put on the air, so a test can price
	// reconciliation in packets rather than guessing.
	SymbolsSent  int
	MessagesSent int
	Refused      int

	// cursor tracks how many symbols have been sent for each bundle, so a
	// transmission the governor interrupted resumes instead of restarting.
	cursor map[uint32]int
}

func (o *outbox) SendMessage(to identity.NodeID, payload []byte) error {
	frame := append([]byte{frameControl}, payload...)
	if len(frame) > o.link.MTU() {
		// Control messages are supposed to fit one packet by construction
		// (gossip.MaxControlMessage). If one does not, that is a bug worth
		// failing loudly for rather than silently fragmenting.
		return fmt.Errorf("control message of %d bytes exceeds the %d-byte MTU", len(frame), o.link.MTU())
	}
	err := o.link.Send(context.Background(), to, frame)
	if err == link.ErrNoBudget {
		o.Refused++
		return nil
	}
	if err != nil {
		return err
	}
	o.MessagesSent++
	return nil
}

// maxSymbolsPerBundle bounds how many symbols one block may ever cost, across
// all resumed attempts, so a node that can never be heard stops paying forever.
const maxSymbolsPerBundle = 4 * fountain.MaxK

func (o *outbox) SendRecords(area record.AreaTag, recs []*record.Record) error {
	b := &bundle.Bundle{Area: area, Records: recs}
	packed, err := bundle.Pack(b, o.dict)
	if err != nil {
		return err
	}

	// The bundle ID is derived from the CONTENT, not drawn at random.
	//
	// §7.2 specifies "bundle_id ... a random uint32 chosen independently by
	// each node". That livelocks under an airtime governor, and the simulator
	// caught it: when the governor interrupts a fountain transmission partway,
	// a random ID means the retry is a different block, so every symbol the
	// receivers already hold becomes worthless. A node under a tight budget
	// then makes partial progress, discards it, and repeats — measured at 30
	// simulated days, 43 minutes of airtime, and zero records delivered.
	//
	// Content-derived IDs make an interrupted transmission RESUMABLE: the same
	// records produce the same block, so symbols accumulate across attempts and
	// a node converges over several budget windows instead of never.
	sum := blake3.Sum256(packed)
	bundleID := binary.BigEndian.Uint32(sum[:4])

	symSize := o.link.MTU() - frameOverhead - fountain.HeaderSize
	enc, err := fountain.NewEncoder(o.link.self, bundleID, packed, symSize)
	if err != nil {
		return err
	}

	// Resume where the last attempt stopped. Sending symbols 0..n again would
	// re-deliver what receivers already have; a fountain code's whole advantage
	// is that ANY further symbol helps, so continue past the cursor.
	start := o.cursor[bundleID]
	if start >= maxSymbolsPerBundle {
		return nil
	}

	// The repair count comes from the observed loss rate — the adaptive alpha
	// of §7.2. Here it is the configured rate; a real node estimates it from
	// whether its bundles are landing.
	total := enc.K() + fountain.RepairCount(enc.K(), o.loss)
	if start >= total {
		// Already sent a full transmission and peers are still asking, so send
		// further repair symbols rather than repeating the same ones.
		total = start + fountain.RepairCount(enc.K(), o.loss)
	}
	if total > maxSymbolsPerBundle {
		total = maxSymbolsPerBundle
	}

	for i := start; i < total; i++ {
		s := enc.Symbol(uint16(i))
		frame := make([]byte, 0, frameOverhead+fountain.HeaderSize+symSize)
		frame = append(frame, frameSymbol)
		frame = binary.BigEndian.AppendUint32(frame, uint32(enc.OrigLen()))
		frame = append(frame, s.Encode()...)

		err := o.link.Send(context.Background(), link.Broadcast, frame)
		if err == link.ErrNoBudget {
			o.Refused++
			o.cursor[bundleID] = i // resume from here when the budget recovers
			return nil
		}
		if err != nil {
			return err
		}
		o.SymbolsSent++
	}
	o.cursor[bundleID] = total
	return nil
}

func (o *outbox) Budget() link.Budget { return o.link.Budget() }

// receive handles one inbound frame.
func (n *node) receive(dg link.Datagram, dicts *bundle.DictionarySet) error {
	if len(dg.Data) == 0 {
		return nil
	}
	switch dg.Data[0] {
	case frameControl:
		return n.engine.Receive(dg.From, dg.Data[1:])

	case frameSymbol:
		if len(dg.Data) < 1+4+fountain.HeaderSize {
			return nil // truncated; the medium is allowed to do that
		}
		origLen := int(binary.BigEndian.Uint32(dg.Data[1:5]))
		s, err := fountain.DecodeSymbol(dg.Data[5:])
		if err != nil {
			return nil // a corrupted symbol is not a protocol violation
		}
		key := decKey{from: dg.From, bundleID: s.BundleID}
		if n.doneBundles[key] {
			return nil
		}
		dec := n.decoders[key]
		if dec == nil {
			if origLen <= 0 || origLen > 1<<20 {
				return nil
			}
			dec, err = fountain.NewDecoder(dg.From, s.BundleID, int(s.K), len(s.Data), origLen)
			if err != nil {
				return nil
			}
			n.decoders[key] = dec
			n.origLens[key] = origLen
		}
		if _, err := dec.Add(s); err != nil {
			return nil
		}
		if !dec.Done() {
			return nil
		}

		packed, err := dec.Payload()
		if err != nil {
			return nil
		}
		n.doneBundles[key] = true
		delete(n.decoders, key)

		b, err := bundle.Unpack(packed, dicts)
		if err != nil {
			return nil
		}
		_, err = n.engine.ApplyRecords(b.Area, b.Records)
		return err
	}
	return nil
}

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
			key:         key,
			id:          key.ID(),
			store:       gossiptest.NewStore(area),
			decoders:    map[decKey]*fountain.Decoder{},
			origLens:    map[decKey]int{},
			doneBundles: map[decKey]bool{},
		}
		n.link = net.NewLink(n.id, dutyCycle)
		n.out = &outbox{
			link:   n.link,
			rnd:    net.Rand(fmt.Sprintf("outbox-%d", i)),
			dict:   dict,
			loss:   cfg.LossRate,
			cursor: map[uint32]int{},
		}
		n.engine = gossip.New(n.id, n.store, n.out, net.Clock(),
			net.Rand(fmt.Sprintf("engine-%d", i)), gcfg)
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
				if err := n.receive(dg, dicts); err != nil {
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
			n.out.SymbolsSent, n.link.Spent().Round(time.Second))
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
		refused += n.out.Refused
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
		symbols += n.out.SymbolsSent
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
				n.out.SymbolsSent, n.link.Spent())
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
