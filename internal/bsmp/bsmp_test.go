package bsmp

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/governor"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

// fakeLink is a link that records what was sent and can refuse or lose.
type fakeLink struct {
	mtu  int
	sent []link.Datagram
	from identity.NodeID

	// refuseAfter makes the governor's answer arrive mid-transmission, which
	// is the normal case on a mesh rather than an edge case.
	refuseAfter int
	loseEvery   int
	classes     []governor.Class
	err         error
}

func (f *fakeLink) MTU() int { return f.mtu }

func (f *fakeLink) Send(ctx context.Context, to identity.NodeID, payload []byte) error {
	return f.SendClass(ctx, to, payload, governor.ClassForum)
}

func (f *fakeLink) SendClass(ctx context.Context, to identity.NodeID, payload []byte, class governor.Class) error {
	if f.err != nil {
		return f.err
	}
	if f.refuseAfter > 0 && len(f.sent) >= f.refuseAfter {
		return link.ErrNoBudget
	}
	f.classes = append(f.classes, class)
	// Copy: the outbox reuses its frame buffer's backing array.
	cp := append([]byte(nil), payload...)
	f.sent = append(f.sent, link.Datagram{From: f.from, Data: cp})
	return nil
}

func (f *fakeLink) Budget() link.Budget { return link.Budget{Available: time.Hour} }

// delivered returns the datagrams, dropping every nth to simulate loss.
func (f *fakeLink) delivered() []link.Datagram {
	if f.loseEvery <= 0 {
		return f.sent
	}
	var out []link.Datagram
	for i, d := range f.sent {
		if (i+1)%f.loseEvery == 0 {
			continue
		}
		out = append(out, d)
	}
	return out
}

// fakeEngine stands in for the gossip engine.
type fakeEngine struct {
	control [][]byte
	applied []*record.Record
	area    record.AreaTag
	err     error
}

func (e *fakeEngine) Receive(from identity.NodeID, payload []byte) error {
	e.control = append(e.control, append([]byte(nil), payload...))
	return nil
}

func (e *fakeEngine) ApplyRecords(area record.AreaTag, recs []*record.Record) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	e.area = area
	e.applied = append(e.applied, recs...)
	return len(recs), nil
}

