package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

// Migration 0007 rebuilds `areas`, and `files` references it ON DELETE CASCADE.
// If foreign keys are enforced during that rebuild, the DROP takes every file
// row with it — silently, because a cascade is not an error.
//
// This is the test that would have caught that, and it is written against the
// real migration runner rather than against a hand-rolled rebuild, because the
// thing being tested is that Store.migrate suspends enforcement at all.
func TestMigrationRebuildKeepsFileRowsAndTheirAreas(t *testing.T) {
	ctx := context.Background()
	st, err := OpenMemory(ctx, clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	area, err := st.CreateFileArea(ctx, "utils", "tools", false)
	if err != nil {
		t.Fatal(err)
	}
	var h blobstore.Hash
	for i := range h {
		h[i] = 0x22
	}
	if _, err := st.PutFile(ctx, "utils", File{
		Name: "readme.txt", Hash: h, Size: 12, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	// Force 0007 to run AGAIN, now that there is data to lose.
	//
	// Without this the test is vacuous: OpenMemory applies every migration
	// before the first row exists, so the rebuild happens on an empty table and
	// a cascade has nothing to delete. Forgetting the migration and re-running
	// is what a real upgrade looks like — an existing database, with files in
	// it, meeting this migration for the first time.
	if _, err := st.db.ExecContext(ctx,
		`DELETE FROM schema_migrations WHERE name = '0007_door_areas.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := st.migrate(ctx); err != nil {
		t.Fatalf("re-running the rebuild over existing data: %v", err)
	}

	files, err := st.ListAreaContents(ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "readme.txt" {
		t.Fatalf("the file area rebuild lost its contents: %+v", files)
	}
	got, err := st.GetFileArea(ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != area.ID {
		t.Errorf("area id changed across the rebuild: %d -> %d; files.area_id "+
			"references it, so this would orphan every row", area.ID, got.ID)
	}
	if got.Tag != area.Tag {
		t.Errorf("area tag changed across the rebuild: %x -> %x", area.Tag[:], got.Tag[:])
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
