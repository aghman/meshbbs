package bsmp

import (
	"errors"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/fountain"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

// twoDicts builds a set holding dictionaries 0 and 1, standing in for a node
// that has been upgraded and can still read what older peers send.
func twoDicts(t *testing.T) (*bundle.Dictionary, *bundle.Dictionary, *bundle.DictionarySet) {
	t.Helper()
	d0, err := bundle.NewRawDictionary(0, []byte("meshbbs post subject from re: wrote"))
	if err != nil {
		t.Fatal(err)
	}
	d1, err := bundle.NewRawDictionary(1, []byte("meshbbs post subject from re: wrote posted thread reply 73 de"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := bundle.NewDictionarySet(d0, d1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(set.Close)
	return d0, d1, set
}

// packedDictID reads the dictionary ID out of the first symbol the outbox sent.
//
// The bundle header travels UNCOMPRESSED precisely so a receiver can read the
// dictionary before spending anything on decompression, which is also what makes
// it readable here without decoding the whole transmission.
func packedDictID(t *testing.T, fl *fakeLink) uint8 {
	t.Helper()
	if len(fl.sent) == 0 {
		t.Fatal("nothing was sent")
	}
	sym, err := fountain.DecodeSymbol(fl.sent[0].Data[frameOverhead:])
	if err != nil {
		t.Fatalf("decode symbol: %v", err)
	}
	// Symbol 0 is systematic, so its payload is the head of the packed bundle:
	// format version, then the dictionary ID.
	if len(sym.Data) < 2 {
		t.Fatalf("first symbol carries %d bytes, too few to hold a bundle header", len(sym.Data))
	}
	return sym.Data[1]
}

// The negotiated floor has to reach the bytes, not just the log line.
//
// SetDictionary is called by the federation loop every tick with gossip's floor,
// and this is the assertion that it changes what actually goes on the air —
// without a restart, because peers upgrade and fall silent while a node runs.
func TestSetDictionaryChangesWhatIsPacked(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(20))
	if err != nil {
		t.Fatal(err)
	}
	d0, d1, _ := twoDicts(t)
	area := record.AreaTagFor("general")
	recs := testRecords(t, key, area, 3)

	fl := &fakeLink{mtu: 233, from: key.ID()}
	out, err := NewOutbox(Config{Self: key.ID(), Link: fl, Dictionary: d1})
	if err != nil {
		t.Fatal(err)
	}

	// Starts at the node's own best.
	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	if got := packedDictID(t, fl); got != 1 {
		t.Errorf("packed with dictionary %d, want 1", got)
	}

	// A peer on an older build turns up, and the floor drops.
	fl.sent = nil
	out.SetDictionary(d0)
	if got := out.Dictionary().ID(); got != 0 {
		t.Fatalf("outbox reports dictionary %d after being lowered, want 0", got)
	}
	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	if got := packedDictID(t, fl); got != 0 {
		t.Errorf("packed with dictionary %d after the floor dropped, want 0", got)
	}

	// The laggard upgrades, and the floor rises again.
	fl.sent = nil
	out.SetDictionary(d1)
	if err := out.SendRecords(area, recs); err != nil {
		t.Fatal(err)
	}
	if got := packedDictID(t, fl); got != 1 {
		t.Errorf("packed with dictionary %d after the floor rose, want 1", got)
	}
}

// A nil dictionary is ignored rather than installed.
//
// Compressing with nothing is not a state Pack has, so accepting one would turn
// a caller's bug into a panic on the next transmission — at which point the
// stack points here instead of at whatever resolved an ID to nothing.
func TestSetDictionaryIgnoresNil(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(21))
	if err != nil {
		t.Fatal(err)
	}
	_, d1, _ := twoDicts(t)
	fl := &fakeLink{mtu: 233, from: key.ID()}
	out, err := NewOutbox(Config{Self: key.ID(), Link: fl, Dictionary: d1})
	if err != nil {
		t.Fatal(err)
	}

	out.SetDictionary(nil)
	if out.Dictionary() != d1 {
		t.Error("a nil dictionary replaced the working one")
	}
	if err := out.SendRecords(record.AreaTagFor("general"), testRecords(t, key, record.AreaTagFor("general"), 2)); err != nil {
		t.Errorf("sending after a nil SetDictionary failed: %v", err)
	}
}

// A bundle this node cannot decompress gets its own counter and its own
// sentence (§7.4).
//
// Before this split it landed in Rejected alongside malformed frames and failed
// signatures, which points a sysop at the radios. The remedy here is at this end
// — a peer is running a newer build — and everything that peer sends will fail
// identically until this node is upgraded, so the counter is also the signal
// that negotiation is not doing its job.
func TestBundleFromANewerDictionaryIsCountedSeparately(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(22))
	if err != nil {
		t.Fatal(err)
	}
	_, d1, _ := twoDicts(t)

	// The sender holds dictionary 1; the receiver holds only 0.
	onlyZero, err := bundle.NewRawDictionary(0, []byte("meshbbs post subject from re: wrote"))
	if err != nil {
		t.Fatal(err)
	}
	receiverSet, err := bundle.NewDictionarySet(onlyZero)
	if err != nil {
		t.Fatal(err)
	}
	defer receiverSet.Close()

	area := record.AreaTagFor("general")
	fl := &fakeLink{mtu: 233, from: key.ID()}
	out, err := NewOutbox(Config{Self: key.ID(), Link: fl, Dictionary: d1})
	if err != nil {
		t.Fatal(err)
	}
	if err := out.SendRecords(area, testRecords(t, key, area, 3)); err != nil {
		t.Fatal(err)
	}

	eng := &fakeEngine{}
	in, err := NewInbox(InboxConfig{
		Engine: eng, Dictionaries: receiverSet,
		Clock: clock.NewVirtual(time.Unix(1_800_000_000, 0)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, dg := range fl.delivered() {
		if err := in.Deliver(dg); err != nil {
			t.Fatalf("deliver: %v", err)
		}
	}

	got := in.Stats()
	if got.UnknownDictionary == 0 {
		t.Error("a bundle needing an unavailable dictionary was not counted as one")
	}
	if got.Rejected != 0 {
		t.Errorf("it also landed in Rejected (%d), which points at the wrong layer", got.Rejected)
	}
	if got.RecordsAdded != 0 {
		t.Errorf("records were applied from a bundle that cannot have been read (%d)", got.RecordsAdded)
	}
}

// The sentinel is what lets every layer above tell this failure apart. Asserted
// directly so a refactor cannot quietly go back to an unstructured error.
func TestUnknownDictionaryIsASentinel(t *testing.T) {
	_, _, set := twoDicts(t)
	if _, err := set.Get(9); !errors.Is(err, bundle.ErrUnknownDictionary) {
		t.Errorf("got %v, want bundle.ErrUnknownDictionary", err)
	}
	if got := set.Highest(); got != 1 {
		t.Errorf("Highest is %d, want 1", got)
	}
}
