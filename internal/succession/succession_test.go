package succession

import (
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
)

var epoch = time.Unix(1_700_000_000, 0).UTC()

func key(t *testing.T, seed uint64) identity.NodeKey {
	t.Helper()
	k, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// fixture builds a registry with the given keys already learned.
func fixture(t *testing.T, keys ...identity.NodeKey) (*Registry, *clock.Virtual) {
	t.Helper()
	clk := clock.NewVirtual(epoch)
	r := New(clk)
	for _, k := range keys {
		r.LearnKey(k.ID(), k.Public)
	}
	return r, clk
}

func succeed(t *testing.T, old identity.NodeKey, new identity.NodeKey, seq uint64, effective time.Time) *record.Record {
	t.Helper()
	rec, err := record.NewSuccessionRecord(old, seq, uint32(effective.Unix()),
		new.ID(), new.Public, uint32(effective.Unix()))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// The ordinary case: a planned rotation is followed without asking.
func TestPlannedRotationIsFollowed(t *testing.T) {
	a, b := key(t, 1), key(t, 2)
	r, _ := fixture(t, a)

	e := r.Offer(succeed(t, a, b, 1, epoch))
	if e.Kind != Followed {
		t.Fatalf("a valid succession was %s: %s", e.Kind, e.Reason)
	}
	if got := r.Resolve(a.ID()); got != b.ID() {
		t.Errorf("resolving %s gave %s, want %s", a.ID().Compact(), got.Compact(), b.ID().Compact())
	}
	// An identity nobody succeeded resolves to itself.
	if got := r.Resolve(b.ID()); got != b.ID() {
		t.Errorf("the successor resolved to %s rather than itself", got.Compact())
	}
	if succ, ok := r.Superseded(a.ID()); !ok || succ != b.ID() {
		t.Error("the predecessor is not reported as superseded")
	}
}

// GUARDRAIL 1: following is never silent. Auto-follow removes the blocking
// prompt, not the disclosure — a sysop who did not authorise a rotation must be
// able to see that one happened.
func TestFollowingIsAlwaysDisclosed(t *testing.T) {
	a, b := key(t, 1), key(t, 2)
	r, _ := fixture(t, a)
	r.Offer(succeed(t, a, b, 1, epoch))

	events := r.Events()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(events))
	}
	msg := events[0].Message()
	t.Logf("alert: %s", msg)

	// §6.1.4.2: both renderings, because a sysop checking an identity handover
	// against something written on paper needs the form they actually have.
	for _, want := range []string{
		a.ID().Compact(), a.ID().Words(),
		b.ID().Compact(), b.ID().Words(),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the alert omits %q; it must name both IDs in both renderings", want)
		}
	}
	if !strings.Contains(strings.ToLower(msg), "compromise") {
		t.Error("the alert does not tell the sysop what an unexpected succession would mean")
	}
}

