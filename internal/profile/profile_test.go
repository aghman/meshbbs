package profile

import (
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

func user(nick string, unlisted bool) User {
	u := User{Nick: nick, Unlisted: unlisted}
	for i := range u.DMKey {
		u.DMKey[i] = byte(i + 1)
	}
	return u
}

// The rule §6.7 exists to enforce: registering costs the network nothing.
//
// This is the whole point of the package. A node with fifty users that publishes
// eagerly spends two days of its entire mesh allocation on introductions.
func TestRegisteringAndReadingPublishNothing(t *testing.T) {
	p := New()
	for i := 0; i < 50; i++ {
		u := user("user"+string(rune('a'+i%26))+string(rune('0'+i/26)), false)
		if d := p.Consider(u, TriggerNone); d.Publish {
			t.Fatalf("%s was published merely for existing", u.Nick)
		}
	}
	emitted, suppressed := p.Stats()
	if emitted != 0 {
		t.Errorf("%d profiles published for fifty registrations; the answer is zero", emitted)
	}
	t.Logf("fifty registrations: %d published, %d suppressed", emitted, suppressed)

	// Quantify what was saved, since that number is the justification.
	saved := 0
	for i := 0; i < 50; i++ {
		saved += record.ProfileSize("userxy")
	}
	t.Logf("airtime avoided: %d bytes of directory data that eager publication would have sent", saved)
	if saved < 4000 {
		t.Errorf("the saving is only %d bytes; §6.7's table says ~5 KB for fifty users", saved)
	}
}

// A profile goes out on first activity that requires the network to know the
// user, and exactly once.
func TestPublishesOnceOnFirstFederatedActivity(t *testing.T) {
	for _, trigger := range []Trigger{TriggerFederatedPost, TriggerOffNodeDM, TriggerDirectoryRequest} {
		t.Run(trigger.String(), func(t *testing.T) {
			p := New()
			u := user("alice", false)

			d := p.Consider(u, trigger)
			if !d.Publish {
				t.Fatalf("first %s did not publish: %s", trigger, d.Reason)
			}
			t.Logf("published %d bytes: %s", d.Bytes, d.Reason)

			// Everything after the first time is free.
			for i := 0; i < 20; i++ {
				if again := p.Consider(u, trigger); again.Publish {
					t.Fatalf("repeat activity %d republished the profile: %s", i, again.Reason)
				}
			}
			emitted, _ := p.Stats()
			if emitted != 1 {
				t.Errorf("21 activities produced %d publications, want 1", emitted)
			}
		})
	}
}

// [N9]: listed by default, with a real opt-out. An unlisted user who never
// published stays invisible — and can still send DMs, because a reply travels
// on what the sender's DM carries rather than on a directory lookup.
func TestUnlistedUsersStayInvisible(t *testing.T) {
	p := New()
	u := user("ghost", true)

	for _, trigger := range []Trigger{TriggerFederatedPost, TriggerOffNodeDM, TriggerDirectoryRequest} {
		if d := p.Consider(u, trigger); d.Publish {
			t.Errorf("an unlisted user was published on %s: %s", trigger, d.Reason)
		}
	}
	if p.Published("ghost") {
		t.Error("an unlisted user appears as published")
	}
}

// The awkward case the design has to handle: a user who WAS listed opts out.
// Peers already hold the old profile, so silence would leave them listed
// forever — but a tombstone would destroy the key material needed to receive
// replies, turning "leave the directory" into "stop receiving mail".
func TestOptingOutAfterBeingListedRepublishesWithTheFlag(t *testing.T) {
	p := New()
	listed := user("bob", false)

	if d := p.Consider(listed, TriggerFederatedPost); !d.Publish {
		t.Fatalf("setup: %s", d.Reason)
	}

	nowUnlisted := listed
	nowUnlisted.Unlisted = true
	d := p.Consider(nowUnlisted, TriggerFederatedPost)
	if !d.Publish {
		t.Fatal("opting out after being listed published nothing; peers would show them forever")
	}
	if d.Trigger != TriggerFlagsChanged {
		t.Errorf("trigger is %s, want %s", d.Trigger, TriggerFlagsChanged)
	}
	t.Logf("republished: %s", d.Reason)

	// And it settles: no repeated republication.
	if again := p.Consider(nowUnlisted, TriggerFederatedPost); again.Publish {
		t.Error("the opt-out republished a second time")
	}
}

// §6.7: deletion tombstones a published profile, and a never-published account
// needs nothing. Emitting a tombstone for an account the network never heard of
// would announce its existence at the moment of deletion.
func TestDeletionOnlyTombstonesWhatWasPublished(t *testing.T) {
	p := New()

	if p.Forget("never-published") {
		t.Error("a local-only account demanded a tombstone, announcing a user the network never knew")
	}

	u := user("carol", false)
	p.Consider(u, TriggerOffNodeDM)
	if !p.Forget("carol") {
		t.Error("deleting a published user produced no tombstone; peers would keep them forever")
	}
	if p.Published("carol") {
		t.Error("the user is still marked as published after deletion")
	}
}

// A user with no DM key cannot be published: the profile's whole purpose is
// carrying that key, and one without it advertises an unreachable address.
func TestUserWithoutADMKeyIsNotPublished(t *testing.T) {
	p := New()
	u := User{Nick: "keyless"} // zero DMKey
	d := p.Consider(u, TriggerFederatedPost)
	if d.Publish {
		t.Error("published a profile with no DM key")
	}
	if d.Reason != ErrNoDMKey.Error() {
		t.Errorf("reason is %q, want the missing-key error", d.Reason)
	}
}

// ---------------------------------------------------------------------------
// Record layer
// ---------------------------------------------------------------------------

func nodeKey(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestProfileRecordRoundTripAndVerify(t *testing.T) {
	k := nodeKey(t, 1)
	u := user("alice", false)
	body := record.ProfileBody{Nick: u.Nick, DMKey: u.DMKey}

	rec, err := record.NewProfileRecord(k, 1, 1_700_000_000, body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := record.VerifyProfileRecord(rec, k.Public)
	if err != nil {
		t.Fatalf("a freshly built profile failed verification: %v", err)
	}
	if got.Nick != body.Nick || got.DMKey != body.DMKey || got.Unlisted() {
		t.Errorf("round trip changed the body: %+v", got)
	}

	// Wrong key must fail rather than quietly pass.
	other := nodeKey(t, 2)
	if _, err := record.VerifyProfileRecord(rec, other.Public); err == nil {
		t.Error("verified a profile against an unrelated node key")
	}

	// §6.7 budgets ~100 B per profile; the arithmetic behind lazy publication
	// depends on that being roughly true.
	size := record.ProfileSize(u.Nick)
	t.Logf("profile on the wire: %d bytes uncompressed", size)
	if size > 160 {
		t.Errorf("a profile is %d bytes, well over the ~100 B the §6.7 table assumes", size)
	}
}

func TestProfileBodyRejectsMalformed(t *testing.T) {
	good, err := record.MarshalProfileBody(record.ProfileBody{
		Nick: "alice", DMKey: user("alice", false).DMKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("reserved flag bits", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[len(bad)-1] = 0x80 // a bit we do not define
		if _, err := record.UnmarshalProfileBody(bad); err == nil {
			t.Error("accepted a reserved flag bit; that is a second wire form for one profile")
		}
	})

	t.Run("all-zero DM key", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		for i := 1 + len("alice"); i < 1+len("alice")+record.X25519KeyLen; i++ {
			bad[i] = 0
		}
		if _, err := record.UnmarshalProfileBody(bad); err == nil {
			t.Error("accepted an all-zero DM key, which makes the shared secret degenerate")
		}
	})

	t.Run("control characters in the nick", func(t *testing.T) {
		// Nicks are rendered into a terminal, so an unfiltered one is an ANSI
		// injection arriving from a stranger's instance.
		if _, err := record.MarshalProfileBody(record.ProfileBody{
			Nick: "evil\x1b[2Jname", DMKey: user("x", false).DMKey,
		}); err == nil {
			t.Error("accepted a nick containing an escape sequence")
		}
	})

	t.Run("length disagreement", func(t *testing.T) {
		for _, bad := range [][]byte{nil, good[:len(good)-1], append(append([]byte(nil), good...), 0)} {
			if _, err := record.UnmarshalProfileBody(bad); err == nil {
				t.Errorf("accepted a %d-byte body", len(bad))
			}
		}
	})

	t.Run("empty nick", func(t *testing.T) {
		if _, err := record.UnmarshalProfileBody([]byte{0}); err == nil {
			t.Error("accepted an empty nick")
		}
	})
}

// ---------------------------------------------------------------------------
// Directory
// ---------------------------------------------------------------------------

func TestDirectoryQualifiesNicksByNode(t *testing.T) {
	a, b := nodeKey(t, 1), nodeKey(t, 2)
	d := NewDirectory()

	// The same nick on two instances is legitimate: nicks are unique within a
	// node only, because global uniqueness would need the registry this design
	// refuses to have (§6.1.4).
	for _, k := range []identity.NodeKey{a, b} {
		body := record.ProfileBody{Nick: "admin", DMKey: user("admin", false).DMKey}
		rec, err := record.NewProfileRecord(k, 1, 1, body)
		if err != nil {
			t.Fatal(err)
		}
		d.Add(rec, body)
	}

	if d.Len() != 2 {
		t.Fatalf("two nodes' admins collapsed into %d entry", d.Len())
	}
	for _, k := range []identity.NodeKey{a, b} {
		q := "admin@" + k.ID().Compact()
		if _, ok := d.Lookup(q); !ok {
			t.Errorf("%s is not in the directory", q)
		}
	}

	found := d.Search("ADM") // case-insensitive, because nicks are for humans
	if len(found) != 2 {
		t.Errorf("search found %d of 2", len(found))
	}
}

// An unlisted profile that reaches us must be honoured, not merely not
// requested. A peer can always choose to send one.
func TestDirectoryHonoursTheUnlistedFlag(t *testing.T) {
	k := nodeKey(t, 1)
	d := NewDirectory()
	dmKey := user("dave", false).DMKey

	listed := record.ProfileBody{Nick: "dave", DMKey: dmKey}
	rec, err := record.NewProfileRecord(k, 1, 1, listed)
	if err != nil {
		t.Fatal(err)
	}
	d.Add(rec, listed)
	if d.Len() != 1 {
		t.Fatal("setup failed")
	}

	// The same user, now opted out.
	unlisted := record.ProfileBody{Nick: "dave", DMKey: dmKey, Flags: record.FlagUnlisted}
	rec2, err := record.NewProfileRecord(k, 2, 2, unlisted)
	if err != nil {
		t.Fatal(err)
	}
	d.Add(rec2, unlisted)

	if d.Len() != 0 {
		t.Error("an unlisted profile remained in the directory")
	}
	if _, ok := d.Lookup("dave@" + k.ID().Compact()); ok {
		t.Error("an unlisted user is still findable")
	}
}

func FuzzUnmarshalProfileBody(f *testing.F) {
	good, _ := record.MarshalProfileBody(record.ProfileBody{
		Nick: "alice", DMKey: user("alice", false).DMKey,
	})
	f.Add(good)
	f.Add([]byte{0})
	f.Add([]byte{})

	// §12.5: profiles arrive from anyone holding the channel PSK, and the nick
	// inside is rendered into a terminal.
	f.Fuzz(func(t *testing.T, data []byte) {
		body, err := record.UnmarshalProfileBody(data)
		if err != nil {
			return
		}
		again, err := record.MarshalProfileBody(body)
		if err != nil {
			t.Fatalf("a parsed profile failed to re-encode: %v", err)
		}
		if string(again) != string(data) {
			t.Fatalf("profile encoding is not canonical:\n input % x\nre-enc % x", data, again)
		}
		for _, r := range body.Nick {
			if r < 0x20 || r == 0x7f {
				t.Fatalf("a control character survived into a nick: %q", body.Nick)
			}
		}
	})
}
