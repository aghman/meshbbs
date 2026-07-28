package store

import (
	"context"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/vv"
)

func gossipFixture(t *testing.T) (*Store, *GossipStore, context.Context, record.AreaTag) {
	t.Helper()
	st, ctx := testStore(t)

	area, err := st.CreateArea(ctx, "general", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	// A non-federated area must never appear to the engine.
	if _, err := st.CreateArea(ctx, "local", "not on the air", false); err != nil {
		t.Fatal(err)
	}

	g, err := NewGossipStore(ctx, st, func(err error) { t.Logf("gossipstore: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	return st, g, ctx, area.Tag
}

func signedRecord(t *testing.T, key identity.NodeKey, area record.AreaTag, seq uint64) *record.Record {
	t.Helper()
	r, err := record.New(key, record.Record{
		Origin: key.ID(),
		Seq:    seq,
		TS:     uint32(time.Unix(1_800_000_000, 0).Add(time.Duration(seq) * time.Minute).Unix()),
		Type:   record.TypePost,
		Area:   area,
		Body:   []byte("subject: from the mesh\n\nbody text"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func trustedKey(t *testing.T, st *Store, ctx context.Context, seed uint64) identity.NodeKey {
	t.Helper()
	key, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, Node{ID: key.ID(), PublicKey: key.Public, DisplayName: "peer"}); err != nil {
		t.Fatal(err)
	}
	return key
}

// Only federated areas reach the engine — plus the roster, which is always
// synced because it is how trust bootstraps. A local-only area is one a sysop
// chose not to put on the air, and leaking it would be a privacy failure.
func TestOnlyFederatedAreasAreOffered(t *testing.T) {
	_, g, _, area := gossipFixture(t)

	areas := g.Areas()
	if len(areas) != 2 {
		t.Fatalf("areas = %v, want the roster and the one federated area", areas)
	}
	if areas[0] != RosterArea {
		t.Errorf("areas[0] = %v, want the roster area", areas[0])
	}
	if areas[1] != area {
		t.Errorf("areas[1] = %v, want the federated area", areas[1])
	}
	for _, a := range areas {
		if a != RosterArea && a != area {
			t.Errorf("a non-federated area leaked to the engine: %v", a)
		}
	}
}

// The property the whole protocol rests on: a version vector reports the
// CONTIGUOUS high-water mark. Claiming a sequence we have a gap below would
// make anti-entropy skip the gap forever.
func TestVectorReportsContiguousHighWaterMark(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)
	key := trustedKey(t, st, ctx, 1)

	// Hold 1, 2 and 4 — the gap at 3 is what a lossy mesh produces routinely.
	for _, seq := range []uint64{1, 2, 4} {
		if err := st.PutRecord(ctx, signedRecord(t, key, area, seq)); err != nil {
			t.Fatal(err)
		}
	}

	if got := g.Vector(area).Get(key.ID()); got != 2 {
		t.Errorf("vector = %d, want 2 — the contiguous run, not the maximum", got)
	}

	// Filling the gap advances it past the record that was already held.
	if err := st.PutRecord(ctx, signedRecord(t, key, area, 3)); err != nil {
		t.Fatal(err)
	}
	if got := g.Vector(area).Get(key.ID()); got != 4 {
		t.Errorf("vector = %d after filling the gap, want 4", got)
	}
}

func TestRecordsReturnsARangeInOrder(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)
	key := trustedKey(t, st, ctx, 1)
	for seq := uint64(1); seq <= 5; seq++ {
		if err := st.PutRecord(ctx, signedRecord(t, key, area, seq)); err != nil {
			t.Fatal(err)
		}
	}

	got := g.Records(area, vv.Range{Origin: key.ID(), From: 2, To: 4})
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	for i, r := range got {
		if want := uint64(i + 2); r.Seq != want {
			t.Errorf("record %d has seq %d, want %d", i, r.Seq, want)
		}
	}

	// Records are rebuilt from the exact signed bytes (§6.2.1 rule 1), so they
	// still verify after a round trip through the database.
	for _, r := range got {
		if err := r.Verify(key.Public); err != nil {
			t.Errorf("a stored record no longer verifies: %v", err)
		}
	}
}

// Apply is the last gate before the log, and it is the one that matters:
// everything arriving from the mesh comes through here.
func TestApplyVerifiesAndCounts(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)
	key := trustedKey(t, st, ctx, 1)

	recs := []*record.Record{
		signedRecord(t, key, area, 1),
		signedRecord(t, key, area, 2),
	}
	added, err := g.Apply(area, recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}

	// Duplicate delivery is routine on a flooding mesh and must count as zero
	// new — otherwise two converged nodes keep telling each other something
	// changed, and each exchange costs a second of channel time.
	added, err = g.Apply(area, recs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Errorf("re-applying counted %d new records", added)
	}
}

// A record from an origin whose key we do not hold is quarantined, not stored.
// §6.1.2 says the NODE record may be minutes behind on a lossy mesh.
func TestApplyRejectsUnknownOrigins(t *testing.T) {
	_, g, _, area := gossipFixture(t)
	stranger, err := identity.GenerateNodeKey(rng.TestSecret(99))
	if err != nil {
		t.Fatal(err)
	}

	added, err := g.Apply(area, []*record.Record{signedRecord(t, stranger, area, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Error("stored a record from an origin with no known key")
	}
}

// A tampered record must not enter the log even when its origin is known and
// trusted. This is the last gate before storage, and everything arriving from
// the mesh comes through it.
func TestApplyRejectsTamperedRecords(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)
	victim := trustedKey(t, st, ctx, 1)

	good := signedRecord(t, victim, area, 1)
	// Rebuild it with one bit of the signature flipped, which is what a record
	// mangled in transit or by an attacker looks like from here.
	raw := append(append([]byte(nil), good.SignedBytes()...), good.Signature()...)
	raw[len(raw)-1] ^= 0x01
	tampered, err := record.Unmarshal(raw)
	if err != nil {
		// Rejected at parse time is also a pass: it never became a record.
		return
	}

	added, err := g.Apply(area, []*record.Record{tampered})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Fatal("a tampered record was accepted")
	}
	if n, _ := st.CountRecords(ctx); n != 0 {
		t.Errorf("%d records in the log after a tampering attempt", n)
	}
}

// The defence one layer down: a record cannot even be CONSTRUCTED claiming an
// origin the signer does not hold the key for, so the obvious forgery is
// impossible before it reaches any gate.
func TestRecordsCannotBeSignedForAnotherOrigin(t *testing.T) {
	_, _, _, area := gossipFixture(t)
	victim, err := identity.GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	attacker, err := identity.GenerateNodeKey(rng.TestSecret(2))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := record.New(attacker, record.Record{
		Origin: victim.ID(), Seq: 1, TS: 1_800_000_000,
		Type: record.TypePost, Area: area, Body: []byte("not mine to write"),
	}); err == nil {
		t.Fatal("signed a record claiming another node's origin")
	}
}

// A record claiming a different area than the bundle carrying it is either a
// bug or an attempt to write where the sender does not federate.
func TestApplyRejectsAreaMismatch(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)
	key := trustedKey(t, st, ctx, 1)
	other := record.AreaTag{'x', 'x', 'x', 'x'}

	added, err := g.Apply(area, []*record.Record{signedRecord(t, key, other, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if added != 0 {
		t.Error("a record was written into an area it did not claim")
	}
}

// One divergent coordinate must not stop the rest of a bundle from landing.
func TestApplySurvivesADivergentRecord(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)
	key := trustedKey(t, st, ctx, 1)

	first := signedRecord(t, key, area, 1)
	if err := st.PutRecord(ctx, first); err != nil {
		t.Fatal(err)
	}

	// A DIFFERENT record at the same (origin, seq) — §6.2.1 rule 3's
	// unrecoverable divergence.
	divergent, err := record.New(key, record.Record{
		Origin: key.ID(), Seq: 1, TS: 1_800_000_001,
		Type: record.TypePost, Area: area, Body: []byte("a different body"),
	})
	if err != nil {
		t.Fatal(err)
	}

	added, err := g.Apply(area, []*record.Record{divergent, signedRecord(t, key, area, 2)})
	if err != nil {
		t.Fatalf("a divergent record aborted the whole bundle: %v", err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1 — the good record only", added)
	}
}

// Areas appear while the BBS is running, so the engine's view must be able to
// change without a restart.
func TestRefreshPicksUpNewAreas(t *testing.T) {
	st, g, ctx, _ := gossipFixture(t)

	if _, err := st.CreateArea(ctx, "newsroom", "", true); err != nil {
		t.Fatal(err)
	}
	// Roster plus the one area the fixture federated.
	if len(g.Areas()) != 2 {
		t.Error("a new area appeared without a refresh")
	}
	if err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if len(g.Areas()) != 3 {
		t.Errorf("areas = %v after refresh, want the roster and two areas", g.Areas())
	}
}

// The bootstrap, and the reason the roster area is always federated: a peer we
// have never heard of must be able to become one whose posts we can verify,
// using nothing but what arrives over the air.
func TestNodeRecordsBootstrapTheirOwnTrust(t *testing.T) {
	st, g, ctx, area := gossipFixture(t)

	stranger, err := identity.GenerateNodeKey(rng.TestSecret(42))
	if err != nil {
		t.Fatal(err)
	}

	// A post from a node we hold no key for is quarantined.
	post := signedRecord(t, stranger, area, 2)
	if added, err := g.Apply(area, []*record.Record{post}); err != nil || added != 0 {
		t.Fatalf("added = %d, err = %v — an unknown origin's post was accepted", added, err)
	}

	// Its NODE record arrives, carrying the key that hashes to its own origin.
	nodeRec, err := record.NewNodeRecord(stranger, 1, 1_800_000_000, "stranger", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	added, err := g.Apply(RosterArea, []*record.Record{nodeRec})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("the NODE record was not accepted (added = %d)", added)
	}

	// And now the same post verifies.
	added, err = g.Apply(area, []*record.Record{post})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatal("a post still failed to verify after its NODE record arrived")
	}

	// The roster row is real, not just the record.
	if _, err := st.GetNode(ctx, stranger.ID()); err != nil {
		t.Errorf("the peer is not in the roster: %v", err)
	}
}

// The roster syncs whatever the sysop configured, because it is how trust
// bootstraps rather than something they opted into.
func TestRosterAreaIsAlwaysFederated(t *testing.T) {
	_, g, _, _ := gossipFixture(t)
	areas := g.Areas()
	if len(areas) == 0 || areas[0] != RosterArea {
		t.Fatalf("areas = %v, want the roster area first", areas)
	}
}
