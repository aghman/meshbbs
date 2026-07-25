package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

func testStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	s, err := OpenMemory(ctx, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, ctx
}

func testKey(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewVirtual(time.Unix(1, 0))
	path := filepath.Join(t.TempDir(), "bbs.db")

	for i := 0; i < 3; i++ {
		s, err := Open(ctx, path, clk)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 1)

	r, err := record.New(k, record.Record{
		Seq: 1, TS: 1_700_000_000, Type: record.TypePost,
		Area: record.AreaTagFor("general"), Body: []byte("hello mesh"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(ctx, r); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRecord(ctx, r.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != r.ID() {
		t.Fatal("ID mismatch after storage round-trip")
	}
	// §6.2.1 rule 1: verification must work off the retained signed bytes.
	if err := got.Verify(k.Public); err != nil {
		t.Fatalf("record loaded from the database failed to verify: %v", err)
	}
}

// The mesh floods, so the same record arrives by several paths. Storing it
// twice must be a no-op, not an error (§6.2).
func TestPutRecordIsIdempotent(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 2)
	r, _ := record.New(k, record.Record{Seq: 1, Type: record.TypePost, Body: []byte("x")})

	for i := 0; i < 3; i++ {
		if err := s.PutRecord(ctx, r); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	n, err := s.CountRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("stored %d copies of one record, want 1", n)
	}
}

// Two DIFFERENT records at the same (origin, seq) is the silent divergence
// §6.2.1 rule 3 exists to prevent. It must be refused loudly.
func TestPutRecordRejectsSeqConflict(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 3)

	a, _ := record.New(k, record.Record{Seq: 7, Type: record.TypePost, Body: []byte("first")})
	b, _ := record.New(k, record.Record{Seq: 7, Type: record.TypePost, Body: []byte("second")})

	if err := s.PutRecord(ctx, a); err != nil {
		t.Fatal(err)
	}
	err := s.PutRecord(ctx, b)
	if !errors.Is(err, ErrSeqConflict) {
		t.Fatalf("expected ErrSeqConflict, got %v", err)
	}
}

// §6.2.1 rule 3: the high-water mark advances durably, and never repeats.
func TestNextSeqIsMonotonic(t *testing.T) {
	s, ctx := testStore(t)
	seen := map[uint64]bool{}
	var last uint64
	for i := 0; i < 100; i++ {
		n, err := s.NextSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if seen[n] {
			t.Fatalf("sequence number %d issued twice", n)
		}
		if n <= last {
			t.Fatalf("sequence went backwards: %d after %d", n, last)
		}
		seen[n] = true
		last = n
	}
}

// The restore-from-backup scenario. If the durable high-water mark is behind
// the records we actually hold, we are about to reissue sequence numbers that
// peers have already accepted with different content — permanent, silent
// divergence. CheckSeqIntegrity must detect it, repair the mark, and bump the
// incarnation so peers know to re-verify (§6.2.1 rule 3).
func TestSeqRegressionIsDetectedAndRepaired(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 4)
	self := k.ID()

	// Write records at seq 1..5, as a healthy node would.
	for seq := uint64(1); seq <= 5; seq++ {
		if _, err := s.NextSeq(ctx); err != nil {
			t.Fatal(err)
		}
		r, err := record.New(k, record.Record{Seq: seq, Type: record.TypePost, Body: []byte("post")})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	before, err := s.SeqState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.HighWater != 5 {
		t.Fatalf("high-water is %d, want 5", before.HighWater)
	}

	// No regression yet.
	repaired, err := s.CheckSeqIntegrity(ctx, self[:])
	if err != nil {
		t.Fatal(err)
	}
	if repaired {
		t.Fatal("reported a regression on a healthy database")
	}

	// Simulate the restore: roll the durable mark back to 2 while the records
	// at 3..5 remain. This is exactly what an older backup of seq_state, or a
	// partially restored database, looks like.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE seq_state SET high_water = 2 WHERE only_row = 1`); err != nil {
		t.Fatal(err)
	}

	repaired, err = s.CheckSeqIntegrity(ctx, self[:])
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("failed to detect a sequence regression — this is the silent divergence case")
	}

	after, err := s.SeqState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.HighWater != 5 {
		t.Fatalf("high-water repaired to %d, want 5", after.HighWater)
	}
	if after.Incarnation != before.Incarnation+1 {
		t.Fatalf("incarnation is %d, want %d — peers need this signal to re-verify",
			after.Incarnation, before.Incarnation+1)
	}

	// Sequence numbers issued after repair must not collide with stored ones.
	next, err := s.NextSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if next != 6 {
		t.Fatalf("next sequence after repair is %d, want 6", next)
	}
}

// A version vector asserts everything up to N was received, so a gap stops the
// count (§7.3).
func TestHighWaterForStopsAtGaps(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 5)

	for _, seq := range []uint64{1, 2, 4, 5} {
		r, _ := record.New(k, record.Record{Seq: seq, Type: record.TypePost, Body: []byte("x")})
		if err := s.PutRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.HighWaterFor(ctx, k.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("high water is %d, want 2 (contiguous prefix only)", got)
	}
}

// §6.1.2: a NODE record verifies with no external input, so it is safe to
// accept from any peer.
func TestNodeRosterFromRecord(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 6)

	r, err := record.NewNodeRecord(k, 1, 1_700_000_000, "pnw-bbs", "sysop@example", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutNodeFromRecord(ctx, r); err != nil {
		t.Fatal(err)
	}

	n, err := s.GetNode(ctx, k.ID())
	if err != nil {
		t.Fatal(err)
	}
	if n.DisplayName != "pnw-bbs" {
		t.Fatalf("display name is %q", n.DisplayName)
	}
	if !n.ID.Matches(n.PublicKey) {
		t.Fatal("stored node's key does not hash to its ID")
	}
}

