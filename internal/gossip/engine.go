package gossip

import (
	"fmt"
	"sort"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/vv"
)

// Store is the log the engine replicates.
//
// An interface rather than the SQLite store so the engine can be driven at
// simulation speed — thousands of nodes for thirty simulated days, in
// milliseconds. §12.1's constraints are what make that possible, and a hard
// dependency on the database would throw them away.
type Store interface {
	// Areas returns the federated areas this node participates in.
	Areas() []record.AreaTag
	// Vector returns the version vector for an area. Never nil.
	Vector(area record.AreaTag) *vv.Vector
	// Records returns the records in a range, in sequence order. It may return
	// fewer than asked for; the caller must not assume the range was filled.
	Records(area record.AreaTag, r vv.Range) []*record.Record
	// Apply stores records and returns how many were new. It must be
	// idempotent: duplicate delivery is routine on a flooding mesh, and
	// re-applying a record must be a no-op rather than an error.
	Apply(area record.AreaTag, recs []*record.Record) (int, error)
}

// Outbox is how the engine reaches the network.
//
// It separates policy from mechanism: the engine decides WHAT to send and WHEN,
// and the outbox handles framing, compression, fountain coding and the airtime
// governor. That split is what lets the engine be tested against a simulated
// mesh without a radio, and what will let it run unchanged over an IP link.
type Outbox interface {
	// SendMessage delivers a small control message. `to` may be
	// link.Broadcast.
	SendMessage(to identity.NodeID, payload []byte) error
	// SendRecords packs records into a bundle and BROADCASTS it, whoever asked.
	//
	// Broadcast rather than unicast on purpose: any other peer that is also
	// behind gets the records for free. It is the same broadcast economy that
	// makes fountain coding the right answer over ARQ (§7.2), applied one layer
	// up.
	SendRecords(area record.AreaTag, recs []*record.Record) error
	// Budget reports the current airtime allowance.
	Budget() link.Budget
}

// Config tunes the engine's airtime behaviour.
type Config struct {
	Schedule ScheduleConfig

	// RequestDelay is the maximum jittered wait before acting on a digest
	// mismatch.
	//
	// This exists because §7.3 does not address what happens when one broadcast
	// digest is heard by fifty peers who are all behind: every one of them
	// unicasts a request, at once, to the same node. That is the digest storm
	// again, wearing a different hat — one 100-byte digest triggering fifty
	// replies costs the channel far more than the digest did.
	//
	// So a peer waits a random fraction of this window before asking, and drops
	// its request if the answer arrives first. Since responses are broadcast,
	// the first peer to ask answers everyone, and the rest fall silent without
	// ever transmitting. Classic multicast repair suppression, and the reason
	// the reply storm never forms.
	RequestDelay time.Duration

	// MaxRangesPerRequest bounds one delta request. A peer further behind than
	// this asks again next cycle rather than in one enormous message.
	MaxRangesPerRequest int

	// MaxRecordsPerPush bounds one response bundle, for the same reason.
	MaxRecordsPerPush int

	// PeerTimeout is how long a peer stays "known" without being heard from.
	// It feeds the peer count that scales the digest interval, so a mesh that
	// shrinks speeds back up rather than staying slow forever.
	PeerTimeout time.Duration
}

// DefaultConfig returns sensible defaults for a LongFast mesh.
func DefaultConfig() Config {
	return Config{
		Schedule: DefaultSchedule(),
		// Roughly two full packet times, so peers spread across several
		// transmission slots rather than colliding in one.
		RequestDelay: 30 * time.Second,
		// Under the wire limit, so a request always fits one packet with room
		// to spare (see MaxRanges).
		MaxRangesPerRequest: MaxRanges - 2,
		MaxRecordsPerPush:   64,
		PeerTimeout:         24 * time.Hour,
	}
}

// peerState is what we know about one peer.
type peerState struct {
	id        identity.NodeID
	lastHeard time.Time
	// lastDigest is their most recent per-area state, used to decide whether
	// we have anything to learn from them.
	lastDigest map[record.AreaTag]AreaState
	// vectors holds full vectors they have sent us, pending reconciliation.
	vectors map[record.AreaTag]*vv.Vector
}

// pendingAsk is a reconciliation step waiting out its suppression window.
type pendingAsk struct {
	peer identity.NodeID
	area record.AreaTag
	at   time.Time
	// wantHash is the peer state that motivated the ask. If our own vector
	// reaches this hash before the timer fires — because someone else's
	// broadcast answered it — the ask is dropped without transmitting.
	wantHash [4]byte
}

