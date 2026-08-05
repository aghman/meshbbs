package webd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/clock"
)

func TestLimiterSpendsAndRefills(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	l := newLimiter(clk, 10)

	// A full hour's allowance is available up front, which is what lets a
	// person mistype a code twice without being told to come back later.
	for i := range 10 {
		if ok, _ := l.allow("client"); !ok {
			t.Fatalf("attempt %d was refused inside the allowance", i+1)
		}
	}
	ok, wait := l.allow("client")
	if ok {
		t.Fatal("the eleventh attempt was allowed")
	}
	if wait <= 0 {
		t.Error("a refusal should say how long to wait")
	}

	// Six minutes is a tenth of an hour, so one token comes back.
	clk.Advance(6 * time.Minute)
	if ok, _ := l.allow("client"); !ok {
		t.Error("a token should have refilled after six minutes")
	}
	if ok, _ := l.allow("client"); ok {
		t.Error("only one token should have refilled")
	}
}

// TestLimiterIsPerClient — one attacker exhausting their allowance must not
// lock out everyone else, which is the entire point of keying it.
func TestLimiterIsPerClient(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	l := newLimiter(clk, 3)

	for range 3 {
		l.allow("attacker")
	}
	if ok, _ := l.allow("attacker"); ok {
		t.Fatal("the attacker still has budget")
	}
	if ok, _ := l.allow("someone-else"); !ok {
		t.Error("an unrelated client was refused")
	}
}

// TestLimiterRefillIsCapped — an idle client accumulates one hour's worth, not
// a day's, or a patient attacker gets a free burst.
func TestLimiterRefillIsCapped(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	l := newLimiter(clk, 5)

	clk.Advance(24 * time.Hour)
	for i := range 5 {
		if ok, _ := l.allow("client"); !ok {
			t.Fatalf("attempt %d refused after a long idle", i+1)
		}
	}
	if ok, _ := l.allow("client"); ok {
		t.Error("a day of idling granted more than one hour's allowance")
	}
}

// TestClientKeyIgnoresForwardedFor is the one that matters. Trusting the header
// means an attacker sends a different value per request and the limiter counts
// each separately — worse than no limiter, because it looks like protection.
func TestClientKeyIgnoresForwardedFor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/auth/enrol/begin", nil)
	r.RemoteAddr = "203.0.113.7:44321"

	base := clientKey(r)
	for _, spoof := range []string{"1.2.3.4", "5.6.7.8, 9.10.11.12", ""} {
		r.Header.Set("X-Forwarded-For", spoof)
		if got := clientKey(r); got != base {
			t.Errorf("X-Forwarded-For %q changed the key: %q vs %q", spoof, got, base)
		}
	}
	if base != "203.0.113.7" {
		t.Errorf("key = %q, want the remote host without its port", base)
	}
}

// TestEnrolIsRateLimited is the endpoint the anti-oracle argument depends on.
func TestEnrolIsRateLimited(t *testing.T) {
	f := newFixture(t)

	body := map[string]string{"code": "AAAA-AAAA-AAAAA"}
	var lastCode int
	for range f.srv.opts.EnrolAttemptsPerHour + 5 {
		w := f.do(t, http.MethodPost, "/auth/enrol/begin", body, testOrigin)
		lastCode = w.Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("guessing was never throttled: last status %d", lastCode)
	}

	// The refusal has to be actionable and machine-readable.
	w := f.do(t, http.MethodPost, "/auth/enrol/begin", body, testOrigin)
	if w.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header on a 429")
	}
	if msg := bodyError(t, w); !strings.Contains(msg, "wait") {
		t.Errorf("refusal is not actionable: %q", msg)
	}
}

// TestEnrolLimitIsStricterThanAuth — enrolment is where codes get guessed, so
// it should not inherit sign-in's looser ceiling.
func TestEnrolLimitIsStricterThanAuth(t *testing.T) {
	f := newFixture(t)
	if f.srv.opts.EnrolAttemptsPerHour >= f.srv.opts.AuthAttemptsPerHour {
		t.Errorf("enrolment allows %d/hour against sign-in's %d: the endpoint where "+
			"codes are tested should be the tighter one",
			f.srv.opts.EnrolAttemptsPerHour, f.srv.opts.AuthAttemptsPerHour)
	}
}

// TestThrottlingDoesNotConsumeCodes — the limiter has to run BEFORE redemption,
// or an attacker could burn a victim's live code by spamming the endpoint
// without ever guessing it. That would turn a defence into a denial of service
// against the one operation the code exists for.
//
// Checked against the store rather than by waiting out the limiter: refilling
// the allowance takes an hour, and an hour also expires the code, so the
// round-trip could not tell "never consumed" from "expired meanwhile".
func TestThrottlingDoesNotConsumeCodes(t *testing.T) {
	f := newFixture(t)
	code := f.issueCode(t, "austin")

	// Exhaust the allowance on wrong guesses.
	for range f.srv.opts.EnrolAttemptsPerHour + 2 {
		f.do(t, http.MethodPost, "/auth/enrol/begin",
			map[string]string{"code": "AAAA-AAAA-AAAAA"}, testOrigin)
	}

	w := f.do(t, http.MethodPost, "/auth/enrol/begin", map[string]string{"code": code}, testOrigin)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected throttling, got %d", w.Code)
	}

	// The victim's code is untouched: still live, still theirs.
	user, err := f.store.RedeemEnrolmentCode(f.ctx, auth.HashEnrolmentCode(code))
	if err != nil {
		t.Fatalf("a throttled attempt consumed the code: %v", err)
	}
	if user.Nick != "austin" {
		t.Errorf("code redeemed to %q, want austin", user.Nick)
	}
}