// The database must not be able to hold a row whose key and ID disagree, since
// every signature check depends on that invariant.
func TestPutNodeRejectsMismatchedKey(t *testing.T) {
	s, ctx := testStore(t)
	a, b := testKey(t, 7), testKey(t, 8)

	err := s.PutNode(ctx, Node{ID: a.ID(), PublicKey: b.Public})
	if err == nil {
		t.Fatal("stored a node whose public key does not hash to its ID")
	}
}

// §6.1.4.1: literal IDs resolve first, and aliases that look like IDs are
// refused at creation so the namespaces cannot collide.
func TestAliasResolution(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 9)

	if err := s.SetAlias(ctx, "pnw", k.ID()); err != nil {
		t.Fatal(err)
	}

	got, err := s.ResolveNodeRef(ctx, "pnw")
	if err != nil {
		t.Fatal(err)
	}
	if got != k.ID() {
		t.Fatal("alias resolved to the wrong node")
	}

	// A literal ID resolves without needing an alias.
	got, err = s.ResolveNodeRef(ctx, k.ID().Compact())
	if err != nil {
		t.Fatal(err)
	}
	if got != k.ID() {
		t.Fatal("literal ID did not resolve to itself")
	}

	// The grouped and word forms work too.
	if _, err := s.ResolveNodeRef(ctx, k.ID().String()); err != nil {
		t.Fatalf("grouped form failed to resolve: %v", err)
	}
	if _, err := s.ResolveNodeRef(ctx, k.ID().Words()); err != nil {
		t.Fatalf("word form failed to resolve: %v", err)
	}

	// An unknown reference must explain the remedy, since users cannot create
	// aliases themselves ([N1]).
	_, err = s.ResolveNodeRef(ctx, "nosuchnode")
	if err == nil {
		t.Fatal("expected an error for an unknown reference")
	}
}

func TestAliasValidation(t *testing.T) {
	k := testKey(t, 10)
	// An alias that is also a valid node ID would be permanently shadowed.
	if err := ValidateAlias(k.ID().Compact()); err == nil {
		t.Fatal("accepted an alias that is a valid node ID")
	}
	for _, bad := range []string{"", "has space", "has/slash", "bad@char"} {
		if err := ValidateAlias(bad); err == nil {
			t.Errorf("accepted invalid alias %q", bad)
		}
	}
	for _, good := range []string{"pnw", "fog-city", "test_1", "a.b"} {
		if err := ValidateAlias(good); err != nil {
			t.Errorf("rejected valid alias %q: %v", good, err)
		}
	}
}

// [N7]: new accounts must NOT get post_federated — open front door, gated
// commons. This is the single most consequential default in user creation.
func TestNewUsersLackFederatedPosting(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "austin", CanLogin: true}); err != nil {
		t.Fatal(err)
	}

	has, err := s.HasCapability(ctx, "austin", CapPostFederated)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Fatal("a new user was granted post_federated by default; " +
			"this would let anyone spend the network's shared airtime ([N7])")
	}

	// The sysop can grant it explicitly.
	if err := s.GrantCapability(ctx, "austin", CapPostFederated, "sysop"); err != nil {
		t.Fatal(err)
	}
	has, _ = s.HasCapability(ctx, "austin", CapPostFederated)
	if !has {
		t.Fatal("grant did not take effect")
	}
}

// [N9]: listed by default.
func TestNewUsersAreDirectoryListed(t *testing.T) {
	s, ctx := testStore(t)
	u, err := s.CreateUser(ctx, CreateUserOptions{Nick: "bob", CanLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	if !u.DirectoryListed {
		t.Fatal("new user is unlisted; [N9] says listed by default")
	}
}

func TestNickRules(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "new"}); !errors.Is(err, ErrNickReserved) {
		t.Fatalf("expected ErrNickReserved for 'new', got %v", err)
	}
	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "guest"}); !errors.Is(err, ErrNickReserved) {
		t.Fatalf("expected ErrNickReserved for 'guest', got %v", err)
	}

	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "austin"}); err != nil {
		t.Fatal(err)
	}
	// Uniqueness is case-insensitive within the instance.
	if _, err := s.CreateUser(ctx, CreateUserOptions{Nick: "AUSTIN"}); !errors.Is(err, ErrNickTaken) {
		t.Fatalf("expected ErrNickTaken for a case variant, got %v", err)
	}

	for _, bad := range []string{"a", "1abc", "has space", "waaaaaaaaaaaaaaaaay-too-long"} {
		if err := ValidateNick(bad); err == nil {
			t.Errorf("accepted invalid nick %q", bad)
		}
	}
}

// There is no email column ([N8]). Assert the schema has none, so a future
// change has to be deliberate.
func TestSchemaHasNoEmailColumn(t *testing.T) {
	s, ctx := testStore(t)
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('users')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "email" {
			t.Fatal("users table has an email column; [N8] decided not to collect email")
		}
	}
}

// Regression: the connection pool is capped at one connection (see Open), so a
// method that issues a second query while a result set is still open
// deadlocks rather than erroring — the process simply stops. ListPosts had
// exactly this bug (it called SelfNode mid-stream).
//
// This test would hang rather than fail if it regressed, so the package's
// -timeout is what turns it into a red build. Keep it cheap so it always runs.
func TestListPostsDoesNotDeadlockOnTheSingleConnection(t *testing.T) {
	s, ctx := testStore(t)
	k := testKey(t, 21)
	if err := s.PutNode(ctx, Node{ID: k.ID(), PublicKey: k.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateArea(ctx, "general", "", false); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.ListPosts(ctx, "general", 10)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListPosts did not return within 10s: it is holding the single " +
			"pooled connection while issuing another query")
	}
}