// Engine runs the gossip cycle for one node.
//
// Single-threaded by contract: Tick and Receive must be called from one
// goroutine. That is not a limitation being apologised for, it is §12.1 — the
// sync engine is event-driven and single-threaded precisely so that a failing
// simulation replays exactly from its seed.
type Engine struct {
	self  identity.NodeID
	store Store
	out   Outbox
	clk   clock.Clock
	rnd   rng.Source
	cfg   Config
	sched *Scheduler

	peers   map[identity.NodeID]*peerState
	pending []pendingAsk

	// Counters for the sysop status screen and for asserting the airtime
	// invariant under simulation (§12.3).
	stats Stats
}

// Stats reports what the engine has done.
type Stats struct {
	DigestsSent       int
	DigestsHeard      int
	DigestsSuppressed int
	VectorReqsSent    int
	VectorReqsDropped int // suppressed because the answer arrived first
	VectorsSent       int
	RangeReqsSent     int
	RecordsPushed     int
	RecordsApplied    int
	BytesSent         int
}

// New builds an engine.
func New(self identity.NodeID, store Store, out Outbox, clk clock.Clock, rnd rng.Source, cfg Config) *Engine {
	e := &Engine{
		self:  self,
		store: store,
		out:   out,
		clk:   clk,
		rnd:   rnd,
		cfg:   cfg,
		peers: map[identity.NodeID]*peerState{},
	}
	e.sched = NewScheduler(cfg.Schedule, clk, rnd, e.PeerCount)
	return e
}

// PeerCount is the number of peers heard from within PeerTimeout.
//
// The digest interval scales with this, so a mesh that shrinks speeds back up
// instead of staying slow because it was once large.
func (e *Engine) PeerCount() int {
	if e.cfg.PeerTimeout <= 0 {
		return len(e.peers) + 1
	}
	cutoff := e.clk.Now().Add(-e.cfg.PeerTimeout)
	n := 0
	for _, p := range e.peers {
		if p.lastHeard.After(cutoff) {
			n++
		}
	}
	return n + 1 // ourselves
}

// Stats returns a snapshot of engine activity.
func (e *Engine) Stats() Stats {
	s := e.stats
	_, s.DigestsSuppressed, _ = e.sched.Stats()
	return s
}

// NextDue reports when this engine next intends to speak.
func (e *Engine) NextDue() time.Time { return e.sched.NextDue() }

// Digest builds this node's current digest.
func (e *Engine) Digest() *Digest {
	areas := map[record.AreaTag]*vv.Vector{}
	for _, a := range e.store.Areas() {
		areas[a] = e.store.Vector(a)
	}
	return NewDigest(areas)
}

// Tick advances the engine: fires due reconciliation steps and broadcasts the
// heartbeat digest when the scheduler says so.
//
// Called from the event loop rather than a timer goroutine, for the same
// determinism reason as everything else here.
func (e *Engine) Tick() error {
	if err := e.firePending(); err != nil {
		return err
	}

	due, _ := e.sched.Due()
	if !due {
		return nil
	}

	d := e.Digest()
	if len(d.Areas) == 0 {
		// Nothing federated; say nothing. A digest with no areas is pure cost.
		e.sched.MarkSent(0)
		return nil
	}

	// The governor has the last word. Anti-entropy is the safety net, and a
	// safety net that spends airtime the mesh does not have is worse than one
	// that waits (§7.6).
	if !e.out.Budget().CanSend() {
		return nil
	}

	payload := d.Encode()
	if err := e.out.SendMessage(link.Broadcast, payload); err != nil {
		return err
	}
	e.stats.DigestsSent++
	e.stats.BytesSent += len(payload)
	e.sched.MarkSent(len(payload))
	return nil
}

// NotePiggybacked records that a digest went out attached to other traffic.
//
// This is §7.3 mitigation 3, and it is why a busy node almost never sends a
// standalone digest: it has already told everyone what it holds.
func (e *Engine) NotePiggybacked(sizeBytes int) { e.sched.NoteTransmitted(sizeBytes) }

