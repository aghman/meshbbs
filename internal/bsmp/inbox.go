package bsmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
)

// maxOrigLen bounds the payload a peer may claim a bundle decodes to.
//
// The length arrives in a symbol header from anyone holding the channel PSK, and
// it sizes an allocation. A megabyte is already far beyond anything the mesh can
// carry — at LongFast a 3 KB bundle is minutes of airtime — so this is not a
// tuning parameter, it is a bound on what a hostile symbol can make us allocate.
const maxOrigLen = 1 << 20

// maxOpenDecoders bounds how many partially-decoded bundles we hold.
//
// Each open decoder is memory a peer can cause us to spend by sending one
// symbol and never following up. On a mesh that is not even hostile behaviour —
// a node that runs out of budget mid-transmission leaves exactly this — so the
// eviction below is by age, not by suspicion.
const maxOpenDecoders = 64

// decoderTTL is how long a partial bundle waits for the rest of itself.
//
// §7.3 batches on a 15-30 minute cycle and a transmission interrupted by the
// governor can resume hours later, so this is generous. The cost of being wrong
// in either direction is asymmetric: expiring too early throws away symbols
// that were paid for in airtime.
const decoderTTL = 6 * time.Hour

// Applier is the half of the gossip engine the inbox needs.
type Applier interface {
	// Receive handles a control message from a peer.
	Receive(from identity.NodeID, payload []byte) error
	// ApplyRecords ingests a decoded bundle.
	ApplyRecords(area record.AreaTag, recs []*record.Record) (int, error)
}

// InboxConfig configures an Inbox.
type InboxConfig struct {
	Engine       Applier
	Dictionaries *bundle.DictionarySet
	Clock        clock.Clock
	// OnEvent reports decoded bundles and rejected frames.
	OnEvent func(string)
}

// Inbox reassembles datagrams into records and feeds them to the engine.
//
// # Why nothing here returns an error for bad input
//
// Every frame arrives from a radio, over a medium that truncates and corrupts,
// from anyone holding the channel PSK. A malformed symbol is not a protocol
// violation to be reported up the stack; it is Tuesday. The inbox counts what
// it rejects — a climbing counter is a real signal — and carries on. Errors are
// reserved for what the ENGINE says, because those mean our own state is wrong.
type Inbox struct {
	cfg InboxConfig
	clk clock.Clock

	mu       sync.Mutex
	decoders map[decKey]*openDecoder
	// done remembers bundles already applied, so the symbols that keep
	// arriving after a decode completes cost nothing.
	done  map[decKey]time.Time
	stats InboxStats
}

type decKey struct {
	from     identity.NodeID
	bundleID uint32
}

type openDecoder struct {
	dec     *fountain.Decoder
	started time.Time
	touched time.Time
}

// InboxStats are counters for the status screen.
type InboxStats struct {
	Control      int
	Symbols      int
	Decoded      int
	RecordsAdded int
	Rejected     int
	Duplicates   int
	Evicted      int
}

// NewInbox builds an inbox.
func NewInbox(cfg InboxConfig) (*Inbox, error) {
	if cfg.Engine == nil {
		return nil, errors.New("bsmp: no engine configured")
	}
	if cfg.Dictionaries == nil {
		return nil, errors.New("bsmp: no dictionary set configured")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewReal()
	}
	return &Inbox{
		cfg:      cfg,
		clk:      cfg.Clock,
		decoders: map[decKey]*openDecoder{},
		done:     map[decKey]time.Time{},
	}, nil
}

// Deliver handles one inbound datagram.
func (in *Inbox) Deliver(dg link.Datagram) error {
	if len(dg.Data) == 0 {
		return nil
	}
	switch dg.Data[0] {
	case link.FrameControl:
		in.bump(func(s *InboxStats) { s.Control++ })
		return in.cfg.Engine.Receive(dg.From, dg.Data[1:])
	case link.FrameSymbol:
		return in.symbol(dg)
	default:
		// Announce and who-is are the link's, and anything else is from a
		// version we do not speak. §7.1 says drop and log, with no
		// compatibility promises before the freeze.
		in.bump(func(s *InboxStats) { s.Rejected++ })
		return nil
	}
}