// GUARDRAIL 2: the old ID is tombstoned at `effective`, but not before.
//
// The asymmetry is the point. Rejecting everything from the old ID the moment a
// succession lands would discard the tail of the predecessor's log, which on a
// mesh is still in flight for minutes or hours after the succession arrives.
func TestOldIDIsTombstonedAtEffectiveOnly(t *testing.T) {
	a, b := key(t, 1), key(t, 2)
	r, _ := fixture(t, a)
	effective := epoch.Add(time.Hour)

	if e := r.Offer(succeed(t, a, b, 1, effective)); e.Kind != Followed {
		t.Fatalf("setup failed: %s", e.Reason)
	}

	cases := []struct {
		name   string
		ts     time.Time
		accept bool
	}{
		{"well before the handover", effective.Add(-24 * time.Hour), true},
		{"just before", effective.Add(-time.Second), true},
		{"exactly at the handover", effective, true},
		{"one second after", effective.Add(time.Second), false},
		{"long after", effective.Add(30 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		ok, why := r.AcceptsRecord(a.ID(), uint32(tc.ts.Unix()))
		if ok != tc.accept {
			t.Errorf("%s: accepted=%v, want %v (%s)", tc.name, ok, tc.accept, why)
		}
	}

	// The successor is unaffected.
	if ok, _ := r.AcceptsRecord(b.ID(), uint32(effective.Add(time.Hour).Unix())); !ok {
		t.Error("the successor's own records were rejected")
	}
	// A node that never rotated is unaffected.
	c := key(t, 3)
	if ok, _ := r.AcceptsRecord(c.ID(), uint32(effective.Add(time.Hour).Unix())); !ok {
		t.Error("an unrelated node's records were rejected")
	}
}

// GUARDRAIL 3: one succession per predecessor.
//
// This is what limits the damage from a stolen key. Without it an attacker
// could redirect a node, watch the sysop investigate, then redirect again —
// pass-the-parcel with no fixed point to reason about. With it they get exactly
// one visible, logged, permanent redirect.
func TestOneSuccessionPerPredecessor(t *testing.T) {
	a, b, c := key(t, 1), key(t, 2), key(t, 3)
	r, _ := fixture(t, a)

	if e := r.Offer(succeed(t, a, b, 1, epoch)); e.Kind != Followed {
		t.Fatalf("the first succession was refused: %s", e.Reason)
	}

	// A second, differently addressed succession from the same old key.
	e := r.Offer(succeed(t, a, c, 2, epoch))
	if e.Kind != Refused {
		t.Fatalf("a second succession from one key was %s; it must be refused", e.Kind)
	}
	t.Logf("refusal: %s", e.Message())
	if r.Resolve(a.ID()) != b.ID() {
		t.Error("the refused second succession changed where the predecessor resolves")
	}

	// Refusals are disclosed too — a refused succession from a peer you trust is
	// exactly the signal that a key has been stolen.
	events := r.Events()
	if len(events) != 2 || events[1].Kind != Refused {
		t.Fatalf("the refusal was not recorded: %+v", events)
	}
}

// Re-offering the SAME succession is routine on a flooding mesh, and must be a
// no-op rather than tripping guardrail 3.
func TestReofferingTheSameSuccessionIsIdempotent(t *testing.T) {
	a, b := key(t, 1), key(t, 2)
	r, _ := fixture(t, a)
	rec := succeed(t, a, b, 1, epoch)

	if e := r.Offer(rec); e.Kind != Followed {
		t.Fatalf("first offer: %s", e.Reason)
	}
	for i := 0; i < 5; i++ {
		if e := r.Offer(rec); e.Kind != Duplicate {
			t.Fatalf("re-offer %d was %s (%s); duplicate delivery must be a no-op",
				i, e.Kind, e.Reason)
		}
	}
	if r.Resolve(a.ID()) != b.ID() {
		t.Error("duplicate delivery disturbed the resolution")
	}
}

// GUARDRAIL 4: a sanity window on `effective`, so a succession cannot be minted
// now and stockpiled for later.
func TestEffectiveTimeMustBeWithinTheWindow(t *testing.T) {
	a, b := key(t, 1), key(t, 2)

	for _, tc := range []struct {
		name      string
		effective time.Time
		want      Kind
	}{
		{"now", epoch, Followed},
		{"tomorrow, just inside", epoch.Add(DefaultWindow - time.Hour), Followed},
		{"yesterday, just inside", epoch.Add(-DefaultWindow + time.Hour), Followed},
		{"a week out", epoch.Add(7 * 24 * time.Hour), Refused},
		{"a year out", epoch.Add(365 * 24 * time.Hour), Refused},
		{"a month ago", epoch.Add(-30 * 24 * time.Hour), Refused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := fixture(t, a)
			e := r.Offer(succeed(t, a, b, 1, tc.effective))
			if e.Kind != tc.want {
				t.Errorf("effective %s was %s, want %s (%s)",
					tc.effective.Format(time.RFC3339), e.Kind, tc.want, e.Reason)
			}
		})
	}
}

// A chain is a node that rotated more than once. Each hop is signed by the
// then-current key, so the chain is legitimate — §6.1.6 says so explicitly.
func TestChainsResolveToTheCurrentIdentity(t *testing.T) {
	a, b, c := key(t, 1), key(t, 2), key(t, 3)
	r, clk := fixture(t, a)

	if e := r.Offer(succeed(t, a, b, 1, epoch)); e.Kind != Followed {
		t.Fatalf("A->B: %s", e.Reason)
	}
	// B's key was learned FROM the succession, so B->C verifies without a
	// separate NODE record having arrived first.
	clk.Advance(time.Hour)
	if e := r.Offer(succeed(t, b, c, 1, epoch.Add(time.Hour))); e.Kind != Followed {
		t.Fatalf("B->C: %s", e.Reason)
	}

	if got := r.Resolve(a.ID()); got != c.ID() {
		t.Errorf("A resolves to %s, want C (%s)", got.Compact(), c.ID().Compact())
	}
	if got := r.Resolve(b.ID()); got != c.ID() {
		t.Errorf("B resolves to %s, want C", got.Compact())
	}

	chain := r.Chain(a.ID())
	if len(chain) != 3 || chain[0] != a.ID() || chain[1] != b.ID() || chain[2] != c.ID() {
		t.Errorf("chain is %v, want A->B->C", chain)
	}
}

// A→B→A would make Resolve walk forever. It is also never a legitimate
// rotation.
func TestCyclesAreRefused(t *testing.T) {
	a, b := key(t, 1), key(t, 2)
	r, _ := fixture(t, a, b)

	if e := r.Offer(succeed(t, a, b, 1, epoch)); e.Kind != Followed {
		t.Fatalf("A->B: %s", e.Reason)
	}
	e := r.Offer(succeed(t, b, a, 1, epoch))
	if e.Kind != Refused {
		t.Fatalf("B->A closed a cycle and was %s", e.Kind)
	}
	t.Logf("refusal: %s", e.Reason)

	// And resolution still terminates.
	done := make(chan identity.NodeID, 1)
	go func() { done <- r.Resolve(a.ID()) }()
	select {
	case got := <-done:
		if got != b.ID() {
			t.Errorf("resolved to %s, want B", got.Compact())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve did not terminate; a cycle was admitted")
	}
}

// Chains are bounded, because resolution walks them and the input is remote.
func TestChainsAreBounded(t *testing.T) {
	keys := make([]identity.NodeKey, MaxChain+4)
	for i := range keys {
		keys[i] = key(t, uint64(i+1))
	}
	r, _ := fixture(t, keys[0])

	refusedAt := -1
	for i := 0; i+1 < len(keys); i++ {
		e := r.Offer(succeed(t, keys[i], keys[i+1], 1, epoch))
		if e.Kind == Refused {
			refusedAt = i
			t.Logf("hop %d refused: %s", i, e.Reason)
			break
		}
	}
	if refusedAt < 0 {
		t.Fatalf("a chain of %d hops was accepted without limit", len(keys)-1)
	}
	if refusedAt > MaxChain {
		t.Errorf("chain grew to %d hops before refusal, limit is %d", refusedAt, MaxChain)
	}
	if got := len(r.Chain(keys[0].ID())); got > MaxChain+1 {
		t.Errorf("Chain returned %d entries, over the bound", got)
	}
}

// The security of auto-follow is entirely that the record self-certifies and is
// signed by the key it is handing away. Both halves are checked here.
func TestForgedSuccessionsAreRefused(t *testing.T) {
	a, b, imposter := key(t, 1), key(t, 2), key(t, 9)

	t.Run("signed by someone else", func(t *testing.T) {
		r, _ := fixture(t, a, imposter)
		// The imposter signs a succession for its OWN origin — it cannot forge
		// a's origin, because New refuses to sign a record whose origin is not
		// the signer. So the attack it CAN mount is redirecting itself, which is
		// legitimate. The real check is that a's identity is untouched.
		if e := r.Offer(succeed(t, imposter, b, 1, epoch)); e.Kind != Followed {
			t.Fatalf("an imposter redirecting ITSELF should be fine: %s", e.Reason)
		}
		if r.Resolve(a.ID()) != a.ID() {
			t.Error("a stranger's succession moved an unrelated node's identity")
		}
	})

	t.Run("successor key does not hash to the successor ID", func(t *testing.T) {
		// Build a valid record, then corrupt the successor ID inside the body so
		// the embedded key no longer hashes to it.
		rec := succeed(t, a, b, 1, epoch)
		body := append([]byte(nil), rec.Body...)
		body[0] ^= 0xFF
		if _, err := record.UnmarshalSuccessionBody(body); err == nil {
			t.Error("a body whose key does not hash to its successor was accepted")
		}
	})

	t.Run("unknown predecessor", func(t *testing.T) {
		r, _ := fixture(t) // no keys learned at all
		e := r.Offer(succeed(t, a, b, 1, epoch))
		if e.Kind != Refused {
			t.Fatalf("a succession from an unknown node was %s", e.Kind)
		}
		// Naming the successor in the alert is useful; trusting it is not.
		if e.Successor != b.ID() {
			t.Error("the alert should still name the claimed successor")
		}
		if r.Resolve(a.ID()) != a.ID() {
			t.Error("an unverifiable succession was followed anyway")
		}
	})

	t.Run("tampered signature", func(t *testing.T) {
		r, _ := fixture(t, a)
		rec := succeed(t, a, b, 1, epoch)
		wire := rec.Marshal()
		wire[len(wire)-1] ^= 0xFF // corrupt the signature
		tampered, err := record.Unmarshal(wire)
		if err != nil {
			return // rejected at the parser, which is also correct
		}
		if e := r.Offer(tampered); e.Kind != Refused {
			t.Errorf("a succession with a broken signature was %s", e.Kind)
		}
	})
}

// A node cannot succeed itself: that is either a mistake or an attempt to make
// the tombstone rule reject the node's own future records.
func TestSelfSuccessionIsRefused(t *testing.T) {
	a := key(t, 1)
	if _, err := record.NewSuccessionRecord(a, 1, uint32(epoch.Unix()),
		a.ID(), a.Public, uint32(epoch.Unix())); err == nil {
		t.Error("a node was allowed to build a succession naming itself")
	}
}

func TestSuccessionBodyRoundTrip(t *testing.T) {
	a, b := key(t, 1), key(t, 2)
	body := record.SuccessionBody{
		Successor:    b.ID(),
		NewPublicKey: b.Public,
		Effective:    uint32(epoch.Unix()),
	}
	enc, err := record.MarshalSuccessionBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(enc) != record.SuccessionBodyLen {
		t.Errorf("encoded to %d bytes, want the fixed %d", len(enc), record.SuccessionBodyLen)
	}
	got, err := record.UnmarshalSuccessionBody(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Successor != body.Successor || got.Effective != body.Effective ||
		string(got.NewPublicKey) != string(body.NewPublicKey) {
		t.Errorf("round trip changed the body: %+v", got)
	}

	// Fixed width means there is no length field to disagree with, so short and
	// long inputs are both simply wrong.
	for _, bad := range [][]byte{nil, enc[:len(enc)-1], append(append([]byte(nil), enc...), 0)} {
		if _, err := record.UnmarshalSuccessionBody(bad); err == nil {
			t.Errorf("accepted a %d-byte body", len(bad))
		}
	}
	_ = a
}

// The whole record path, end to end, the way a peer would see it.
func TestVerifySuccessionRecord(t *testing.T) {
	a, b, c := key(t, 1), key(t, 2), key(t, 3)
	rec := succeed(t, a, b, 1, epoch)

	if _, err := record.VerifySuccessionRecord(rec, a.Public); err != nil {
		t.Fatalf("a valid succession failed verification: %v", err)
	}
	// Verifying with the wrong key must fail loudly rather than quietly
	// succeeding against whatever was supplied.
	if _, err := record.VerifySuccessionRecord(rec, c.Public); err == nil {
		t.Error("verified a succession against an unrelated public key")
	}
	// And a non-SUCCESSION record is refused rather than misparsed.
	node, err := record.NewNodeRecord(a, 2, uint32(epoch.Unix()), "n", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := record.VerifySuccessionRecord(node, a.Public); err == nil {
		t.Error("a NODE record was accepted as a SUCCESSION")
	}
}
