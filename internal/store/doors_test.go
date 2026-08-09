package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDoor() Door {
	return Door{
		Name:            "tradewars",
		Path:            "/opt/doors/tw/tw2002",
		Args:            []string{"-node", "1"},
		Cwd:             "/opt/doors/tw",
		EnvPassthrough:  []string{"TZ"},
		DropfileType:    DropfileNone,
		MaxConcurrent:   4,
		NodeLock:        true,
		WallClock:       90 * time.Minute,
		APILevel:        APIAnnounce,
		AnnounceArea:    "games",
		AnnouncePerHour: 2,
		StateQuota:      4096,
		Enabled:         true,
	}
}

func TestDoorRoundTrips(t *testing.T) {
	s, ctx := testStore(t)

	want := testDoor()
	if err := s.PutDoor(ctx, want, "sysop"); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.GetDoor(ctx, want.Name)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.CreatedAt = 0

	if got.Name != want.Name || got.Path != want.Path || got.Cwd != want.Cwd {
		t.Errorf("identity round-tripped as %+v", got)
	}
	if strings.Join(got.Args, ",") != strings.Join(want.Args, ",") {
		t.Errorf("args round-tripped as %v, want %v", got.Args, want.Args)
	}
	if strings.Join(got.EnvPassthrough, ",") != strings.Join(want.EnvPassthrough, ",") {
		t.Errorf("env_passthrough round-tripped as %v, want %v",
			got.EnvPassthrough, want.EnvPassthrough)
	}
	if got.WallClock != want.WallClock {
		t.Errorf("wall clock round-tripped as %v, want %v", got.WallClock, want.WallClock)
	}
	if !got.NodeLock || !got.Enabled {
		t.Errorf("flags round-tripped as node_lock=%v enabled=%v", got.NodeLock, got.Enabled)
	}
	if got.APILevel != want.APILevel {
		t.Errorf("api level round-tripped as %d, want %d", got.APILevel, want.APILevel)
	}
}

