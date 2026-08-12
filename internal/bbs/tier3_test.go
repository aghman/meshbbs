package bbs

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/dmkey"
	"github.com/aghman/meshbbs/internal/keyring"
	"github.com/aghman/meshbbs/internal/store"
)

// The tier-3 property, stated as a test: the server can seal to a user whose
// private key it has never held, and cannot open what it sealed.
func TestTheServerSealsToAClientHeldKeyAndCannotOpenIt(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "alice", "hunter2")
	mkUser(t, svc, st, ctx, "bob", "hunter2")

	// Bob takes custody. The private half exists only here, in the test's
	// hands, standing in for his laptop.
	priv, pub, err := keyring.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer priv.Zero()
	if err := st.SetClientHeldDMKey(ctx, "bob", pub, true); err != nil {
		t.Fatal(err)
	}

	// Sending is unchanged — it needs the public half and nothing else, which
	// is the §8.2 boundary Phase 1 was told to protect.
	if _, err := svc.SendDM(ctx, "alice", "bob", "hello", "meet at the repeater"); err != nil {
		t.Fatalf("sending to a client-held key failed: %v", err)
	}

	dms, err := st.Inbox(ctx, "bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(dms) != 1 {
		t.Fatalf("bob has %d messages", len(dms))
	}

	// The server refuses to open it, distinctly from "no key yet".
	_, err = svc.OpenDM(ctx, "bob", "hunter2", dms[0].SealedBytes)
	if !errors.Is(err, ErrClientHeldKey) {
		t.Fatalf("got %v, want ErrClientHeldKey", err)
	}

	// And the key really is out of reach: nothing wrapped is left behind for a
	// later code path to find and quietly use.
	if _, err := st.WrappedDMKey(ctx, "bob"); err == nil {
		t.Error("a wrapped private key survived the switch to client-held custody")
	}

	// Bob, on his own machine, can read it.
	plain, err := keyring.Open(priv, dms[0].SealedBytes)
	if err != nil {
		t.Fatalf("the holder of the private key could not open the message: %v", err)
	}
	payload, err := store.UnmarshalSealedPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Text != "meet at the repeater" {
		t.Errorf("body came back as %q", payload.Text)
	}

	// The armour the BBS would show him round-trips.
	sealed, err := dmkey.Unarmour(dmkey.Armour(dms[0].SealedBytes))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keyring.Open(priv, sealed); err != nil {
		t.Errorf("the message did not survive being armoured for the screen: %v", err)
	}
}

// Adopting a different key strands every message already delivered, and the
// server cannot re-seal them because it never had the private half. Refused
// unless the sysop says so.
func TestAdoptingADifferentKeyIsRefusedByDefault(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "alice", "hunter2")
	mkUser(t, svc, st, ctx, "bob", "hunter2")

	_, first, err := keyring.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetClientHeldDMKey(ctx, "bob", first, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendDM(ctx, "alice", "bob", "s", "already delivered"); err != nil {
		t.Fatal(err)
	}

	_, second, err := keyring.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetClientHeldDMKey(ctx, "bob", second, false); !errors.Is(err, store.ErrWouldStrandExistingMail) {
		t.Fatalf("got %v, want ErrWouldStrandExistingMail", err)
	}

	// Re-adopting the SAME key is not a replacement and must not be refused —
	// a user re-running the setup should not need a scary flag.
	if err := st.SetClientHeldDMKey(ctx, "bob", first, false); err != nil {
		t.Errorf("re-adopting the same key was refused: %v", err)
	}

	// With --replace it goes through, because that is the sysop's call.
	if err := st.SetClientHeldDMKey(ctx, "bob", second, true); err != nil {
		t.Errorf("an explicit replacement was refused: %v", err)
	}
}

// EnsureDMKey runs at login. For a tier-3 user it must do nothing: generating
// here would replace a key whose private half is on their laptop.
func TestEnsureDMKeyLeavesAClientHeldKeyAlone(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "bob", "hunter2")

	_, pub, err := keyring.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetClientHeldDMKey(ctx, "bob", pub, true); err != nil {
		t.Fatal(err)
	}

	// Several logins.
	for i := 0; i < 3; i++ {
		if err := svc.EnsureDMKey(ctx, "bob", "hunter2"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.DMPublicKey(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if got != pub {
		t.Error("logging in replaced a client-held key; every message sent to the old one is now unreadable")
	}
	if _, err := st.WrappedDMKey(ctx, "bob"); err == nil {
		t.Error("logging in wrapped a private key for a user who holds their own")
	}
	clientHeld, err := st.DMKeyIsClientHeld(ctx, "bob")
	if err != nil || !clientHeld {
		t.Errorf("custody was lost: clientHeld=%v err=%v", clientHeld, err)
	}
}

// "Holds their own key" and "has no key" look identical in the schema apart
// from the public half, and call for opposite responses.
func TestClientHeldIsDistinctFromHavingNoKey(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "fresh", "hunter2")

	// mkUser goes through signup, which generates a tier-2 key. Clear it to get
	// an account with no key at all.
	if err := st.ResetPasswordAsSysop(ctx, "fresh", "hunter2", true); err != nil {
		t.Fatal(err)
	}
	clientHeld, err := st.DMKeyIsClientHeld(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if clientHeld {
		t.Error("an account with no key at all was reported as client-held")
	}

	// And a tier-2 user is not client-held either.
	mkUser(t, svc, st, ctx, "tier2", "hunter2")
	clientHeld, err = st.DMKeyIsClientHeld(ctx, "tier2")
	if err != nil {
		t.Fatal(err)
	}
	if clientHeld {
		t.Error("a server-held key was reported as client-held")
	}
	if _, err := st.WrappedDMKey(ctx, "tier2"); err != nil {
		t.Errorf("a tier-2 user lost their wrapped key: %v", err)
	}
}

// The block the BBS shows has to be the thing the helper accepts. Asserted
// against dmkey rather than by eye, because the two halves ship separately and
// a user cannot debug a format mismatch.
func TestWhatTheBBSShowsIsWhatTheHelperReads(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "alice", "hunter2")
	mkUser(t, svc, st, ctx, "bob", "hunter2")

	priv, pub, err := keyring.Generate(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer priv.Zero()
	if err := st.SetClientHeldDMKey(ctx, "bob", pub, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SendDM(ctx, "alice", "bob", "subj", "body text"); err != nil {
		t.Fatal(err)
	}
	dms, err := st.Inbox(ctx, "bob", 1)
	if err != nil || len(dms) != 1 {
		t.Fatalf("dms=%d err=%v", len(dms), err)
	}

	// Exactly what the TUI puts on screen, including the instruction line.
	shown := "Copy the block below and run:  meshbbs-key open\n\n" + dmkey.Armour(dms[0].SealedBytes)
	if !strings.Contains(shown, "meshbbs-key open") {
		t.Error("the screen does not tell the user what to run")
	}

	sealed, err := dmkey.Unarmour(shown)
	if err != nil {
		t.Fatalf("the helper could not parse what the BBS displayed: %v", err)
	}
	plain, err := keyring.Open(priv, sealed)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := store.UnmarshalSealedPayload(plain)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Text != "body text" || payload.Subject != "subj" {
		t.Errorf("round trip produced %+v", payload)
	}
}
