package webd

import (
	"net"
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

// TestClientKeyIgnoresForwardedForByDefault is the one that matters. With no
// trusted proxies configured, honouring the header would let an attacker send a
// different value per request — worse than no limiter, because it looks like
// protection and provides none.
func TestClientKeyIgnoresForwardedForByDefault(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/auth/enrol/begin", nil)
	r.RemoteAddr = "203.0.113.7:44321"

	base := clientKey(r, nil)
	for _, spoof := range []string{"1.2.3.4", "5.6.7.8, 9.10.11.12", ""} {
		r.Header.Set("X-Forwarded-For", spoof)
		if got := clientKey(r, nil); got != base {
			t.Errorf("X-Forwarded-For %q changed the key: %q vs %q", spoof, got, base)
		}
	}
	if base != "203.0.113.7" {
		t.Errorf("key = %q, want the remote host without its port", base)
	}
}

func proxies(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// TestClientKeyBehindATrustedProxy — the header is only consulted when the
// request actually came from a proxy the sysop listed.
func TestClientKeyBehindATrustedProxy(t *testing.T) {
	trusted := proxies(t, "10.0.0.0/8")

	cases := []struct {
		name      string
		remote    string
		forwarded []string
		want      string
	}{
		{
			name:      "proxy reports the client",
			remote:    "10.0.0.1:5000",
			forwarded: []string{"203.0.113.7"},
			want:      "203.0.113.7",
		},
		{
			// The rightmost entry is what the nearest proxy observed and the
			// only one it can vouch for. Everything left of it came from
			// whoever spoke earlier — including the client.
			name:      "a spoofed prefix is ignored",
			remote:    "10.0.0.1:5000",
			forwarded: []string{"1.2.3.4, 5.6.7.8, 203.0.113.7"},
			want:      "203.0.113.7",
		},
		{
			name:      "trusted hops are skipped",
			remote:    "10.0.0.1:5000",
			forwarded: []string{"203.0.113.7, 10.0.0.9"},
			want:      "203.0.113.7",
		},
		{
			name:      "repeated header lines are one chain",
			remote:    "10.0.0.1:5000",
			forwarded: []string{"1.2.3.4", "203.0.113.7, 10.0.0.9"},
			want:      "203.0.113.7",
		},
		{
			// An untrusted sender's header is not evidence of anything.
			name:      "untrusted remote keeps its own address",
			remote:    "198.51.100.4:5000",
			forwarded: []string{"203.0.113.7"},
			want:      "198.51.100.4",
		},
		{
			name:      "no header falls back to the proxy",
			remote:    "10.0.0.1:5000",
			forwarded: nil,
			want:      "10.0.0.1",
		},
		{
			// If every hop is a proxy there is no client to attribute to, so
			// the nearest real address is the honest answer.
			name:      "all hops trusted",
			remote:    "10.0.0.1:5000",
			forwarded: []string{"10.0.0.8, 10.0.0.9"},
			want:      "10.0.0.1",
		},
		{
			// Garbage means the chain cannot be believed past that point.
			name:      "malformed hop stops the walk",
			remote:    "10.0.0.1:5000",
			forwarded: []string{"203.0.113.7, not-an-ip"},
			want:      "10.0.0.1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/auth/enrol/begin", nil)
			r.RemoteAddr = tc.remote
			for _, line := range tc.forwarded {
				r.Header.Add("X-Forwarded-For", line)
			}
			if got := clientKey(r, trusted); got != tc.want {
				t.Errorf("clientKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTrustedProxyLimitsAreStillPerClient is the point of the whole feature:
// two clients behind one proxy must not share an allowance.
func TestTrustedProxyLimitsAreStillPerClient(t *testing.T) {
	f := newFixture(t)
	f.srv.opts.TrustedProxies = proxies(t, "192.0.2.0/24")

	body := map[string]string{"code": "AAAA-AAAA-AAAAA"}
	exhaust := func(client string) int {
		var last int
		for range f.srv.opts.EnrolAttemptsPerHour + 2 {
			last = f.doAs(t, http.MethodPost, "/auth/enrol/begin", body,
				testOrigin, "192.0.2.10:5000", client).Code
		}
		return last
	}

	if got := exhaust("203.0.113.7"); got != http.StatusTooManyRequests {
		t.Fatalf("first client was never throttled: %d", got)
	}
	// A different client behind the same proxy still has its own budget.
	w := f.doAs(t, http.MethodPost, "/auth/enrol/begin", body,
		testOrigin, "192.0.2.10:5000", "198.51.100.4")
	if w.Code == http.StatusTooManyRequests {
		t.Error("a second client behind the same proxy inherited the first's exhaustion")
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