// firePending sends any reconciliation step whose suppression window has
// expired and which is still worth sending.
func (e *Engine) firePending() error {
	if len(e.pending) == 0 {
		return nil
	}
	now := e.clk.Now()
	keep := e.pending[:0]
	for _, ask := range e.pending {
		if now.Before(ask.at) {
			keep = append(keep, ask)
			continue
		}
		// The suppression check: if our own state now matches what the peer
		// advertised, someone else's broadcast already answered us and asking
		// would be pure cost.
		if e.store.Vector(ask.area).Hash() == ask.wantHash {
			e.stats.VectorReqsDropped++
			continue
		}
		if !e.out.Budget().CanSend() {
			// Out of budget: keep the ask rather than dropping it, so the work
			// is not silently lost when the governor relaxes.
			keep = append(keep, ask)
			continue
		}
		req := &VectorReq{Areas: []record.AreaTag{ask.area}}
		payload := req.Encode()
		if err := e.out.SendMessage(ask.peer, payload); err != nil {
			return err
		}
		e.stats.VectorReqsSent++
		e.stats.BytesSent += len(payload)
	}
	e.pending = keep
	return nil
}

// Receive handles an inbound gossip message.
func (e *Engine) Receive(from identity.NodeID, payload []byte) error {
	if from == e.self {
		return nil // our own broadcast, echoed back
	}
	t, err := PeekType(payload)
	if err != nil {
		return err
	}

	p := e.peers[from]
	if p == nil {
		p = &peerState{
			id:         from,
			lastDigest: map[record.AreaTag]AreaState{},
			vectors:    map[record.AreaTag]*vv.Vector{},
		}
		e.peers[from] = p
	}
	p.lastHeard = e.clk.Now()

	switch t {
	case MsgDigest:
		return e.onDigest(p, payload)
	case MsgVectorReq:
		return e.onVectorReq(p, payload)
	case MsgVector:
		return e.onVector(p, payload)
	case MsgRangeReq:
		return e.onRangeReq(p, payload)
	}
	return fmt.Errorf("unhandled gossip message type %s", t)
}

// onDigest is the heart of the cycle: compare, and decide whether to ask.
func (e *Engine) onDigest(p *peerState, payload []byte) error {
	d, err := DecodeDigest(payload)
	if err != nil {
		return err
	}
	e.stats.DigestsHeard++

	shared := 0
	matching := 0
	for _, a := range d.Areas {
		p.lastDigest[a.Tag] = a

		mine := e.store.Vector(a.Tag)
		if mine.Len() == 0 && !e.federates(a.Tag) {
			continue // not an area we carry
		}
		shared++

		if mine.Hash() == a.Hash {
			matching++
			continue
		}

		// Divergence proved. Who asks?
		//
		// The peer that is BEHIND asks, so that only one side transmits. Count
		// decides, and when counts tie or both saturate we ask anyway — a
		// wasted request costs one small packet, whereas both sides staying
		// quiet costs permanent divergence.
		if SaturatingCount(mine.Count()) > a.Count {
			continue // we are ahead; they will ask us
		}
		e.scheduleAsk(p.id, a.Tag, a.Hash)
	}

	// Suppression only when the peer agreed with us EVERYWHERE we overlap. A
	// peer that matches on one area and differs on another has not said what we
	// would have said (§7.3 mitigation 4).
	e.sched.NoteHeard(shared > 0 && matching == shared)
	return nil
}

// federates reports whether this node carries an area.
func (e *Engine) federates(tag record.AreaTag) bool {
	for _, a := range e.store.Areas() {
		if a == tag {
			return true
		}
	}
	return false
}

// scheduleAsk queues a vector request behind a jittered suppression window.
func (e *Engine) scheduleAsk(peer identity.NodeID, area record.AreaTag, wantHash [4]byte) {
	for _, ask := range e.pending {
		if ask.peer == peer && ask.area == area {
			return // already queued
		}
	}
	delay := time.Duration(0)
	if e.cfg.RequestDelay > 0 {
		delay = time.Duration(e.rnd.Float64() * float64(e.cfg.RequestDelay))
	}
	e.pending = append(e.pending, pendingAsk{
		peer: peer, area: area, at: e.clk.Now().Add(delay), wantHash: wantHash,
	})
}

// onVectorReq answers a peer's request for our full vector.
func (e *Engine) onVectorReq(p *peerState, payload []byte) error {
	req, err := DecodeVectorReq(payload)
	if err != nil {
		return err
	}
	for _, tag := range req.Areas {
		if !e.federates(tag) {
			continue
		}
		if !e.out.Budget().CanSend() {
			return nil
		}
		msg := &VectorMsg{Area: tag, Vector: e.store.Vector(tag)}
		out := msg.Encode()
		if err := e.out.SendMessage(p.id, out); err != nil {
			return err
		}
		e.stats.VectorsSent++
		e.stats.BytesSent += len(out)
	}
	return nil
}