// An empty args list must come back empty rather than nil, because the runner
// passes it straight to argv and a JSON null there is a decode error later
// rather than an empty list now.
func TestDoorEmptyListsRoundTripAsEmpty(t *testing.T) {
	s, ctx := testStore(t)

	d := testDoor()
	d.Args = nil
	d.EnvPassthrough = nil
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.GetDoor(ctx, d.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Args == nil || len(got.Args) != 0 {
		t.Errorf("args round-tripped as %#v, want an empty slice", got.Args)
	}
	if got.EnvPassthrough == nil || len(got.EnvPassthrough) != 0 {
		t.Errorf("env_passthrough round-tripped as %#v, want an empty slice", got.EnvPassthrough)
	}
}

// §9.1.1: act_as_user is a per-door sysop grant, and the sort of decision
// somebody should be able to find afterwards.
func TestGrantingActAsUserIsAudited(t *testing.T) {
	s, ctx := testStore(t)

	ordinary := testDoor()
	if err := s.PutDoor(ctx, ordinary, "sysop"); err != nil {
		t.Fatal(err)
	}
	if n := countAudit(t, s, "door.grant_act_as_user"); n != 0 {
		t.Errorf("a level-%d door logged %d act_as_user grants", ordinary.APILevel, n)
	}

	elevated := testDoor()
	elevated.APILevel = APIActAsUser
	if err := s.PutDoor(ctx, elevated, "sysop"); err != nil {
		t.Fatal(err)
	}
	if n := countAudit(t, s, "door.grant_act_as_user"); n != 1 {
		t.Errorf("granting act_as_user logged %d audit rows, want 1", n)
	}
}

func TestDoorValidation(t *testing.T) {
	s, ctx := testStore(t)

	cases := []struct {
		name string
		edit func(*Door)
		want string
	}{
		{"no name", func(d *Door) { d.Name = " " }, "name"},
		{"no path", func(d *Door) { d.Path = "" }, "path"},
		{"no working directory", func(d *Door) { d.Cwd = "" }, "working directory"},
		{"api level too high", func(d *Door) { d.APILevel = 5 }, "api_level"},
		{"api level too low", func(d *Door) { d.APILevel = 0 }, "api_level"},
		{"no wall clock", func(d *Door) { d.WallClock = 0 }, "wall-clock"},
		{"unknown dropfile", func(d *Door) { d.DropfileType = "door64.sys" }, "dropfile"},
		{"unknown capability", func(d *Door) { d.RequiredCapability = "be_excellent" }, "capability"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDoor()
			tc.edit(&d)
			err := s.PutDoor(ctx, d, "sysop")
			if err == nil {
				t.Fatalf("accepted %+v", d)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The limits are stored as configured, whatever this platform can do with
// them: what is enforceable is internal/door's question, and the launcher
// refuses a door whose limits cannot be applied here.
func TestDoorLimitsRoundTrip(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	d.CPULimit = 90 * time.Second
	d.MemLimit = 1 << 20
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDoor(ctx, d.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.CPULimit != d.CPULimit || got.MemLimit != d.MemLimit {
		t.Errorf("limits round-tripped as cpu=%v mem=%d", got.CPULimit, got.MemLimit)
	}
}

// An unset announce area is not a rate limit of zero: there is nowhere to post.
func TestMayAnnounceNeedsADestination(t *testing.T) {
	d := testDoor()
	if !d.MayAnnounce() {
		t.Error("a level-3 door with an area may not announce")
	}
	d.AnnounceArea = "  "
	if d.MayAnnounce() {
		t.Error("a door with no area may announce")
	}
	d = testDoor()
	d.APILevel = APIState
	if d.MayAnnounce() {
		t.Error("a level-2 door may announce")
	}
}

// ---------------------------------------------------------------------------
// Level 2 — state
// ---------------------------------------------------------------------------

func TestDoorStateRoundTrips(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}

	// A key that has never been written is not an error: a door asking for a
	// saved game on a player's first run is the ordinary case.
	if _, ok, err := s.DoorStateGet(ctx, d.Name, ScopeUser, "alice", "save"); err != nil || ok {
		t.Errorf("first read returned ok=%v err=%v, want false and no error", ok, err)
	}

	if err := s.DoorStateSet(ctx, d.Name, ScopeUser, "alice", "save", "sector=42", d.StateQuota); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok, err := s.DoorStateGet(ctx, d.Name, ScopeUser, "alice", "save")
	if err != nil || !ok || v != "sector=42" {
		t.Errorf("read back %q ok=%v err=%v", v, ok, err)
	}

	keys, err := s.DoorStateKeys(ctx, d.Name, ScopeUser, "alice")
	if err != nil || len(keys) != 1 || keys[0] != "save" {
		t.Errorf("keys are %v (err %v)", keys, err)
	}

	if err := s.DoorStateDelete(ctx, d.Name, ScopeUser, "alice", "save"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deleting what is not there succeeds, so tidying up needs no lookup first.
	if err := s.DoorStateDelete(ctx, d.Name, ScopeUser, "alice", "save"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

// §9.1.1: no cross-door or cross-user reach. This is the whole security
// property of level 2, so it is asserted from every direction.
func TestDoorStateDoesNotReachAcrossDoorsOrUsers(t *testing.T) {
	s, ctx := testStore(t)

	tw := testDoor()
	lord := testDoor()
	lord.Name = "lord"
	for _, d := range []Door{tw, lord} {
		if err := s.PutDoor(ctx, d, "sysop"); err != nil {
			t.Fatal(err)
		}
	}

	set := func(door, scope, owner, key, value string) {
		t.Helper()
		if err := s.DoorStateSet(ctx, door, scope, owner, key, value, 0); err != nil {
			t.Fatalf("set %s/%s/%s: %v", door, scope, owner, err)
		}
	}
	set(tw.Name, ScopeUser, "alice", "k", "alice-in-tw")
	set(tw.Name, ScopeUser, "bob", "k", "bob-in-tw")
	set(lord.Name, ScopeUser, "alice", "k", "alice-in-lord")
	set(tw.Name, ScopeGlobal, "", "k", "tw-global")

	for _, tc := range []struct{ door, scope, owner, want string }{
		{tw.Name, ScopeUser, "alice", "alice-in-tw"},
		{tw.Name, ScopeUser, "bob", "bob-in-tw"},
		{lord.Name, ScopeUser, "alice", "alice-in-lord"},
		{tw.Name, ScopeGlobal, "", "tw-global"},
	} {
		got, ok, err := s.DoorStateGet(ctx, tc.door, tc.scope, tc.owner, "k")
		if err != nil || !ok {
			t.Fatalf("%s/%s/%s: ok=%v err=%v", tc.door, tc.scope, tc.owner, ok, err)
		}
		if got != tc.want {
			t.Errorf("%s/%s/%s holds %q, want %q", tc.door, tc.scope, tc.owner, got, tc.want)
		}
	}

	// And a door with no state of its own sees nothing, rather than the other
	// door's.
	if _, ok, _ := s.DoorStateGet(ctx, lord.Name, ScopeGlobal, "", "k"); ok {
		t.Error("lord can read tradewars' global state")
	}
}

func TestDoorStateRejectsBadScopes(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ scope, owner, want string }{
		{ScopeGlobal, "alice", "no owner"},
		{ScopeUser, "", "needs a nick"},
		{"everyone", "", "unknown door state scope"},
	}
	for _, tc := range cases {
		err := s.DoorStateSet(ctx, d.Name, tc.scope, tc.owner, "k", "v", 0)
		if err == nil {
			t.Errorf("accepted scope=%q owner=%q", tc.scope, tc.owner)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("error %q does not mention %q", err, tc.want)
		}
	}
}

func TestDoorStateQuota(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	d.StateQuota = 64
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}

	fill := strings.Repeat("x", 50)
	if err := s.DoorStateSet(ctx, d.Name, ScopeUser, "alice", "a", fill, d.StateQuota); err != nil {
		t.Fatalf("first write: %v", err)
	}
	err := s.DoorStateSet(ctx, d.Name, ScopeUser, "alice", "b", fill, d.StateQuota)
	if !errors.Is(err, ErrStateQuota) {
		t.Errorf("second write returned %v, want %v", err, ErrStateQuota)
	}

	// The quota is over the whole door, not per user: another player cannot
	// start a fresh allowance.
	err = s.DoorStateSet(ctx, d.Name, ScopeUser, "bob", "a", fill, d.StateQuota)
	if !errors.Is(err, ErrStateQuota) {
		t.Errorf("a second user's write returned %v, want %v", err, ErrStateQuota)
	}

	// Overwriting an existing key charges only the difference, or a door at its
	// limit could never save again — which would make the quota a one-way door
	// rather than a bound.
	if err := s.DoorStateSet(ctx, d.Name, ScopeUser, "alice", "a", "small", d.StateQuota); err != nil {
		t.Errorf("shrinking an existing key: %v", err)
	}
	if err := s.DoorStateSet(ctx, d.Name, ScopeUser, "bob", "a", "ok", d.StateQuota); err != nil {
		t.Errorf("writing after room was freed: %v", err)
	}
}

// Two of a door's sessions writing at once must not each see room and both
// take it. A quota that only holds when nothing is happening is not a quota.
func TestDoorStateQuotaHoldsUnderConcurrency(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	d.StateQuota = 200
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}

	const writers = 12
	value := strings.Repeat("y", 40) // 12 * ~41 bytes is well over 200

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.DoorStateSet(ctx, d.Name, ScopeUser,
				string(rune('a'+i)), "k", value, d.StateQuota)
		}()
	}
	wg.Wait()

	used, err := s.DoorStateUsed(ctx, d.Name)
	if err != nil {
		t.Fatal(err)
	}
	if used > d.StateQuota {
		t.Errorf("the door is using %d bytes against a quota of %d", used, d.StateQuota)
	}
}

func TestDoorStateRejectsOversizedEntries(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}

	longKey := strings.Repeat("k", MaxDoorStateKeyLen+1)
	if err := s.DoorStateSet(ctx, d.Name, ScopeGlobal, "", longKey, "v", 0); err == nil {
		t.Error("accepted an oversized key")
	}
	longValue := strings.Repeat("v", MaxDoorStateValueLen+1)
	if err := s.DoorStateSet(ctx, d.Name, ScopeGlobal, "", "k", longValue, 0); err == nil {
		t.Error("accepted an oversized value")
	}
}

// Deleting a door deletes what it kept: a reinstall is a new installation and
// should not inherit the old one's saved games.
func TestDeletingADoorDeletesItsState(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}
	if err := s.DoorStateSet(ctx, d.Name, ScopeUser, "alice", "save", "v", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDoor(ctx, d.Name, "sysop"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.DoorStateGet(ctx, d.Name, ScopeUser, "alice", "save"); err != nil || ok {
		t.Errorf("a reinstalled door inherited state: ok=%v err=%v", ok, err)
	}
}

// ---------------------------------------------------------------------------
// Level 4 — the one-time notice
// ---------------------------------------------------------------------------

// §9.1.1: the user is told the first time a door acts as them. Once.
func TestDoorNoticeIsShownExactlyOnce(t *testing.T) {
	s, ctx := testStore(t)

	first, err := s.DoorNoticeNeeded(ctx, "tradewars", "alice")
	if err != nil || !first {
		t.Fatalf("first call returned %v (err %v), want true", first, err)
	}
	again, err := s.DoorNoticeNeeded(ctx, "tradewars", "alice")
	if err != nil || again {
		t.Errorf("second call returned %v (err %v), want false", again, err)
	}

	// Per door and per user, not per either alone.
	if other, _ := s.DoorNoticeNeeded(ctx, "lord", "alice"); !other {
		t.Error("a different door reused alice's notice")
	}
	if other, _ := s.DoorNoticeNeeded(ctx, "tradewars", "bob"); !other {
		t.Error("a different user reused the notice")
	}
}

// Two sessions of the same door acting as the same user at once must show the
// notice once — not twice, and not never.
func TestDoorNoticeIsShownOnceUnderConcurrency(t *testing.T) {
	s, ctx := testStore(t)

	const callers = 16
	var (
		mu    sync.Mutex
		shown int
		wg    sync.WaitGroup
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			need, err := s.DoorNoticeNeeded(ctx, "tradewars", "alice")
			if err != nil {
				return
			}
			if need {
				mu.Lock()
				shown++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if shown != 1 {
		t.Errorf("the notice would have been shown %d times, want 1", shown)
	}
}

// A door that is removed and reinstalled under the same name is, to the user,
// the same door they were already told about. Re-showing the notice would say
// something untrue about what is new — the opposite call from door_state, and
// for the opposite reason.
func TestDoorNoticeSurvivesAReinstall(t *testing.T) {
	s, ctx := testStore(t)
	d := testDoor()
	d.APILevel = APIActAsUser
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}
	if need, _ := s.DoorNoticeNeeded(ctx, d.Name, "alice"); !need {
		t.Fatal("the first notice was not needed")
	}
	if err := s.DeleteDoor(ctx, d.Name, "sysop"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}
	if need, _ := s.DoorNoticeNeeded(ctx, d.Name, "alice"); need {
		t.Error("a reinstalled door asked to tell alice again")
	}
}

func countAudit(t *testing.T, s *Store, action string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
