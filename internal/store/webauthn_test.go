package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
)

func webFixture(t *testing.T) (*Store, context.Context, *clock.Virtual) {
	t.Helper()
	ctx := context.Background()
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	st, err := OpenMemory(ctx, clk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateUser(ctx, CreateUserOptions{Nick: "austin", CanLogin: true}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, clk
}

func TestPasskeyEnrolAndLookup(t *testing.T) {
	st, ctx, _ := webFixture(t)

	cred := WebAuthnCredential{
		CredentialID: []byte("cred-1"),
		PublicKey:    []byte("pub-1"),
		Transports:   []string{"internal", "hybrid"},
		Label:        "phone",
	}
	if err := st.AddWebAuthnCredential(ctx, "austin", cred); err != nil {
		t.Fatal(err)
	}

	// Sign-in arrives with only the credential ID and must resolve the account
	// by itself — discoverable credentials mean no nick is ever typed.
	got, user, err := st.WebAuthnCredentialByID(ctx, []byte("cred-1"))
	if err != nil {
		t.Fatal(err)
	}
	if user.Nick != "austin" {
		t.Errorf("credential resolved to %q, want austin", user.Nick)
	}
	if string(got.PublicKey) != "pub-1" {
		t.Errorf("public key = %q", got.PublicKey)
	}
	if len(got.Transports) != 2 || got.Transports[0] != "internal" {
		t.Errorf("transports = %v", got.Transports)
	}

	if err := st.AddWebAuthnCredential(ctx, "austin", cred); !errors.Is(err, ErrCredentialExists) {
		t.Errorf("re-enrolling the same credential = %v, want ErrCredentialExists", err)
	}
}

// TestPasskeyCloneIsRefused pins the counter rule: a real authenticator only
// counts up, so a repeat or a regression means the credential exists on two
// devices. Accepting it and writing the lower value would hide exactly the
// event the counter exists to reveal.
func TestPasskeyCloneIsRefused(t *testing.T) {
	st, ctx, _ := webFixture(t)
	if err := st.AddWebAuthnCredential(ctx, "austin", WebAuthnCredential{
		CredentialID: []byte("c"), PublicKey: []byte("p"), SignCount: 5,
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.UseWebAuthnCredential(ctx, []byte("c"), 6); err != nil {
		t.Fatalf("advancing the counter should succeed: %v", err)
	}
	if err := st.UseWebAuthnCredential(ctx, []byte("c"), 6); !errors.Is(err, ErrCloned) {
		t.Errorf("replayed counter = %v, want ErrCloned", err)
	}
	if err := st.UseWebAuthnCredential(ctx, []byte("c"), 3); !errors.Is(err, ErrCloned) {
		t.Errorf("regressed counter = %v, want ErrCloned", err)
	}
}

// TestPasskeyZeroCounterIsNotAClone guards the other side of that rule. Most
// platform authenticators never implement a counter and report zero forever;
// treating that as a clone would lock out the majority of real passkeys.
func TestPasskeyZeroCounterIsNotAClone(t *testing.T) {
	st, ctx, _ := webFixture(t)
	if err := st.AddWebAuthnCredential(ctx, "austin", WebAuthnCredential{
		CredentialID: []byte("c"), PublicKey: []byte("p"), SignCount: 0,
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := st.UseWebAuthnCredential(ctx, []byte("c"), 0); err != nil {
			t.Fatalf("use %d with a counterless authenticator: %v", i, err)
		}
	}
}

func TestEnrolmentCodeIsSingleUse(t *testing.T) {
	st, ctx, _ := webFixture(t)

	const hash = "deadbeef"
	if err := st.PutEnrolmentCode(ctx, "austin", hash, st.now()+600); err != nil {
		t.Fatal(err)
	}

	u, err := st.RedeemEnrolmentCode(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if u.Nick != "austin" {
		t.Errorf("redeemed to %q, want austin", u.Nick)
	}

	if _, err := st.RedeemEnrolmentCode(ctx, hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("second redemption = %v, want ErrNotFound", err)
	}
}

// TestEnrolmentCodeIssueReplaces is what makes a shoulder-surfed code
// self-healing: the owner issues another and the old one is dead.
func TestEnrolmentCodeIssueReplaces(t *testing.T) {
	st, ctx, _ := webFixture(t)

	if err := st.PutEnrolmentCode(ctx, "austin", "first", st.now()+600); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEnrolmentCode(ctx, "austin", "second", st.now()+600); err != nil {
		t.Fatal(err)
	}

	if _, err := st.RedeemEnrolmentCode(ctx, "first"); !errors.Is(err, ErrNotFound) {
		t.Errorf("superseded code = %v, want ErrNotFound", err)
	}
	if _, err := st.RedeemEnrolmentCode(ctx, "second"); err != nil {
		t.Errorf("current code should redeem: %v", err)
	}
}

// TestExpiredEnrolmentCodeIsConsumedAnyway checks that expiry is reported
// distinctly — so the UI can say "ask for another" rather than "wrong code" —
// and that the row is still destroyed, keeping single-use true in every path.
func TestExpiredEnrolmentCodeIsConsumedAnyway(t *testing.T) {
	st, ctx, clk := webFixture(t)

	if err := st.PutEnrolmentCode(ctx, "austin", "h", st.now()+60); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * time.Minute)

	if _, err := st.RedeemEnrolmentCode(ctx, "h"); !errors.Is(err, ErrCodeExpired) {
		t.Errorf("expired code = %v, want ErrCodeExpired", err)
	}
	if _, err := st.RedeemEnrolmentCode(ctx, "h"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired code survived redemption: %v", err)
	}
}
