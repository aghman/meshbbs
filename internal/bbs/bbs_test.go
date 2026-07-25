package bbs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

func testService(t *testing.T) (*Service, *store.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	st, err := store.OpenMemory(ctx, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	key, err := identity.GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutNode(ctx, store.Node{ID: key.ID(), PublicKey: key.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	svc := New(st, key, clk)
	if err := svc.SeedDefaultAreas(ctx); err != nil {
		t.Fatal(err)
	}
	return svc, st, ctx
}

func mkUser(t *testing.T, svc *Service, st *store.Store, ctx context.Context, nick, passphrase string, caps ...string) {
	t.Helper()
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: nick, CanLogin: true, Capabilities: append(store.DefaultCapabilities, caps...),
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureDMKey(ctx, nick, passphrase); err != nil {
		t.Fatal(err)
	}
}

func TestPostAndRead(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")

	if _, err := svc.Post(ctx, "austin", "general", "Hello", "First post on the mesh."); err != nil {
		t.Fatal(err)
	}
	posts, err := st.ListPosts(ctx, "general", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	if posts[0].Author != "austin" || posts[0].Subject != "Hello" {
		t.Fatalf("post metadata wrong: %+v", posts[0])
	}
	if posts[0].Body != "First post on the mesh." {
		t.Fatalf("body is %q", posts[0].Body)
	}
}

// [N7]: local areas are open to everyone; federated areas require the
// capability, because posting there spends the network's shared airtime.
func TestFederatedAreaRequiresCapability(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "bob", "pw")

	if _, err := st.CreateArea(ctx, "mesh-wide", "Federated area", true); err != nil {
		t.Fatal(err)
	}

	// Local posting works with no special grant.
	if _, err := svc.Post(ctx, "bob", "general", "hi", "local post"); err != nil {
		t.Fatalf("local post refused: %v", err)
	}

	_, err := svc.Post(ctx, "bob", "mesh-wide", "hi", "federated post")
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("expected ErrNotPermitted, got %v", err)
	}
	// The message must name the remedy — the user cannot grant it themselves.
	if !strings.Contains(err.Error(), store.CapPostFederated) {
		t.Errorf("error does not name the capability needed: %v", err)
	}

	if err := st.GrantCapability(ctx, "bob", store.CapPostFederated, "sysop"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Post(ctx, "bob", "mesh-wide", "hi", "federated post"); err != nil {
		t.Fatalf("post refused after the grant: %v", err)
	}
}

// Default areas must be local-only: a fresh BBS must not start spending mesh
// airtime because of a default (§6.3).
func TestDefaultAreasAreLocalOnly(t *testing.T) {
	_, st, ctx := testService(t)
	areas, err := st.ListAreas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(areas) == 0 {
		t.Fatal("no default areas were created")
	}
	for _, a := range areas {
		if a.Federated {
			t.Errorf("default area %q is federated; sysops must opt in", a.Name)
		}
	}
}

func TestDMRoundTrip(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "austin-pw")
	mkUser(t, svc, st, ctx, "bob", "bob-pw")

	if _, err := svc.SendDM(ctx, "austin", "bob", "Antenna", "Want to split a J-pole order?"); err != nil {
		t.Fatal(err)
	}

	inbox, err := st.Inbox(ctx, "bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 1 {
		t.Fatalf("bob has %d messages, want 1", len(inbox))
	}
	if inbox[0].Sender != "austin" {
		t.Fatalf("routing metadata wrong: %+v", inbox[0])
	}
	// The subject must NOT be readable from the index: [D7] authorised
	// exposing who-talks-to-whom, not message content (§8.1).
	if inbox[0].Subject != "" {
		t.Fatalf("subject leaked into the cleartext index: %q", inbox[0].Subject)
	}

	payload, err := svc.OpenDM(ctx, "bob", "bob-pw", inbox[0].SealedBytes)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Text != "Want to split a J-pole order?" {
		t.Fatalf("decrypted %q", payload.Text)
	}
	// The subject is sealed with the body, so it only appears after decryption.
	if payload.Subject != "Antenna" {
		t.Fatalf("subject is %q, want Antenna", payload.Subject)
	}
}

// §8.2: the sysop holds ciphertext. Without the recipient's passphrase the
// server cannot read the message, even with full database access.
func TestDMBodyIsUnreadableWithoutThePassphrase(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "austin-pw")
	mkUser(t, svc, st, ctx, "bob", "bob-pw")

	secret := "the repeater password is swordfish"
	if _, err := svc.SendDM(ctx, "austin", "bob", "shh", secret); err != nil {
		t.Fatal(err)
	}

	inbox, err := st.Inbox(ctx, "bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inbox[0].SealedBytes), secret) {
		t.Fatal("the stored DM contains plaintext")
	}
	// The whole record body, as stored, must not contain the plaintext either.
	rec, err := st.GetRecord(ctx, inbox[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rec.Body), secret) {
		t.Fatal("the record body contains plaintext")
	}

	if _, err := svc.OpenDM(ctx, "bob", "wrong-passphrase", inbox[0].SealedBytes); err == nil {
		t.Fatal("a wrong passphrase decrypted the message")
	}
}

// [D7]: addressing is deliberately in the clear, so the server can route and
// bounce without decrypting. Assert that it genuinely is readable.
func TestDMRoutingMetadataIsReadable(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")
	mkUser(t, svc, st, ctx, "bob", "pw")

	if _, err := svc.SendDM(ctx, "austin", "bob", "subject line", "body"); err != nil {
		t.Fatal(err)
	}
	n, err := st.UnreadCount(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("unread count is %d, want 1", n)
	}
}

func TestSendDMToUnknownUser(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")

	_, err := svc.SendDM(ctx, "austin", "nobody", "hi", "text")
	if !errors.Is(err, ErrNoRecipient) {
		t.Fatalf("expected ErrNoRecipient, got %v", err)
	}
}

// §6.1.4.1: aliases resolve at compose time. An alias must never end up in the
// stored record — the recipient's alias table is a different table.
func TestAliasResolvesAtComposeTimeAndNeverReachesTheRecord(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")

	other, err := identity.GenerateNodeKey(rng.TestSecret(99))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlias(ctx, "pnw", other.ID()); err != nil {
		t.Fatal(err)
	}

	nick, node, err := svc.ResolveRecipient(ctx, "joe@pnw")
	if err != nil {
		t.Fatal(err)
	}
	if nick != "joe" {
		t.Fatalf("nick is %q", nick)
	}
	if node != other.ID() {
		t.Fatal("alias resolved to the wrong node")
	}

	// A bare nick means "on this BBS".
	_, localNode, err := svc.ResolveRecipient(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if localNode != svc.NodeID() {
		t.Fatal("a bare nick did not resolve to the local node")
	}
}

func TestUnknownAliasExplainsTheRemedy(t *testing.T) {
	svc, _, ctx := testService(t)
	_, _, err := svc.ResolveRecipient(ctx, "joe@nowhere")
	if err == nil {
		t.Fatal("expected an error for an unknown alias")
	}
	// Users cannot create aliases themselves ([N1]), so the error must say to
	// ask the sysop rather than implying a self-service fix.
	if !strings.Contains(err.Error(), "sysop") {
		t.Errorf("error does not point at the sysop: %v", err)
	}
}

func TestEmptyPostRejected(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")
	if _, err := svc.Post(ctx, "austin", "general", "subject", "   \n  "); err == nil {
		t.Fatal("an empty post was accepted")
	}
}

// Every post and DM is a signed record, so Phase 2 replicates them without
// reworking this layer.
func TestPostsAndDMsAreSignedRecords(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")
	mkUser(t, svc, st, ctx, "bob", "pw")

	postID, err := svc.Post(ctx, "austin", "general", "s", "body")
	if err != nil {
		t.Fatal(err)
	}
	dmID, err := svc.SendDM(ctx, "austin", "bob", "s", "body")
	if err != nil {
		t.Fatal(err)
	}

	self, err := st.SelfNode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []record.ID{postID, dmID} {
		rec, err := st.GetRecord(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := rec.Verify(self.PublicKey); err != nil {
			t.Fatalf("record %s does not verify: %v", rec.ID(), err)
		}
	}
}

// [D7] authorised exposing who-talks-to-whom, NOT message content. §8.1 says
// to treat the mesh as public, so a cleartext subject would put the gist of
// every private message in front of every sysop on the channel.
func TestSubjectIsEncryptedNotJustTheBody(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")
	mkUser(t, svc, st, ctx, "bob", "bob-pw")

	subject := "Repeater access codes"
	id, err := svc.SendDM(ctx, "austin", "bob", subject, "body text")
	if err != nil {
		t.Fatal(err)
	}

	rec, err := st.GetRecord(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rec.Body), subject) {
		t.Fatal("the subject appears in cleartext in the stored record")
	}
	if strings.Contains(string(rec.SignedBytes()), subject) {
		t.Fatal("the subject appears in cleartext in the signed bytes (it would go on the wire)")
	}

	// Sender and recipient DO remain readable — that is what [D7] authorised,
	// and it is what makes immediate bounces and spam filtering possible.
	body, err := store.UnmarshalDMBody(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body.Sender != "austin" || body.Recipient != "bob" {
		t.Fatalf("routing metadata should stay readable, got %+v", body)
	}

	// And the recipient still gets the subject back.
	payload, err := svc.OpenDM(ctx, "bob", "bob-pw", body.Sealed)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Subject != subject {
		t.Fatalf("subject did not survive the round trip: %q", payload.Subject)
	}
}
