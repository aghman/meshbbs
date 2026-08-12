package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

// Migration 0007 rebuilds `areas`, and `files` references it ON DELETE CASCADE.
// If foreign keys are enforced during that rebuild, the DROP takes every file
// row with it — silently, because a cascade is not an error.
//
// # Why this builds an old database rather than rewinding a new one
//
// The obvious version applies every migration, then deletes the bookkeeping for
// the ones under test and re-runs them. It does not work and it does not stay
// working: ALTER TABLE ADD COLUMN is not idempotent, and every migration added
// after 0007 has to be hand-undone in the test or it fails with a confusing
// duplicate-column error. That is a treadmill, and the sort that gets a test
// deleted rather than fixed.
//
// So this constructs the database as it was at 0006 — before the rebuild
// existed — puts real rows in it, and then runs the migrator forward exactly
// as an upgrade would. Nothing needs touching when migration 0011 is written.
func TestMigrationRebuildKeepsFileRowsAndTheirAreas(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	st := &Store{db: db, clock: clock.NewReal()}

	applyMigrationsThrough(t, st, "0006")

	// Raw SQL, because the Store's own helpers project columns that later
	// migrations add and this database does not have yet.
	hash := bytes.Repeat([]byte{0x22}, 32)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO areas (name, tag, description, federated, read_only, retention_days, kind, created_at)
		 VALUES ('utils', X'054F38AE', 'tools', 0, 0, 0, 'file', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO blobs (hash, size, created_at) VALUES (?, 12, 1)`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO files (area_id, name, hash, uploader, uploaded_at)
		 VALUES ((SELECT id FROM areas WHERE name = 'utils'), 'readme.txt', ?, 'austin', 1)`,
		hash); err != nil {
		t.Fatal(err)
	}

	var areaID int64
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM areas WHERE name = 'utils'`).Scan(&areaID); err != nil {
		t.Fatal(err)
	}

	// The upgrade itself.
	if err := st.migrate(ctx); err != nil {
		t.Fatalf("upgrading a populated database: %v", err)
	}

	var files int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE name = 'readme.txt'`).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("the file area rebuild lost its contents: %d rows left", files)
	}

	got, err := st.GetFileArea(ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != areaID {
		t.Errorf("area id changed across the rebuild: %d -> %d; files.area_id "+
			"references it, so this would orphan every row", areaID, got.ID)
	}
}

