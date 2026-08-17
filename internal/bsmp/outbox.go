// Package bsmp carries the sync protocol over a link (design §7.2, §7.3).
//
// # What this is
//
// The seam between the engine and the wire. Above it, internal/gossip decides
// WHAT to send — which records a peer is missing, when to speak, whom to ask.
// Below it, a link.Link moves opaque datagrams. This package is everything in
// between: packing records into a bundle, compressing it against a trained
// dictionary, fountain-coding the result into symbols that fit one MTU, and
// reassembling all of that at the far end.
//
// It ran as a test transport inside the simulator for the whole of Phase 2,
// which is why the sync protocol could be debugged before any radio existed.
// This is that code made real, and the difference is not cosmetic: a test
// transport may assume a cooperative medium, and this one runs against a
// governor that refuses, a mesh that loses packets, and peers that arrive
// mid-transmission.
//
// # The two properties that make it work on a mesh
//
// Transmissions are RESUMABLE. `bundle_id` is derived from the bundle's content
// (§7.2), so when the governor interrupts a transmission partway, the retry is
// the same block: symbols the receivers already hold keep their value and the
// sender continues from a cursor. An earlier draft drew the ID at random, and
// the simulator measured what that costs — thirty simulated days, forty-three
// minutes of airtime, and zero records delivered.
//
// Reception is ORDER-INDEPENDENT. Any K+ε symbols decode a bundle, so a peer
// that missed the start of a transmission is not behind; it just needs more
// symbols, which the next broadcast supplies to everyone at once.
package bsmp

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/governor"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"lukechampine.com/blake3"
)

// frameOverhead is what this layer costs on top of a fountain symbol: the frame
// type byte and the original bundle length.
//
// The length is carried because a decoder must know how long the payload is
// before it can finish, and a receiver may hear symbol 7 before symbol 0. Four
// bytes per symbol is the price of not requiring the first symbol to arrive
// first, on a medium where it very often does not.
const frameOverhead = 1 + 4

// maxSymbolsPerBundle bounds what one bundle may ever cost across all resumed
// attempts, so a node nobody can hear stops paying for it forever.
const maxSymbolsPerBundle = 4 * fountain.MaxK

// classifier decides the priority of a bundle's traffic (§7.6).
//
// It is a function rather than a field on the bundle because the area tag is
// what the engine has, and mapping tags to priorities is a local policy: one
// sysop's mail area is another's announcement board.
type Classifier func(record.AreaTag) governor.Class

// Sender is the half of link.Link the outbox needs. Keeping it narrow is what
// lets the whole package be tested against a fake that loses packets on demand.
type Sender interface {
	Send(ctx context.Context, to identity.NodeID, payload []byte) error
	MTU() int
	Budget() link.Budget
}

// ChargeSender is an optional capability: a link that can be told what to bill
// a transmission to. The mesh link implements it; IP and the simulator do not
// need to, because neither shares a channel with anybody.
//
// It carries a governor.Charge rather than a bare class because §6.3's per-area
// share needs the area as well, and the two travel together — the class is
// DERIVED from the area, so splitting them across two arguments invites a
// caller to compute one from a tag and pass the other from somewhere else.
type ChargeSender interface {
	SendCharged(ctx context.Context, to identity.NodeID, payload []byte, ch governor.Charge) error
}