// onVector compares a peer's full vector against ours and asks for the delta.
func (e *Engine) onVector(p *peerState, payload []byte) error {
	msg, err := DecodeVectorMsg(payload)
	if err != nil {
		return err
	}
	if !e.federates(msg.Area) {
		return nil
	}
	p.vectors[msg.Area] = msg.Vector

	missing := e.store.Vector(msg.Area).Missing(msg.Vector)
	if len(missing) == 0 {
		return nil // nothing to learn after all
	}

	// Ask for the lowest sequences first. Filling gaps from the bottom is what
	// advances the contiguous high-water mark; taking the newest records first
	// would leave the vector stuck where it was and make the next request
	// identical to this one — a peer that never catches up while transmitting
	// constantly.
	sort.Slice(missing, func(i, j int) bool { return missing[i].From < missing[j].From })
	if len(missing) > e.cfg.MaxRangesPerRequest {
		missing = missing[:e.cfg.MaxRangesPerRequest]
	}
	// Then bound by BYTES, because a range's encoded width grows with the
	// magnitude of its sequence numbers. A count-based limit fits one packet in
	// a young area and quietly overflows in a mature one.
	missing = FitRanges(msg.Area, missing, MaxControlMessage)
	if len(missing) == 0 {
		return nil
	}
	if !e.out.Budget().CanSend() {
		return nil
	}

	req := &RangeReq{Area: msg.Area, Ranges: missing}
	out := req.Encode()
	if err := e.out.SendMessage(p.id, out); err != nil {
		return err
	}
	e.stats.RangeReqsSent++
	e.stats.BytesSent += len(out)
	return nil
}

// onRangeReq answers a delta request by BROADCASTING the records.
func (e *Engine) onRangeReq(p *peerState, payload []byte) error {
	req, err := DecodeRangeReq(payload)
	if err != nil {
		return err
	}
	if !e.federates(req.Area) {
		return nil
	}

	var out []*record.Record
	for _, rg := range req.Ranges {
		if len(out) >= e.cfg.MaxRecordsPerPush {
			break
		}
		recs := e.store.Records(req.Area, rg)
		room := e.cfg.MaxRecordsPerPush - len(out)
		if len(recs) > room {
			recs = recs[:room]
		}
		out = append(out, recs...)
	}
	if len(out) == 0 {
		return nil
	}
	if !e.out.Budget().CanSend() {
		return nil
	}
	if err := e.out.SendRecords(req.Area, out); err != nil {
		return err
	}
	e.stats.RecordsPushed += len(out)
	// A bundle carries a piggybacked digest, so this counts as having announced
	// our state (§7.3 mitigation 3).
	e.NotePiggybacked(e.Digest().Size())
	return nil
}

// Publish stores locally authored records and broadcasts them.
//
// This is §7.3 cycle step 1, the opportunistic push, and it is how content
// actually propagates. Everything else in this package is the safety net that
// catches what the push missed.
//
// If the governor refuses, the records are still stored and simply not
// broadcast now. That is deliberate rather than a dropped write: the digest
// cycle will reveal that peers lack them and they will be pulled. Blocking a
// local post because the radio is busy would make the BBS unusable to defend an
// airtime budget that anti-entropy will satisfy anyway.
func (e *Engine) Publish(area record.AreaTag, recs []*record.Record) error {
	if len(recs) == 0 {
		return nil
	}
	if _, err := e.store.Apply(area, recs); err != nil {
		return err
	}
	if !e.out.Budget().CanSend() {
		return nil
	}
	if err := e.out.SendRecords(area, recs); err != nil {
		return err
	}
	e.stats.RecordsPushed += len(recs)
	// A bundle carries a piggybacked digest, so we have announced our state.
	e.NotePiggybacked(e.Digest().Size())
	return nil
}

// ApplyRecords stores inbound records and updates our view.
//
// Called by the transport when a bundle arrives, whether we asked for it or
// not — the broadcast economy means most useful records arrive unrequested.
func (e *Engine) ApplyRecords(area record.AreaTag, recs []*record.Record) (int, error) {
	n, err := e.store.Apply(area, recs)
	e.stats.RecordsApplied += n
	return n, err
}