func (in *Inbox) symbol(dg link.Datagram) error {
	if len(dg.Data) < frameOverhead+fountain.HeaderSize {
		in.bump(func(s *InboxStats) { s.Rejected++ })
		return nil
	}
	origLen := int(binary.BigEndian.Uint32(dg.Data[1:5]))
	if origLen <= 0 || origLen > maxOrigLen {
		in.bump(func(s *InboxStats) { s.Rejected++ })
		return nil
	}
	sym, err := fountain.DecodeSymbol(dg.Data[frameOverhead:])
	if err != nil {
		in.bump(func(s *InboxStats) { s.Rejected++ })
		return nil
	}

	// Keyed by SENDER as well as bundle ID. Bundle IDs are content-derived and
	// two nodes can legitimately produce the same one for the same records; if
	// state were keyed on the ID alone their symbols would be mixed into one
	// decode and quietly corrupt each other (§7.2).
	key := decKey{from: dg.From, bundleID: sym.BundleID}
	now := in.clk.Now()

	in.mu.Lock()
	if _, ok := in.done[key]; ok {
		in.stats.Duplicates++
		in.mu.Unlock()
		return nil
	}
	in.expire(now)

	od := in.decoders[key]
	if od == nil {
		dec, err := fountain.NewDecoder(dg.From, sym.BundleID, int(sym.K), len(sym.Data), origLen)
		if err != nil {
			in.stats.Rejected++
			in.mu.Unlock()
			return nil
		}
		od = &openDecoder{dec: dec, started: now}
		in.decoders[key] = od
	}
	od.touched = now
	// Enforce the cap AFTER inserting, or the new decoder pushes the count one
	// over it. The freshly-touched entry is never the victim.
	in.evictToCap()

	if _, err := od.dec.Add(sym); err != nil {
		// A symbol that does not fit the block it claims to belong to — a
		// different K, a different symbol size. Not fatal to the decode already
		// in progress, so drop just this one.
		in.stats.Rejected++
		in.mu.Unlock()
		return nil
	}
	in.stats.Symbols++

	if !od.dec.Done() {
		in.mu.Unlock()
		return nil
	}
	packed, err := od.dec.Payload()
	delete(in.decoders, key)
	if err != nil {
		in.stats.Rejected++
		in.mu.Unlock()
		return nil
	}
	in.done[key] = now
	in.stats.Decoded++
	in.mu.Unlock()

	// Unpacking and applying happen OUTSIDE the lock: decompression and a
	// database write are slow, and holding the lock across them would stall
	// every other symbol arriving from every other peer.
	b, err := bundle.Unpack(packed, in.cfg.Dictionaries)
	if err != nil {
		in.bump(func(s *InboxStats) { s.Rejected++ })
		in.event(fmt.Sprintf("bundle from %s decoded but would not unpack: %v", dg.From.Short(), err))
		return nil
	}

	added, err := in.cfg.Engine.ApplyRecords(b.Area, b.Records)
	if err != nil {
		// This one IS returned: a failure here is our own storage or
		// verification, not a peer's malformed frame.
		return fmt.Errorf("applying %d records from %s: %w", len(b.Records), dg.From.Short(), err)
	}
	in.bump(func(s *InboxStats) { s.RecordsAdded += added })
	in.event(fmt.Sprintf("bundle from %s: %d records, %d new",
		dg.From.Short(), len(b.Records), added))
	return nil
}

// expire drops stale partial decodes and bounds how many are held. Caller holds
// the lock.
func (in *Inbox) expire(now time.Time) {
	for k, od := range in.decoders {
		if now.Sub(od.touched) > decoderTTL {
			delete(in.decoders, k)
			in.stats.Evicted++
		}
	}
	for k, at := range in.done {
		if now.Sub(at) > decoderTTL {
			delete(in.done, k)
		}
	}

}

// evictToCap drops the least recently touched decoders until the cap holds.
//
// Preferring the stalest is what makes this safe under load: a peer flooding
// new bundle IDs evicts its own half-finished work before it evicts a peer that
// is patiently mid-transfer. Caller holds the lock.
func (in *Inbox) evictToCap() {
	for len(in.decoders) > maxOpenDecoders {
		var oldest decKey
		var oldestAt time.Time
		first := true
		for k, od := range in.decoders {
			if first || od.touched.Before(oldestAt) {
				oldest, oldestAt, first = k, od.touched, false
			}
		}
		delete(in.decoders, oldest)
		in.stats.Evicted++
	}
}

// Stats reports counters.
func (in *Inbox) Stats() InboxStats {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.stats
}

// Pending reports how many bundles are partially decoded, which is the honest
// answer to "is anything happening?" on a link where one bundle takes hours.
func (in *Inbox) Pending() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.decoders)
}

func (in *Inbox) bump(f func(*InboxStats)) {
	in.mu.Lock()
	defer in.mu.Unlock()
	f(&in.stats)
}

func (in *Inbox) event(s string) {
	if in.cfg.OnEvent != nil {
		in.cfg.OnEvent(s)
	}
}
