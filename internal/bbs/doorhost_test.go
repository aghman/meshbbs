package bbs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/store"
)

// The fake host in the door package proves the API's logic. This proves the
// same interface against the real store and the real Post path — which is where
// the two things that cannot be faked live: the author field's sixteen bytes,
// and the capability gate inside Post that §9.1.1's intersection rule rests on.

func doorFixture(t *testing.T) (*DoorHost, *store.Store, context.Context) {
	t.Helper()
	svc, st, ctx := testService(t)
	return svc.Doors(), st, ctx
}

func TestDoorHostAnnouncesUnderAMarkedAuthor(t *testing.T) {
	h, st, ctx := doorFixture(t)

	if _, err := st.CreateArea(ctx, "games", "games chatter", false); err != nil {
		t.Fatal(err)
	}
	id, err := h.Announce(ctx, "tradewars", "games", "New champion", "alice won")
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if id == "" {
		t.Error("announce returned no record id")
	}

	posts, err := st.ListPosts(ctx, "games", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("posts: %d", len(posts))
	}
	author := posts[0].Author
	if author != store.DoorAuthor("tradewars") {
		t.Errorf("author is %q, want %q", author, store.DoorAuthor("tradewars"))
	}
	// The whole point of the marker: this can never be an account.
	if err := store.ValidateNick(author); err == nil {
		t.Errorf("%q is a valid nick, so a door is indistinguishable from a person", author)
	}
	name, ok := store.DoorNameFromAuthor(author)
	if !ok || name != "tradewars" {
		t.Errorf("the author does not read back as a door: %q ok=%v", name, ok)
	}
}

// A door name that cannot fit the author field is a configuration error the
// sysop sees when saving, not a failure the first time the door announces.
func TestDoorThatAnnouncesNeedsAShortEnoughName(t *testing.T) {
	_, st, ctx := doorFixture(t)

	d := store.Door{
		Name:         strings.Repeat("d", store.MaxAnnounceDoorNameLen+1),
		Path:         "/opt/doors/x",
		Cwd:          "/opt/doors",
		DropfileType: store.DropfileNone,
		WallClock:    time.Hour,
		APILevel:     store.APIAnnounce,
		AnnounceArea: "games",
	}
	err := st.PutDoor(ctx, d, "sysop")
	if err == nil {
		t.Fatal("a door too long to post under was accepted")
	}
	if !strings.Contains(err.Error(), "author field") {
		t.Errorf("the error does not explain the limit: %v", err)
	}

	// The same name is fine for a door that does not announce, because nothing
	// will ever try to write it into a post.
	d.AnnounceArea = ""
	d.APILevel = store.APIState
	if err := st.PutDoor(ctx, d, "sysop"); err != nil {
		t.Errorf("a long-named door that does not announce was refused: %v", err)
	}
}

// §9.1.1: capabilities intersect, never escalate. This is the case that matters
// — the door has level 4, the user does not have post_federated, and the post
// must not happen.
func TestDoorActingAsAUserCannotFederateForThem(t *testing.T) {
	h, st, ctx := doorFixture(t)

	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: "alice", CanLogin: true,
		// Deliberately the default set, which excludes post_federated ([N7]).
		Capabilities: store.DefaultCapabilities,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArea(ctx, "netmail", "federated", true); err != nil {
		t.Fatal(err)
	}

	_, err := h.PostAs(ctx, "alice", "netmail", "hi", "from a door")
	if err == nil {
		t.Fatal("a door posted to a federated area for a user who may not")
	}
	if !strings.Contains(err.Error(), store.CapPostFederated) {
		t.Errorf("the refusal does not name the missing capability: %v", err)
	}

	// And once the user has it, the door inherits exactly that and no more.
	if err := st.GrantCapability(ctx, "alice", store.CapPostFederated, "sysop"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostAs(ctx, "alice", "netmail", "hi", "from a door"); err != nil {
		t.Errorf("the door could not post for a user who may: %v", err)
	}
}

func TestDoorHostStateGoesThroughTheStore(t *testing.T) {
	h, st, ctx := doorFixture(t)

	d := store.Door{
		Name: "tradewars", Path: "/opt/doors/tw", Cwd: "/opt/doors",
		DropfileType: store.DropfileNone, WallClock: time.Hour,
		APILevel: store.APIState, StateQuota: 1024,
	}
	if err := st.PutDoor(ctx, d, "sysop"); err != nil {
		t.Fatal(err)
	}

	if err := h.StateSet(ctx, d.Name, store.ScopeUser, "alice", "save", "sector=42", 1024); err != nil {
		t.Fatalf("set: %v", err)
	}
	v, ok, err := h.StateGet(ctx, d.Name, store.ScopeUser, "alice", "save")
	if err != nil || !ok || v != "sector=42" {
		t.Errorf("get returned %q ok=%v err=%v", v, ok, err)
	}
	keys, err := h.StateKeys(ctx, d.Name, store.ScopeUser, "alice")
	if err != nil || len(keys) != 1 {
		t.Errorf("keys are %v (err %v)", keys, err)
	}
	if err := h.StateDelete(ctx, d.Name, store.ScopeUser, "alice", "save"); err != nil {
		t.Errorf("delete: %v", err)
	}
}

func TestDoorHostReportsWhetherAnAreaSpendsAirtime(t *testing.T) {
	h, st, ctx := doorFixture(t)

	if _, err := st.CreateArea(ctx, "local", "local only", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateArea(ctx, "mesh", "on the mesh", true); err != nil {
		t.Fatal(err)
	}

	if fed, err := h.AreaIsFederated(ctx, "local"); err != nil || fed {
		t.Errorf("local reported federated=%v (err %v)", fed, err)
	}
	if fed, err := h.AreaIsFederated(ctx, "mesh"); err != nil || !fed {
		t.Errorf("mesh reported federated=%v (err %v)", fed, err)
	}
}
