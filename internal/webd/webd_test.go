package webd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

const testOrigin = "https://bbs.example.com"

type fixture struct {
	srv   *Server
	store *store.Store
	clock *clock.Virtual
	ctx   context.Context
}

func newFixture(t *testing.T) *fixture {
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
	if err := st.PutNode(ctx, store.Node{
		ID: key.ID(), PublicKey: key.Public, DisplayName: "test-bbs", IsSelf: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{Nick: "austin", CanLogin: true}); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(bbs.New(st, key, clk), st, Options{
		Origin:                  testOrigin,
		MaxSessions:             4,
		MaxSessionsPerUser:      2,
		IdleTimeoutMins:         30,
		UnlockedIdleTimeoutMins: 10,
		SessionTTLHours:         12,
		Clock:                   clk,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{srv: srv, store: st, clock: clk, ctx: ctx}
}

// do runs a request through the full handler chain, including the security
// middleware — testing the mux alone would skip the CSRF check entirely.
func (f *fixture) do(t *testing.T, method, path string, body any, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	f.srv.http.Handler.ServeHTTP(w, req)
	return w
}

func bodyError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		return w.Body.String()
	}
	if s, ok := out["error"].(string); ok {
		return s
	}
	return w.Body.String()
}

// ---------------------------------------------------------------------------
// CSRF and headers
// ---------------------------------------------------------------------------

// TestCrossOriginPostsAreRefused is the check that matters most here: a
// cookie-authenticated POST is not protected by the same-origin policy the way
// fetch() is, so without this any site could drive these endpoints.
func TestCrossOriginPostsAreRefused(t *testing.T) {
	f := newFixture(t)

	cases := map[string]string{
		"another site":   "https://evil.example.com",
		"missing":        "",
		"scheme differs": "http://bbs.example.com",
	}
	for name, origin := range cases {
		t.Run(name, func(t *testing.T) {
			w := f.do(t, http.MethodPost, "/auth/login/begin", nil, origin)
			if w.Code != http.StatusForbidden {
				t.Errorf("origin %q got %d, want 403", origin, w.Code)
			}
		})
	}

	// The real origin still works, or the check would be useless in a
	// different way.
	if w := f.do(t, http.MethodPost, "/auth/login/begin", nil, testOrigin); w.Code != http.StatusOK {
		t.Errorf("same-origin request got %d, want 200: %s", w.Code, bodyError(t, w))
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodGet, "/api/me", nil, "")

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
	}
	for h, v := range want {
		if got := w.Header().Get(h); got != v {
			t.Errorf("%s = %q, want %q", h, got, v)
		}
	}
	// The page is self-contained by design — no CDN, no external font — so the
	// CSP should be able to forbid everything else outright.
	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP missing %q: %s", want, csp)
		}
	}
}

func TestMeRequiresASession(t *testing.T) {
	f := newFixture(t)
	if w := f.do(t, http.MethodGet, "/api/me", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /api/me = %d, want 401", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Enrolment ([D18])
// ---------------------------------------------------------------------------

func (f *fixture) issueCode(t *testing.T, nick string) string {
	t.Helper()
	code, hash, err := auth.NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	expires := f.clock.Now().Add(10 * time.Minute).Unix()
	if err := f.store.PutEnrolmentCode(f.ctx, nick, hash, expires); err != nil {
		t.Fatal(err)
	}
	return code
}

func TestEnrolBeginAcceptsAValidCode(t *testing.T) {
	f := newFixture(t)
	code := f.issueCode(t, "austin")

	w := f.do(t, http.MethodPost, "/auth/enrol/begin", map[string]string{"code": code}, testOrigin)
	if w.Code != http.StatusOK {
		t.Fatalf("enrol/begin = %d: %s", w.Code, bodyError(t, w))
	}

	// The response must be usable WebAuthn creation options, and must require a
	// resident key — without one the user would have to type a nick to sign in,
	// which is the friction [D17] set out to remove.
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	pk, ok := out["publicKey"].(map[string]any)
	if !ok {
		t.Fatalf("no publicKey in creation options: %s", w.Body.String())
	}
	if pk["challenge"] == nil {
		t.Error("creation options carry no challenge")
	}
	sel, _ := pk["authenticatorSelection"].(map[string]any)
	if sel == nil || sel["residentKey"] != "required" {
		t.Errorf("resident key not required: %v", sel)
	}

	// A ceremony cookie must be set, or finish has nothing to match against.
	if !strings.Contains(w.Header().Get("Set-Cookie"), ceremonyCookie) {
		t.Error("no ceremony cookie was set")
	}
}

// TestEnrolBeginSpendsTheCode pins the anti-oracle property. Redeeming at
// finish instead would let a caller test codes all day and only burn one when
// it worked, which is what makes a rate-limited 64-bit code sufficient.
func TestEnrolBeginSpendsTheCode(t *testing.T) {
	f := newFixture(t)
	code := f.issueCode(t, "austin")

	if w := f.do(t, http.MethodPost, "/auth/enrol/begin",
		map[string]string{"code": code}, testOrigin); w.Code != http.StatusOK {
		t.Fatalf("first use = %d: %s", w.Code, bodyError(t, w))
	}
	w := f.do(t, http.MethodPost, "/auth/enrol/begin", map[string]string{"code": code}, testOrigin)
	if w.Code != http.StatusBadRequest {
		t.Errorf("reused code = %d, want 400", w.Code)
	}
}

// TestEnrolTellsExpiredFromWrong keeps the two failures distinguishable. "That
// expired, get another" is actionable; "wrong code" sends someone off to
// re-read what they already typed correctly.
func TestEnrolTellsExpiredFromWrong(t *testing.T) {
	f := newFixture(t)
	code := f.issueCode(t, "austin")
	f.clock.Advance(11 * time.Minute)

	w := f.do(t, http.MethodPost, "/auth/enrol/begin", map[string]string{"code": code}, testOrigin)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expired code = %d, want 400", w.Code)
	}
	if msg := bodyError(t, w); !strings.Contains(msg, "expired") || !strings.Contains(msg, "press P") {
		t.Errorf("expiry message is not actionable: %q", msg)
	}

	other, _, err := auth.NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	w = f.do(t, http.MethodPost, "/auth/enrol/begin", map[string]string{"code": other}, testOrigin)
	if msg := bodyError(t, w); strings.Contains(msg, "expired") {
		t.Errorf("an unknown code was reported as expired: %q", msg)
	}
}

func TestEnrolRejectsEmptyCode(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodPost, "/auth/enrol/begin", map[string]string{"code": "  "}, testOrigin)
	if w.Code != http.StatusBadRequest {
		t.Errorf("empty code = %d, want 400", w.Code)
	}
}

// TestEnrolFinishNeedsACeremony — [D18]'s invariant is that no path leads from
// a code to a session, and finish without a live ceremony must not invent one.
func TestEnrolFinishNeedsACeremony(t *testing.T) {
	f := newFixture(t)
	w := f.do(t, http.MethodPost, "/auth/enrol/finish", map[string]string{}, testOrigin)
	if w.Code != http.StatusBadRequest {
		t.Errorf("finish with no ceremony = %d, want 400", w.Code)
	}
	if strings.Contains(w.Header().Get("Set-Cookie"), sessionCookie+"=") &&
		!strings.Contains(w.Header().Get("Set-Cookie"), sessionCookie+"=;") {
		t.Error("a session cookie was issued without a completed ceremony")
	}
}