func testDicts(t *testing.T) (*bundle.Dictionary, *bundle.DictionarySet) {
	t.Helper()
	d, err := bundle.NewRawDictionary(0, []byte("meshbbs post subject from re: wrote"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := bundle.NewDictionarySet(d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.Close)
	return d, set
}

func testRecords(t *testing.T, key identity.NodeKey, area record.AreaTag, n int) []*record.Record {
	t.Helper()
	var out []*record.Record
	base := time.Unix(1_800_000_000, 0).UTC()
	for i := 0; i < n; i++ {
		r, err := record.New(key, record.Record{
			Origin: key.ID(),
			Seq:    uint64(i + 1),
			TS:     uint32(base.Add(time.Duration(i) * time.Minute).Unix()),
			Type:   record.TypePost,
			Area:   area,
			Body: []byte("subject: a post about the mesh\n\n" +
				"Bodies compress well against the dictionary, which is the point of §7.4."),
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func newPair(t *testing.T, mut func(*fakeLink)) (*Outbox, *Inbox, *fakeLink, *fakeEngine, identity.NodeKey) {
	t.Helper()
	key, err := identity.GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	dict, dicts := testDicts(t)

	fl := &fakeLink{mtu: 233, from: key.ID()}
	if mut != nil {
		mut(fl)
	}
	out, err := NewOutbox(Config{Self: key.ID(), Link: fl, Dictionary: dict})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{}
	in, err := NewInbox(InboxConfig{Engine: eng, Dictionaries: dicts, Clock: clock.NewVirtual(time.Unix(1_800_000_000, 0))})
	if err != nil {
		t.Fatal(err)
	}
	return out, in, fl, eng, key
}

// The whole point of the package: records in one end, the same records out the
// other, having crossed a 233-byte MTU as fountain symbols.
func TestRecordsRoundTripThroughSymbols(t *testing.T) {
	out, in, fl, eng, key := newPair(t, nil)
	area := record.AreaTag{'t', 'e', 's', 't'}
	recs := testRecords(t, key, area, 6)

	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	if len(fl.sent) == 0 {
		t.Fatal("nothing was transmitted")
	}
	for _, d := range fl.sent {
		if len(d.Data) > fl.mtu {
			t.Fatalf("a frame of %d bytes exceeds the %d-byte MTU", len(d.Data), fl.mtu)
		}
		if d.Data[0] != link.FrameSymbol {
			t.Fatalf("frame type = %d, want a symbol", d.Data[0])
		}
	}

	for _, d := range fl.delivered() {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.applied) != len(recs) {
		t.Fatalf("applied %d records, sent %d", len(eng.applied), len(recs))
	}
	if eng.area != area {
		t.Errorf("area = %v, want %v", eng.area, area)
	}
	for i, got := range eng.applied {
		if got.Seq != recs[i].Seq || string(got.Body) != string(recs[i].Body) {
			t.Errorf("record %d did not survive the round trip", i)
		}
	}
}

// §7.2's reason for fountain coding: any K+ε symbols decode, so the receiver
// does not care WHICH ones it missed.
func TestDecodesDespiteLoss(t *testing.T) {
	out, in, fl, eng, key := newPair(t, func(f *fakeLink) { f.loseEvery = 4 })
	area := record.AreaTag{'l', 'o', 's', 's'}
	recs := testRecords(t, key, area, 8)

	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	delivered := fl.delivered()
	if len(delivered) >= len(fl.sent) {
		t.Fatal("the test lost nothing")
	}
	for _, d := range delivered {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.applied) != len(recs) {
		t.Fatalf("applied %d of %d records after losing a quarter of the symbols",
			len(eng.applied), len(recs))
	}
}

// The governor interrupting a transmission is the normal case, and §7.2 makes
// it survivable: the same records produce the same block, so symbols already
// delivered keep their value and the sender resumes from a cursor.
func TestInterruptedTransmissionResumes(t *testing.T) {
	out, in, fl, eng, key := newPair(t, func(f *fakeLink) { f.refuseAfter = 3 })
	area := record.AreaTag{'r', 'e', 's', 'u'}
	recs := testRecords(t, key, area, 8)

	// First attempt: the budget runs out after three symbols.
	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	if len(fl.sent) != 3 {
		t.Fatalf("sent %d symbols before the refusal, want 3", len(fl.sent))
	}
	first := append([]link.Datagram(nil), fl.sent...)

	// Budget recovers.
	fl.refuseAfter = 0
	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}

	// The resumed symbols must be NEW ones. Re-sending the first three would
	// tell receivers what they already know, which on a mesh is the difference
	// between converging and livelocking.
	resumed := fl.sent[len(first):]
	if len(resumed) == 0 {
		t.Fatal("nothing was sent on resume")
	}
	seen := map[string]bool{}
	for _, d := range first {
		seen[string(d.Data)] = true
	}
	for _, d := range resumed {
		if seen[string(d.Data)] {
			t.Error("a resumed transmission repeated a symbol the receiver already had")
		}
	}

	// And the whole thing still decodes.
	for _, d := range fl.sent {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.applied) != len(recs) {
		t.Fatalf("applied %d of %d records across two attempts", len(eng.applied), len(recs))
	}
}

// A refusal is not an error the engine can act on: §7.3's whole design is that
// a missed beat is repaired by the next one.
func TestBudgetRefusalIsNotAnError(t *testing.T) {
	out, _, fl, _, _ := newPair(t, func(f *fakeLink) { f.refuseAfter = 0 })
	fl.refuseAfter = 1 // refuse everything after the first

	if err := out.SendMessage(link.Broadcast, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("first message: %v", err)
	}
	if err := out.SendMessage(link.Broadcast, []byte{0x03}); err != nil {
		t.Fatalf("a refused control message returned an error: %v", err)
	}
	if out.Stats().Refused != 1 {
		t.Errorf("Refused = %d, want 1", out.Stats().Refused)
	}
}

// Control traffic must be priced as control, or §7.6's ladder is decorative.
func TestControlTrafficIsClassified(t *testing.T) {
	out, _, fl, _, key := newPair(t, nil)
	area := record.AreaTag{'m', 'a', 'i', 'l'}

	if err := out.SendMessage(link.Broadcast, []byte{0x09}); err != nil {
		t.Fatal(err)
	}
	if fl.classes[0] != governor.ClassControl {
		t.Errorf("control message went out as %v", fl.classes[0])
	}

	// And an area classifier decides the rest.
	dict, _ := testDicts(t)
	fl2 := &fakeLink{mtu: 233}
	out2, err := NewOutbox(Config{
		Self: key.ID(), Link: fl2, Dictionary: dict,
		Classify: func(a record.AreaTag) governor.Class {
			if a == (record.AreaTag{'m', 'a', 'i', 'l'}) {
				return governor.ClassDM
			}
			return governor.ClassForum
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := out2.SendRecords(area, testRecords(t, key, area, 2)); err != nil {
		t.Fatal(err)
	}
	for _, c := range fl2.classes {
		if c != governor.ClassDM {
			t.Errorf("mail went out as %v, want dm", c)
		}
	}
}

// Symbols that keep arriving after a bundle is decoded must cost nothing.
func TestSymbolsAfterDecodeAreCheap(t *testing.T) {
	out, in, fl, eng, key := newPair(t, nil)
	area := record.AreaTag{'d', 'u', 'p', 'e'}
	if err := out.SendRecords(area, testRecords(t, key, area, 4)); err != nil {
		t.Fatal(err)
	}
	for _, d := range fl.sent {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	applied := len(eng.applied)

	// Deliver every symbol a second time.
	for _, d := range fl.sent {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.applied) != applied {
		t.Errorf("records were applied twice: %d then %d", applied, len(eng.applied))
	}
	if in.Stats().Duplicates == 0 {
		t.Error("repeat symbols were not counted as duplicates")
	}
	if in.Pending() != 0 {
		t.Errorf("%d decoders left open after a completed bundle", in.Pending())
	}
}

// Everything here arrives from a radio, from anyone holding the channel PSK.
// Malformed input is counted, not returned.
func TestMalformedFramesAreCountedNotFatal(t *testing.T) {
	_, in, _, eng, _ := newPair(t, nil)

	frames := [][]byte{
		{},                          // empty
		{link.FrameSymbol},          // truncated
		{link.FrameSymbol, 0, 0, 0}, // no symbol header
		{0xFF, 1, 2, 3},             // a frame type we do not speak
		append([]byte{link.FrameSymbol, 0xFF, 0xFF, 0xFF, 0xFF}, make([]byte, 40)...), // absurd length
		append([]byte{link.FrameSymbol, 0, 0, 0, 10}, make([]byte, 40)...),            // garbage symbol
	}
	for i, f := range frames {
		if err := in.Deliver(link.Datagram{Data: f}); err != nil {
			t.Errorf("frame %d returned an error: %v", i, err)
		}
	}
	if len(eng.applied) != 0 {
		t.Error("malformed frames produced records")
	}
	if in.Stats().Rejected == 0 {
		t.Error("nothing was counted as rejected")
	}
}

// A length field from a stranger sizes an allocation, so it is bounded.
func TestAbsurdLengthIsRefusedBeforeAllocating(t *testing.T) {
	_, in, _, _, _ := newPair(t, nil)
	frame := append([]byte{link.FrameSymbol, 0x7F, 0xFF, 0xFF, 0xFF}, make([]byte, 60)...)
	if err := in.Deliver(link.Datagram{Data: frame}); err != nil {
		t.Fatal(err)
	}
	if in.Pending() != 0 {
		t.Error("a decoder was opened for a 2 GB bundle")
	}
}

// An engine failure is OUR problem and must surface; a peer's bad frame is not.
func TestEngineErrorsPropagate(t *testing.T) {
	out, in, fl, eng, key := newPair(t, nil)
	eng.err = errors.New("database is on fire")
	area := record.AreaTag{'e', 'r', 'r', 'x'}

	if err := out.SendRecords(area, testRecords(t, key, area, 3)); err != nil {
		t.Fatal(err)
	}
	var got error
	for _, d := range fl.sent {
		if err := in.Deliver(d); err != nil {
			got = err
		}
	}
	if got == nil {
		t.Fatal("an engine failure was swallowed")
	}
}

// A peer that opens bundles and never finishes them must not grow our memory
// without bound — and eviction must prefer the stalest, so a flood evicts its
// own half-finished work rather than a peer that is mid-transfer.
func TestOpenDecodersAreBounded(t *testing.T) {
	_, in, _, _, key := newPair(t, nil)
	dict, _ := testDicts(t)
	area := record.AreaTag{'f', 'l', 'o', 'd'}

	for i := 0; i < maxOpenDecoders*2; i++ {
		fl := &fakeLink{mtu: 233}
		out, err := NewOutbox(Config{Self: key.ID(), Link: fl, Dictionary: dict})
		if err != nil {
			t.Fatal(err)
		}
		// Distinct content per bundle means a distinct bundle ID.
		recs := testRecords(t, key, area, 3)
		distinct, err := record.New(key, record.Record{
			Origin: key.ID(), Seq: uint64(1000 + i), TS: 1_800_000_000,
			Type: record.TypePost, Area: area,
			Body: []byte{byte(i), byte(i >> 8), 'x'},
		})
		if err != nil {
			t.Fatal(err)
		}
		recs = append(recs, distinct)
		if err := out.SendRecords(area, recs); err != nil {
			t.Fatal(err)
		}
		// Only the first symbol: every bundle stays unfinished.
		if err := in.Deliver(fl.sent[0]); err != nil {
			t.Fatal(err)
		}
	}
	if in.Pending() > maxOpenDecoders {
		t.Errorf("%d open decoders, cap is %d", in.Pending(), maxOpenDecoders)
	}
	if in.Stats().Evicted == 0 {
		t.Error("nothing was evicted despite exceeding the cap")
	}
}

// Partial decodes expire, so airtime spent on a bundle nobody finished is
// eventually reclaimed rather than held forever.
func TestStalePartialsExpire(t *testing.T) {
	key, _ := identity.GenerateNodeKey(rng.TestSecret(3))
	dict, dicts := testDicts(t)
	clk := clock.NewVirtual(time.Unix(1_800_000_000, 0))
	fl := &fakeLink{mtu: 233}
	out, err := NewOutbox(Config{Self: key.ID(), Link: fl, Dictionary: dict})
	if err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{}
	in, err := NewInbox(InboxConfig{Engine: eng, Dictionaries: dicts, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}

	area := record.AreaTag{'s', 't', 'a', 'l'}
	if err := out.SendRecords(area, testRecords(t, key, area, 4)); err != nil {
		t.Fatal(err)
	}
	if err := in.Deliver(fl.sent[0]); err != nil {
		t.Fatal(err)
	}
	if in.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", in.Pending())
	}

	clk.Advance(decoderTTL + time.Hour)
	// Any later symbol triggers the sweep.
	if err := in.Deliver(fl.sent[1]); err != nil {
		t.Fatal(err)
	}
	if in.Stats().Evicted == 0 {
		t.Error("a six-hour-old partial decode was never expired")
	}
}

// §8.3: under an amateur licence, encrypted mail may not be transmitted — and
// this is the point every DM crosses on its way to the air, whether we wrote it
// or are relaying someone else's. Part 97 does not distinguish between those.
func TestHamModeBlocksDMBundles(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(9))
	if err != nil {
		t.Fatal(err)
	}
	dict, _ := testDicts(t)
	fl := &fakeLink{mtu: 233}

	mail := record.AreaTag{'m', 'a', 'i', 'l'}
	pub := record.AreaTag{'n', 'e', 'w', 's'}
	licensed := true
	var refused []record.AreaTag

	out, err := NewOutbox(Config{
		Self: key.ID(), Link: fl, Dictionary: dict,
		Classify: func(a record.AreaTag) governor.Class {
			if a == mail {
				return governor.ClassDM
			}
			return governor.ClassForum
		},
		AllowEncryptedDMs: func() bool { return !licensed },
		OnRefusedDM:       func(a record.AreaTag) { refused = append(refused, a) },
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := out.SendRecords(mail, testRecords(t, key, mail, 3)); err != nil {
		t.Fatal(err)
	}
	if len(fl.sent) != 0 {
		t.Fatalf("%d DM symbols went on the air under an amateur licence", len(fl.sent))
	}
	if len(refused) != 1 || refused[0] != mail {
		t.Errorf("the refusal did not reach the sysop log: %v", refused)
	}
	if out.Stats().RefusedHamMode != 1 {
		t.Errorf("RefusedHamMode = %d, want 1", out.Stats().RefusedHamMode)
	}

	// The important half: public traffic federates normally. A ham-mode
	// instance gives up private mail over the mesh and nothing else.
	if err := out.SendRecords(pub, testRecords(t, key, pub, 3)); err != nil {
		t.Fatal(err)
	}
	if len(fl.sent) == 0 {
		t.Fatal("public forum traffic was blocked in ham mode")
	}

	// And the block lifts without a restart when the sysop leaves ham mode,
	// which is why the gate is a function rather than a bool.
	licensed = false
	before := len(fl.sent)
	if err := out.SendRecords(mail, testRecords(t, key, mail, 3)); err != nil {
		t.Fatal(err)
	}
	if len(fl.sent) == before {
		t.Error("mail is still blocked after leaving ham mode")
	}
}

// Every frame the outbox emits must fit the MTU it was given, with nothing
// spilling over from the arithmetic that derives symbol size from it.
//
// This is the shape of the bug that stopped every bundle this project ever put
// on a real mesh. symSize is MTU minus the frame's own overhead, so a symbol
// frame lands EXACTLY on the MTU by construction — correct arithmetic, and
// correct is not the same as sendable. The link's MTU now reserves room the
// Meshtastic firmware needs for the Data wrapper and never asks for, and the
// property worth pinning here is the one that held all along: whatever the
// link says it can carry, the outbox must not exceed it.
//
// Sweeping sizes rather than testing one: an off-by-one in the derivation only
// shows up at particular MTUs, and the failure it caused was invisible — the
// link counted the frame sent and the receiver simply never saw it.
func TestEveryEmittedFrameFitsTheMTU(t *testing.T) {
	for _, mtu := range []int{64, 100, 128, 200, 217, 233} {
		t.Run(fmt.Sprintf("mtu=%d", mtu), func(t *testing.T) {
			out, _, fl, _, key := newPair(t, func(f *fakeLink) { f.mtu = mtu })
			area := record.AreaTag{'t', 'e', 's', 't'}

			if err := out.SendRecords(area, testRecords(t, key, area, 6)); err != nil {
				t.Fatal(err)
			}
			if len(fl.sent) == 0 {
				t.Fatal("nothing was sent")
			}
			for i, dg := range fl.sent {
				if len(dg.Data) > mtu {
					t.Errorf("frame %d is %d bytes, over the %d-byte MTU", i, len(dg.Data), mtu)
				}
			}
		})
	}
}

// A bundle that reassembles damaged must not become a permanent hole.
//
// `done` is marked before the payload is unpacked, and it is what makes a
// sender's later repair symbols cheap to ignore. If it survives a failed
// checksum, the very symbols that could repair the bundle are the ones
// suppressed, and a transmission that was merely damaged becomes unrecoverable
// — strictly worse than having no checksum, since without one the corruption
// at least reached a layer that could complain about it.
func TestADamagedBundleCanStillBeRepaired(t *testing.T) {
	out, in, fl, eng, key := newPair(t, nil)
	area := record.AreaTag{'d', 'm', 'g', 'd'}
	recs := testRecords(t, key, area, 6)

	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	sent := fl.delivered()
	if len(sent) < 2 {
		t.Fatalf("need several symbols to corrupt one, got %d", len(sent))
	}

	// Damage one symbol's payload. The fountain layer has no way to notice:
	// the header still describes a well-formed symbol of the right size, so it
	// solves the block and hands back confidently wrong bytes.
	damaged := make([]link.Datagram, len(sent))
	copy(damaged, sent)
	bad := append([]byte(nil), damaged[0].Data...)
	bad[len(bad)-1] ^= 0xFF
	damaged[0] = link.Datagram{From: damaged[0].From, Data: bad}

	for _, d := range damaged {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	if got := in.Stats().Corrupt; got == 0 {
		t.Fatal("a damaged bundle was not counted as corrupt")
	}
	if len(eng.applied) != 0 {
		t.Fatalf("%d records were applied from a damaged bundle", len(eng.applied))
	}

	// Now the sender repeats it, as it will when the peer keeps asking. This is
	// the delivery that must NOT be swallowed by the duplicate guard.
	for _, d := range sent {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.applied) != len(recs) {
		t.Fatalf("after a clean retransmission %d of %d records arrived — "+
			"the damaged attempt poisoned the bundle", len(eng.applied), len(recs))
	}
}

// The integrity check works only while the sender's bundle ID and the
// receiver's recomputation are the same function of the same bytes.
//
// A divergence would not fail loudly. Every bundle would decode, fail its ID
// comparison, be counted corrupt and discarded, and federation would go quiet
// with the counters blaming the link — which is a worse version of the bug the
// check was added to find. Pinning the relationship here means a change to the
// derivation breaks a test rather than the mesh.
func TestBundleIDIsTheIntegrityCheck(t *testing.T) {
	out, in, fl, eng, key := newPair(t, nil)
	area := record.AreaTag{'i', 'd', 'c', 'k'}
	recs := testRecords(t, key, area, 4)

	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	for _, d := range fl.delivered() {
		if err := in.Deliver(d); err != nil {
			t.Fatal(err)
		}
	}
	// Undamaged traffic must sail through: if the two ends had drifted apart,
	// this is where it would show, as a clean bundle called corrupt.
	if got := in.Stats().Corrupt; got != 0 {
		t.Errorf("%d undamaged bundles were called corrupt — "+
			"the sender's ID and the receiver's recomputation disagree", got)
	}
	if len(eng.applied) != len(recs) {
		t.Fatalf("applied %d of %d records", len(eng.applied), len(recs))
	}

	// And the ID a symbol carries really is the hash the receiver recomputes,
	// rather than two derivations that merely happen to agree today.
	sym, err := fountain.DecodeSymbol(fl.sent[0].Data[frameOverhead:])
	if err != nil {
		t.Fatal(err)
	}
	d, _ := testDicts(t)
	packed, err := bundle.Pack(&bundle.Bundle{Area: area, Records: recs}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := bundleIDFor(packed); got != sym.BundleID {
		t.Errorf("bundleIDFor = %#08x, symbol header carries %#08x", got, sym.BundleID)
	}
}