// Config configures an Outbox.
type Config struct {
	// Self is this node's ID. It seeds the fountain code's repair masks, so
	// both ends must agree on it — which is why the mesh link refuses to
	// deliver a datagram it cannot attribute (§7.1.2).
	Self identity.NodeID
	// Link is where datagrams go.
	Link Sender
	// Dictionary compresses bundles (§7.4).
	Dictionary *bundle.Dictionary
	// Classify maps an area to a priority class. Nil treats everything as
	// forum traffic, which is the safe middle.
	Classify Classifier
	// LossRate seeds the repair-symbol count. §7.2 makes this adaptive; until
	// the engine feeds back what is landing, it is a configured starting point.
	LossRate float64

	// AllowEncryptedDMs gates private mail leaving this node (§8.3).
	//
	// A function rather than a bool because ham mode is discovered when the
	// radio is read and can change on a reconnect — a sysop who enables it in
	// the Meshtastic app should not have to restart the BBS for the block to
	// take effect. Nil allows DMs, which is right for IP links and ISM bands.
	AllowEncryptedDMs func() bool
	// OnRefusedDM reports mail the policy stopped, so it reaches the sysop log
	// rather than vanishing.
	OnRefusedDM func(record.AreaTag)
}

// Outbox implements gossip.Outbox over a link.
type Outbox struct {
	cfg Config

	mu sync.Mutex
	// cursor remembers how far each bundle got, so an interrupted
	// transmission resumes instead of restarting.
	cursor map[uint32]int
	// lossRate is the CURRENT estimate, seeded from Config.LossRate and moved
	// by SetLossRate as the engine observes what is landing (§7.2 item 4).
	lossRate float64
	// dict is the CURRENT compression dictionary, seeded from Config.Dictionary
	// and moved by SetDictionary as peers say what they can read (§7.4). Same
	// shape as lossRate and for the same reason: it is a value the federation
	// loop learns rather than one the caller knew at construction.
	dict  *bundle.Dictionary
	stats Stats
}

// Stats are counters for the sysop status screen.
type Stats struct {
	MessagesSent int
	SymbolsSent  int
	BundlesSent  int
	Refused      int
	Abandoned    int
	// RefusedHamMode counts DM bundles the Part 97 block stopped (§8.3).
	RefusedHamMode int
}

// NewOutbox builds an outbox.
func NewOutbox(cfg Config) (*Outbox, error) {
	if cfg.Link == nil {
		return nil, fmt.Errorf("bsmp: no link configured")
	}
	if cfg.Dictionary == nil {
		return nil, fmt.Errorf("bsmp: no compression dictionary configured")
	}
	if cfg.Self.IsZero() {
		return nil, fmt.Errorf("bsmp: no node ID configured")
	}
	if cfg.LossRate < 0 || cfg.LossRate >= 1 {
		return nil, fmt.Errorf("bsmp: loss rate %v is not a fraction below 1", cfg.LossRate)
	}
	if cfg.Classify == nil {
		cfg.Classify = func(record.AreaTag) governor.Class { return governor.ClassForum }
	}
	return &Outbox{cfg: cfg, cursor: map[uint32]int{}, lossRate: cfg.LossRate, dict: cfg.Dictionary}, nil
}

// SendMessage delivers a small control message (§7.3).
func (o *Outbox) SendMessage(to identity.NodeID, payload []byte) error {
	frame := append([]byte{link.FrameControl}, payload...)
	if len(frame) > o.cfg.Link.MTU() {
		// Control messages fit one packet by construction
		// (gossip.MaxControlMessage). One that does not is a bug worth failing
		// loudly for rather than silently fragmenting.
		return fmt.Errorf("bsmp: control message of %d bytes exceeds the %d-byte MTU",
			len(frame), o.cfg.Link.MTU())
	}

	// Control frames belong to the protocol rather than to any one area, so
	// they carry the zero tag and the link bills them to no area's share.
	err := o.send(context.Background(), to, frame, governor.Charge{Class: governor.ClassControl})
	if err == link.ErrNoBudget {
		// Not an error to propagate: the engine's whole design is that a
		// missed beat is repaired by the next one (§7.3). Failing the caller
		// would turn a budget decision into an error path nobody can act on.
		o.count(func(s *Stats) { s.Refused++ })
		return nil
	}
	if err != nil {
		return err
	}
	o.count(func(s *Stats) { s.MessagesSent++ })
	return nil
}