// applyMigrationsThrough runs the migrations up to and including limit, and
// records them, so a later migrate() picks up exactly where this left off.
func applyMigrationsThrough(t *testing.T, s *Store, limit string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY NOT NULL,
			applied_at INTEGER NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	applied := 0
	for _, name := range names {
		if name > limit && !strings.HasPrefix(name, limit) {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("applying %s: %v", name, err)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, 1)`, name); err != nil {
			t.Fatal(err)
		}
		applied++
	}
	if applied == 0 {
		t.Fatalf("no migrations matched %q; has the numbering changed?", limit)
	}
}

// The CHECK is the point of the rebuild: it must now admit 'door' and must
// still refuse anything else.
func TestAreaKindCheckAdmitsDoorAndNothingElse(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateDoorArea(ctx, "lordleague", "LORD", false); err != nil {
		t.Fatalf("creating a door league: %v", err)
	}

	// Straight past the Go API, because the constraint is what a direct
	// sqlite3 session runs into and the Go-side kind constants are not.
	_, err = st.db.ExecContext(ctx,
		`INSERT INTO areas (name, tag, description, federated, kind, created_at)
		 VALUES ('bogus', X'01020304', '', 0, 'sandwich', 0)`)
	if err == nil {
		t.Error("the areas table accepted a kind of 'sandwich'")
	}
}

// A name is spent once across all three kinds, because all three derive the
// same tag from it — the collision migrations 0005 and 0007 exist to prevent.
func TestAnAreaNameIsSpentAcrossEveryKind(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateDoorArea(ctx, "arena", "", false); err != nil {
		t.Fatal(err)
	}
	for name, create := range map[string]func(context.Context, string, string, bool) (Area, error){
		"message": st.CreateArea,
		"file":    st.CreateFileArea,
		"door":    st.CreateDoorArea,
	} {
		if _, err := create(ctx, "arena", "", false); !errors.Is(err, ErrAreaExists) {
			t.Errorf("creating a %s area named arena returned %v, want ErrAreaExists", name, err)
		}
	}
}

// Kind-scoped lookups must refuse the other kinds, and must say which kind the
// area actually is. Naming the other one was fine with two kinds and became a
// guess with three.
func TestKindScopedLookupsNameTheActualKind(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateDoorArea(ctx, "league", "", false); err != nil {
		t.Fatal(err)
	}

	for _, get := range []func(context.Context, string) (Area, error){st.GetArea, st.GetFileArea} {
		_, err := get(ctx, "league")
		if !errors.Is(err, ErrWrongAreaKind) {
			t.Fatalf("got %v, want ErrWrongAreaKind", err)
		}
		if !strings.Contains(err.Error(), "door league") {
			t.Errorf("error does not say what kind it actually is: %v", err)
		}
	}
	if _, err := st.GetDoorArea(ctx, "league"); err != nil {
		t.Errorf("GetDoorArea refused a door league: %v", err)
	}
}

// The two symmetric refusals in Apply. Each catches a different mistake and
// both matter, because a record accepted here is one this node will relay.
func TestApplyKeepsDoorEventsAndPostsInTheirOwnAreas(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	key, err := identity.GenerateNodeKey(rng.TestSecret(31))
	if err != nil {
		t.Fatal(err)
	}
	// The peer's NODE record has to land first or every other record is
	// quarantined for want of a key, which would make these assertions pass for
	// the wrong reason.
	node, err := record.NewNodeRecord(key, 1, 1, "peer", "", 1)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.CreateDoorArea(ctx, "league", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArea(ctx, "general", "", true); err != nil {
		t.Fatal(err)
	}

	var problems []error
	g, err := NewGossipStore(ctx, st, func(e error) { problems = append(problems, e) })
	if err != nil {
		t.Fatal(err)
	}
	if n, err := g.Apply(RosterArea, []*record.Record{node}); err != nil || n != 1 {
		t.Fatalf("seeding the roster: n=%d err=%v", n, err)
	}

	league := record.AreaTagFor("league")
	general := record.AreaTagFor("general")

	doorEvent := func(seq uint64, area record.AreaTag) *record.Record {
		r, err := record.NewDoorEventRecord(key, seq, 1, area, record.DoorEventBody{
			Game: "lord", Events: []record.DoorEvent{{Kind: 1, Actor: "alice"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	post := func(seq uint64, area record.AreaTag) *record.Record {
		r, err := record.New(key, record.Record{
			Origin: key.ID(), Seq: seq, TS: 1, Type: record.TypePost,
			Area: area, Body: []byte("hello"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	t.Run("a door event belongs in a league", func(t *testing.T) {
		if n, err := g.Apply(league, []*record.Record{doorEvent(2, league)}); err != nil || n != 1 {
			t.Fatalf("n=%d err=%v", n, err)
		}
	})

	t.Run("a door event in a forum is refused", func(t *testing.T) {
		problems = nil
		n, err := g.Apply(general, []*record.Record{doorEvent(3, general)})
		if err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if len(problems) == 0 || !strings.Contains(problems[0].Error(), "not a door league") {
			t.Errorf("problems = %v", problems)
		}
	})

	t.Run("a post in a league is refused", func(t *testing.T) {
		problems = nil
		n, err := g.Apply(league, []*record.Record{post(4, league)})
		if err != nil || n != 0 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if len(problems) == 0 || !strings.Contains(problems[0].Error(), "carries only door events") {
			t.Errorf("problems = %v", problems)
		}
	})

	t.Run("the roster is unaffected", func(t *testing.T) {
		// RosterArea is the zero tag and is not in the areas table at all, so a
		// naive "is this a door area" lookup returns false for it — which is
		// right, and worth pinning, because the roster is the one area whose
		// rejection would break the bootstrap entirely.
		problems = nil
		next, err := record.NewNodeRecord(key, 5, 2, "peer", "", 2)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := g.Apply(RosterArea, []*record.Record{next}); err != nil || n != 1 {
			t.Fatalf("n=%d err=%v problems=%v", n, err, problems)
		}
	})
}

// A league that is not federated must not be classified as one for traffic
// purposes, because Refresh only ever sees federated areas.
func TestIsDoorAreaFollowsFederation(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateDoorArea(ctx, "league", "", false); err != nil {
		t.Fatal(err)
	}
	g, err := NewGossipStore(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	tag := record.AreaTagFor("league")
	if g.IsDoorArea(tag) {
		t.Error("an unfederated league was classified as a door area")
	}

	if err := st.SetAreaFederated(ctx, "league", true, "sysop"); err != nil {
		t.Fatal(err)
	}
	if err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if !g.IsDoorArea(tag) {
		t.Error("a federated league was not classified after Refresh")
	}
	if g.IsFileArea(tag) {
		t.Error("a league was classified as a file area")
	}
}

// §11.3's cross-field rule, enforced where the field is written because SQLite
// cannot express "these rows add up" as a CHECK.
func TestAreaSharesMustFitInOneBudget(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := st.CreateArea(ctx, name, "", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetAreaShare(ctx, "alpha", 0.6, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAreaShare(ctx, "beta", 0.4, "test"); err != nil {
		t.Fatalf("0.6 + 0.4 should fit exactly: %v", err)
	}
	if err := st.SetAreaShare(ctx, "gamma", 0.1, "test"); !errors.Is(err, ErrShareOverBudget) {
		t.Errorf("got %v, want ErrShareOverBudget", err)
	}

	// Re-setting an area's own share compares against the OTHERS, so lowering
	// or re-applying one must never trip on itself.
	if err := st.SetAreaShare(ctx, "alpha", 0.5, "test"); err != nil {
		t.Errorf("lowering an existing share was refused: %v", err)
	}
	if err := st.SetAreaShare(ctx, "beta", 0.4, "test"); err != nil {
		t.Errorf("re-applying an unchanged share was refused: %v", err)
	}

	// Out of range in either direction.
	for _, bad := range []float64{-0.1, 1.5} {
		if err := st.SetAreaShare(ctx, "gamma", bad, "test"); err == nil {
			t.Errorf("a share of %v was accepted", bad)
		}
	}
}

// A local-only area spends no mesh airtime, so it must not reserve any.
func TestLocalAreasDoNotConsumeTheAirtimeBudget(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateArea(ctx, "localonly", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArea(ctx, "onthemesh", "", true); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAreaShare(ctx, "localonly", 0.9, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAreaShare(ctx, "onthemesh", 0.9, "test"); err != nil {
		t.Errorf("a local-only area's share counted against the mesh budget: %v", err)
	}

	// And only the federated one reaches the governor.
	shares, err := st.AreaShares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shares[record.AreaTagFor("localonly")]; ok {
		t.Error("a local-only area was handed to the governor")
	}
	if got := shares[record.AreaTagFor("onthemesh")]; got != 0.9 {
		t.Errorf("federated share = %v, want 0.9", got)
	}
}
