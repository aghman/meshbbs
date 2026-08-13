package sneakernet

import (
	"bytes"
	"context"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

type instance struct {
	st  *store.Store
	gs  *store.GossipStore
	key identity.NodeKey
	ctx context.Context
}

func newInstance(t *testing.T, seed uint64) *instance {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, store.Node{ID: key.ID(), PublicKey: key.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArea(ctx, "general", "", true); err != nil {
		t.Fatal(err)
	}
	gs, err := store.NewGossipStore(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &instance{st: st, gs: gs, key: key, ctx: ctx}
}

// post writes n records into general and makes the author's key known.
func (in *instance) post(t *testing.T, n int, body string) {
	t.Helper()
	area := record.AreaTagFor("general")
	for i := 0; i < n; i++ {
		seq, err := in.st.NextSeq(in.ctx, area)
		if err != nil {
			t.Fatal(err)
		}
		r, err := record.New(in.key, record.Record{
			Origin: in.key.ID(), Seq: seq, TS: uint32(1_765_000_000 + i),
			Type: record.TypePost, Area: area, Body: []byte(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := in.st.PutRecord(in.ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}

// learn teaches this instance another node's key, as a NODE record would.
func (in *instance) learn(t *testing.T, other *instance) {
	t.Helper()
	if err := in.st.PutNode(in.ctx, store.Node{
		ID: other.key.ID(), PublicKey: other.key.Public,
	}); err != nil {
		t.Fatal(err)
	}
}

func (in *instance) count(t *testing.T) uint64 {
	t.Helper()
	return in.gs.Vector(record.AreaTagFor("general")).Count()
}

func dicts(t *testing.T) (*bundle.Dictionary, *bundle.DictionarySet) {
	t.Helper()
	d, err := bundle.NewRawDictionary(0, []byte("meshbbs post subject from the and that with"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := bundle.NewDictionarySet(d)
	if err != nil {
		t.Fatal(err)
	}
	return d, set
}

// The two-trip exchange, which is the whole design: a stick goes out carrying
// what A has, comes back carrying only what A was missing, and both boards end
// up level without ever having spoken.
func TestATwoTripExchangeConvergesTwoBoards(t *testing.T) {
	a, b := newInstance(t, 1), newInstance(t, 2)
	a.post(t, 5, "from A")
	b.post(t, 3, "from B")
	a.learn(t, b)
	b.learn(t, a)

	dict, set := dicts(t)

	// Trip one: A writes a stick and it is carried to B.
	out, err := Export(a.gs, dict, a.key.ID(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stick bytes.Buffer
	if err := Write(&stick, out, nil); err != nil {
		t.Fatal(err)
	}

	atB, err := Read(bytes.NewReader(stick.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import(b.gs, set, atB)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 5 {
		t.Fatalf("B took %d of A's 5 records (rejected: %v)", res.Records, res.Rejected)
	}
	if got := b.count(t); got != 8 {
		t.Fatalf("B holds %d records, want 8", got)
	}

	// Trip two: B answers using the vectors that arrived on the stick. No
	// conversation happened — everything B knows about A came off the drive.
	back, err := Export(b.gs, dict, b.key.ID(), 2, atB)
	if err != nil {
		t.Fatal(err)
	}
	var reply bytes.Buffer
	if err := Write(&reply, back, nil); err != nil {
		t.Fatal(err)
	}

	atA, err := Read(bytes.NewReader(reply.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err = Import(a.gs, set, atA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 3 {
		t.Fatalf("A took %d of B's 3 records (rejected: %v)", res.Records, res.Rejected)
	}
	if got := a.count(t); got != 8 {
		t.Errorf("A holds %d records, want 8", got)
	}

	// Converged: the same vector on both sides is what convergence MEANS
	// (§7.3), rather than the same count.
	av := a.gs.Vector(record.AreaTagFor("general"))
	bv := b.gs.Vector(record.AreaTagFor("general"))
	if !av.Equal(bv) {
		t.Error("the two boards hold the same number of records but different ones")
	}
}

// The return leg must carry only the difference. Sending everything back would
// work and would waste the trip, which on a medium measured in days matters
// more than on a radio.
func TestTheReturnLegSendsOnlyTheDifference(t *testing.T) {
	a, b := newInstance(t, 3), newInstance(t, 4)
	a.post(t, 10, "from A")
	a.learn(t, b)
	b.learn(t, a)
	dict, set := dicts(t)

	// B already has 6 of A's 10.
	out, _ := Export(a.gs, dict, a.key.ID(), 1, nil)
	first, err := Import(b.gs, set, out)
	if err != nil || first.Records != 10 {
		t.Fatalf("setup: %d records, %v", first.Records, err)
	}

	// A second exchange, now that B is level. B writes its own carrier — which
	// is what states its vectors — and A answers it.
	fromB, err := Export(b.gs, dict, b.key.ID(), 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	againAtB, err := Export(a.gs, dict, a.key.ID(), 3, fromB)
	if err != nil {
		t.Fatal(err)
	}
	if len(againAtB.Bundles) != 0 {
		t.Errorf("a converged pair still packed %d bundles", len(againAtB.Bundles))
	}
	// And the vectors still travel, because that is how the other side learns
	// there is nothing to send.
	if len(againAtB.Vectors) == 0 {
		t.Error("an empty exchange carried no vectors, so it says nothing")
	}
}

// A bundle that will not unpack must not cost the rest of the stick. The person
// carrying it is not coming back today.
func TestOneBadBundleDoesNotFailTheImport(t *testing.T) {
	a, b := newInstance(t, 5), newInstance(t, 6)
	a.post(t, 4, "from A")
	a.learn(t, b)
	b.learn(t, a)
	dict, set := dicts(t)

	out, err := Export(a.gs, dict, a.key.ID(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Insert a bundle of garbage ahead of the real one.
	out.Bundles = append([][]byte{[]byte("not a bundle at all")}, out.Bundles...)

	res, err := Import(b.gs, set, out)
	if err != nil {
		t.Fatalf("the import failed outright: %v", err)
	}
	if res.Records != 4 {
		t.Errorf("took %d records, want all 4 despite the bad bundle", res.Records)
	}
	if len(res.Rejected) != 1 {
		t.Errorf("rejected %v, want exactly one bundle", res.Rejected)
	}
}

// Records still have to verify. Tolerating a bad FRAME is not tolerating a bad
// signature, and a stick is not a way around the roster.
func TestRecordsFromAnUnknownOriginAreStillRefused(t *testing.T) {
	a, b := newInstance(t, 7), newInstance(t, 8)
	a.post(t, 3, "from a stranger")
	// b never learns a's key.
	dict, set := dicts(t)

	out, err := Export(a.gs, dict, a.key.ID(), 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Import(b.gs, set, out)
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 0 {
		t.Errorf("took %d records signed by a key it holds no NODE record for", res.Records)
	}
	if got := b.count(t); got != 0 {
		t.Errorf("B's log grew to %d from an unverifiable stick", got)
	}
}

// Only files this node actually holds can be carried, and the ceilings are
// reported rather than silently applied.
func TestBlobsToCarrySkipsWhatItCannotSend(t *testing.T) {
	bs, err := blobstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	present, _, err := bs.Put(bytes.NewReader([]byte("here")))
	if err != nil {
		t.Fatal(err)
	}
	var absent blobstore.Hash
	absent[0] = 0xEE

	files := []store.File{
		{Name: "here.txt", Hash: present},
		{Name: "elsewhere.txt", Hash: absent},
		{Name: "duplicate.txt", Hash: present},
	}

	refs, skipped := BlobsToCarry(files, bs, nil)
	if len(refs) != 1 || refs[0].Hash != present {
		t.Fatalf("selected %+v", refs)
	}
	if len(skipped) != 0 {
		t.Errorf("skipped %v; a missing body and a duplicate are not worth reporting", skipped)
	}

	// A file the far side already has is not carried twice.
	refs, _ = BlobsToCarry(files, bs, map[blobstore.Hash]bool{present: true})
	if len(refs) != 0 {
		t.Errorf("carried %d files the other side already had", len(refs))
	}
}