// SendRecords packs records into a bundle and broadcasts it as fountain
// symbols.
//
// Broadcast whoever asked, per §7.3: any peer that is also behind gets the
// records for free. That is the same broadcast economy §7.2 uses to choose
// fountain coding over ARQ, applied one layer up.
func (o *Outbox) SendRecords(area record.AreaTag, recs []*record.Record) error {
	o.mu.Lock()
	dict := o.dict
	o.mu.Unlock()

	packed, err := bundle.Pack(&bundle.Bundle{Area: area, Records: recs, DictID: dict.ID()}, dict)
	if err != nil {
		return err
	}

	// Content-derived, so an interrupted transmission is resumable. See the
	// package comment for what a random ID cost when the simulator measured it.
	bundleID := bundleIDFor(packed)

	symSize := o.cfg.Link.MTU() - frameOverhead - fountain.HeaderSize
	if symSize <= 0 {
		return fmt.Errorf("bsmp: MTU %d is too small to carry a symbol", o.cfg.Link.MTU())
	}
	enc, err := fountain.NewEncoder(o.cfg.Self, bundleID, packed, symSize)
	if err != nil {
		return err
	}

	o.mu.Lock()
	start := o.cursor[bundleID]
	o.mu.Unlock()

	if start >= maxSymbolsPerBundle {
		// Nobody has decoded this after four times the maximum block size.
		// Continuing to pay for it would starve everything else.
		o.count(func(s *Stats) { s.Abandoned++ })
		return nil
	}

	o.mu.Lock()
	loss := o.lossRate
	o.mu.Unlock()

	total := enc.K() + fountain.RepairCount(enc.K(), loss)
	if start >= total {
		// A full transmission already went out and peers are still asking, so
		// send FURTHER repair symbols rather than repeating ones they have.
		total = start + fountain.RepairCount(enc.K(), loss)
	}
	if total > maxSymbolsPerBundle {
		total = maxSymbolsPerBundle
	}

	class := o.cfg.Classify(area)

	// §8.3: under an amateur licence, encrypted mail may not be transmitted.
	// Enforced here because this is the single point every DM crosses on its
	// way to the air — whether we wrote it or are relaying someone else's,
	// which Part 97 does not distinguish between.
	if class == governor.ClassDM && o.cfg.AllowEncryptedDMs != nil && !o.cfg.AllowEncryptedDMs() {
		o.count(func(s *Stats) { s.RefusedHamMode++ })
		if o.cfg.OnRefusedDM != nil {
			o.cfg.OnRefusedDM(area)
		}
		return nil
	}

	for i := start; i < total; i++ {
		s := enc.Symbol(uint16(i))
		frame := make([]byte, 0, frameOverhead+fountain.HeaderSize+symSize)
		frame = append(frame, link.FrameSymbol)
		frame = binary.BigEndian.AppendUint32(frame, uint32(enc.OrigLen()))
		frame = append(frame, s.Encode()...)

		err := o.send(context.Background(), link.Broadcast, frame,
			governor.Charge{Class: class, Area: area})
		if err == link.ErrNoBudget {
			// Stop here and remember where. The governor is not a failure; it
			// is the thing that makes this a good neighbour, and the cursor is
			// what makes obeying it free.
			o.mu.Lock()
			o.cursor[bundleID] = i
			o.mu.Unlock()
			o.count(func(s *Stats) { s.Refused++ })
			return nil
		}
		if err != nil {
			// A transport failure mid-transmission still advances the cursor:
			// the symbols that DID go out are not worthless, and repeating them
			// would spend airtime telling peers what they already know.
			o.mu.Lock()
			o.cursor[bundleID] = i
			o.mu.Unlock()
			return err
		}
		o.count(func(s *Stats) { s.SymbolsSent++ })
	}

	o.mu.Lock()
	o.cursor[bundleID] = total
	o.mu.Unlock()
	o.count(func(s *Stats) { s.BundlesSent++ })
	return nil
}

// send routes through SendCharged when the link understands priorities.
func (o *Outbox) send(ctx context.Context, to identity.NodeID, frame []byte, ch governor.Charge) error {
	if cs, ok := o.cfg.Link.(ChargeSender); ok {
		return cs.SendCharged(ctx, to, frame, ch)
	}
	return o.cfg.Link.Send(ctx, to, frame)
}

// Budget reports the link's current allowance.
func (o *Outbox) Budget() link.Budget { return o.cfg.Link.Budget() }

// Stats reports counters.
func (o *Outbox) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stats
}

// Forget drops the resume cursor for a bundle, for when the engine knows every
// peer has it.
func (o *Outbox) Forget(bundleID uint32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.cursor, bundleID)
}

func (o *Outbox) count(f func(*Stats)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f(&o.stats)
}

// bundleIDFor derives a bundle's ID from its packed bytes.
//
// One function rather than two call sites because the inbox uses this as an
// integrity check: it recomputes the ID over what the fountain decoder handed
// back and compares it with the ID the sender put in every symbol header. That
// only works while both ends agree on the derivation, and a divergence would
// not fail loudly — it would silently reject every bundle as corrupt. Sharing
// the function is what makes the agreement structural rather than a convention
// two files happen to follow.
func bundleIDFor(packed []byte) uint32 {
	sum := blake3.Sum256(packed)
	return binary.BigEndian.Uint32(sum[:4])
}

// SetLossRate updates the repair sizing from what the engine has observed.
//
// §7.2 item 4 makes this a governor decision rather than a protocol constant,
// and the reason is airtime: at K=15 on LongFast, assuming 50% loss on a link
// that is actually clean sends 50 symbols where 16 would do — six minutes of
// channel time instead of two, for every peer on the frequency, forever. A
// pessimistic constant is not a safe default here, it is a standing charge on
// the commons.
//
// Clamped rather than validated, because the caller is a controller reacting to
// noisy evidence and a transient overshoot should be absorbed, not returned as
// an error nobody can act on.
// SetDictionary changes which dictionary bundles are compressed with (§7.4).
//
// Called by the federation loop with gossip's negotiated floor: the highest
// dictionary every peer this node can hear has said it holds. It changes at
// runtime because peers upgrade, and because a peer that has fallen silent stops
// constraining us — neither of which should need a restart to take effect.
//
// # Why this is one value and not one per peer
//
// SendRecords broadcasts. Every listening node hears the same bytes, so there is
// exactly one dictionary to choose and one peer on an old build sets it for
// everybody. The alternative — encoding the same records once per peer — would
// give up the broadcast economy that §7.2 chose fountain coding to get, and
// spend the airtime of N transmissions to save the bytes of one.
//
// A nil dictionary is ignored rather than accepted: compressing with nothing is
// not a state Pack has, and a caller that resolved an ID to nothing has a bug
// worth leaving visible in its own code rather than turning into a panic here.
func (o *Outbox) SetDictionary(d *bundle.Dictionary) {
	if d == nil {
		return
	}
	o.mu.Lock()
	o.dict = d
	o.mu.Unlock()
}

// Dictionary reports what bundles are currently compressed with.
func (o *Outbox) Dictionary() *bundle.Dictionary {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dict
}

func (o *Outbox) SetLossRate(rate float64) {
	if rate < 0 {
		rate = 0
	}
	if rate > maxLossRate {
		rate = maxLossRate
	}
	o.mu.Lock()
	o.lossRate = rate
	o.mu.Unlock()
}

// LossRate reports the estimate currently sizing repair symbols.
func (o *Outbox) LossRate() float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lossRate
}

// maxLossRate caps the estimate. §7.2's own cost table stops at 50%, and past
// that the repair count climbs steeply for a link that is arguably not usable
// as a link at all — better to stop paying and let want-repair carry the
// stragglers than to triple every transmission chasing them.
const maxLossRate = 0.5
