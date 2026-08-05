package webd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/store"
)

// The whole passkey path, end to end: an SSH-minted code becomes a registered
// credential, and that credential becomes a session ([D17], [D18]).
//
// This is the path that had no coverage — every other test in this package
// mints a session directly, because a passkey needs a device. It has one now
// (authenticator_test.go), so nothing here is mocked: go-webauthn verifies real
// attestation and real signatures.

// client keeps cookies across requests, which the ceremonies require: begin
// sets one and finish is meaningless without it.
type client struct {
	f       *fixture
	t       *testing.T
	cookies map[string]string
}

func newClient(t *testing.T, f *fixture) *client {
	return &client{f: f, t: t, cookies: map[string]string{}}
}

func (c *client) post(path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.request(http.MethodPost, path, body)
}

func (c *client) get(path string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.request(http.MethodGet, path, nil)
}

func (c *client) request(method, path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()

	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Origin", testOrigin)
	for k, v := range c.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	w := httptest.NewRecorder()
	c.f.srv.http.Handler.ServeHTTP(w, req)

	for _, sc := range w.Result().Cookies() {
		if sc.MaxAge < 0 || sc.Value == "" {
			delete(c.cookies, sc.Name)
			continue
		}
		c.cookies[sc.Name] = sc.Value
	}
	return w
}

// challengeFrom pulls the challenge out of a begin response, whichever ceremony
// it belongs to.
func challengeFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("could not read begin response: %v\n%s", err, w.Body.String())
	}
	if out.PublicKey.Challenge == "" {
		t.Fatalf("no challenge in begin response:\n%s", w.Body.String())
	}
	return out.PublicKey.Challenge
}

func userHandleFrom(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var out struct {
		PublicKey struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.PublicKey.User.ID
}

// enrol runs the full registration ceremony and returns the authenticator.
func enrol(t *testing.T, f *fixture, c *client, nick string) *authenticator {
	t.Helper()

	code, hash, err := auth.NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutEnrolmentCode(f.ctx, nick, hash, f.clock.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}

	begin := c.post("/auth/enrol/begin", map[string]string{"code": code})
	if begin.Code != http.StatusOK {
		t.Fatalf("enrol/begin = %d: %s", begin.Code, bodyError(t, begin))
	}

	a := newAuthenticator(t, "bbs.example.com", testOrigin)
	// The user handle is issued by the server and stored on the device; it is
	// what a discoverable credential presents at sign-in instead of a nick.
	handle, err := decodeB64(userHandleFrom(t, begin))
	if err != nil {
		t.Fatal(err)
	}
	a.handle = handle

	finish := c.post("/auth/enrol/finish", a.Register(t, challengeFrom(t, begin)))
	if finish.Code != http.StatusOK {
		t.Fatalf("enrol/finish = %d: %s", finish.Code, bodyError(t, finish))
	}
	return a
}

// signIn runs the full assertion ceremony.
func signIn(t *testing.T, c *client, a *authenticator) *httptest.ResponseRecorder {
	t.Helper()
	begin := c.post("/auth/login/begin", nil)
	if begin.Code != http.StatusOK {
		t.Fatalf("login/begin = %d: %s", begin.Code, bodyError(t, begin))
	}
	return c.post("/auth/login/finish", a.Assert(t, challengeFrom(t, begin)))
}

// TestPasskeyEnrolThenSignIn is the path [D18] exists to make possible: an
// account that predates the web gets a credential from an SSH-minted code, and
// then signs in with it.
func TestPasskeyEnrolThenSignIn(t *testing.T) {
	f := newFixture(t)
	c := newClient(t, f)

	a := enrol(t, f, c, "austin")

	// The credential is real and stored against the account.
	creds, err := f.store.WebAuthnCredentials(f.ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 1 {
		t.Fatalf("stored %d credentials, want 1", len(creds))
	}

	// Enrolment deliberately does NOT open a session ([D18]).
	if me := c.get("/api/me"); me.Code == http.StatusOK {
		t.Error("finishing enrolment signed the user in; no path may lead from a code to a session")
	}

	// The passkey does.
	w := signIn(t, c, a)
	if w.Code != http.StatusOK {
		t.Fatalf("login/finish = %d: %s", w.Code, bodyError(t, w))
	}

	me := c.get("/api/me")
	if me.Code != http.StatusOK {
		t.Fatalf("/api/me after sign-in = %d", me.Code)
	}
	var who struct {
		Nick string `json:"nick"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &who); err != nil {
		t.Fatal(err)
	}
	if who.Nick != "austin" {
		t.Errorf("signed in as %q, want austin", who.Nick)
	}
}

// TestPasskeySignInNeedsNoNick — discoverable credentials are the reason the
// browser path is less work than SSH rather than more ([D17]). Nothing the
// client sends at login names an account.
func TestPasskeySignInNeedsNoNick(t *testing.T) {
	f := newFixture(t)
	c := newClient(t, f)
	a := enrol(t, f, c, "austin")

	begin := c.post("/auth/login/begin", nil)
	if strings.Contains(begin.Body.String(), "austin") {
		t.Errorf("the sign-in challenge names the account:\n%s", begin.Body.String())
	}

	assertion := a.Assert(t, challengeFrom(t, begin))
	raw, _ := json.Marshal(assertion)
	if strings.Contains(string(raw), "austin") {
		t.Errorf("the assertion names the account:\n%s", raw)
	}
	if w := c.post("/auth/login/finish", assertion); w.Code != http.StatusOK {
		t.Fatalf("login/finish = %d: %s", w.Code, bodyError(t, w))
	}
}

// TestPasskeyRejectsAForgedSignature — the assertion is only worth anything if
// a wrong key fails. A different authenticator claiming the same credential ID
// is the attack this must refuse.
func TestPasskeyRejectsAForgedSignature(t *testing.T) {
	f := newFixture(t)
	c := newClient(t, f)
	real := enrol(t, f, c, "austin")

	forger := newAuthenticator(t, "bbs.example.com", testOrigin)
	forger.credID = real.credID // claim the enrolled credential
	forger.handle = real.handle

	begin := c.post("/auth/login/begin", nil)
	w := c.post("/auth/login/finish", forger.Assert(t, challengeFrom(t, begin)))
	if w.Code == http.StatusOK {
		t.Fatal("a signature from the wrong key was accepted")
	}
	if me := c.get("/api/me"); me.Code == http.StatusOK {
		t.Error("a rejected assertion still opened a session")
	}
}

// TestPasskeyRejectsAReplayedChallenge — a challenge answered twice is a replay,
// and the ceremony store is single-use precisely to stop it.
func TestPasskeyRejectsAReplayedChallenge(t *testing.T) {
	f := newFixture(t)
	c := newClient(t, f)
	a := enrol(t, f, c, "austin")

	begin := c.post("/auth/login/begin", nil)
	challenge := challengeFrom(t, begin)
	assertion := a.Assert(t, challenge)

	if w := c.post("/auth/login/finish", assertion); w.Code != http.StatusOK {
		t.Fatalf("first use should succeed: %d %s", w.Code, bodyError(t, w))
	}
	// Same challenge, same signature, replayed.
	if w := c.post("/auth/login/finish", assertion); w.Code == http.StatusOK {
		t.Error("a replayed assertion was accepted")
	}
}

// TestPasskeyRejectsAForeignOrigin — the origin is bound into the signed client
// data, which is what makes a passkey phishing-resistant. A credential minted
// for another site must not work here.
func TestPasskeyRejectsAForeignOrigin(t *testing.T) {
	f := newFixture(t)
	c := newClient(t, f)
	enrol(t, f, c, "austin")

	evil := newAuthenticator(t, "evil.example.com", "https://evil.example.com")
	begin := c.post("/auth/login/begin", nil)
	if w := c.post("/auth/login/finish", evil.Assert(t, challengeFrom(t, begin))); w.Code == http.StatusOK {
		t.Error("a credential from another origin was accepted")
	}
}

// TestPasskeyDrivesTheBBS closes the loop: the session a real passkey produced
// carries the WebSocket bridge, so signing in actually reaches the BBS.
func TestPasskeyDrivesTheBBS(t *testing.T) {
	f := newFixture(t)
	c := newClient(t, f)
	a := enrol(t, f, c, "austin")

	if w := signIn(t, c, a); w.Code != http.StatusOK {
		t.Fatalf("sign-in failed: %d %s", w.Code, bodyError(t, w))
	}

	sessionID := c.cookies[sessionCookie]
	if sessionID == "" {
		t.Fatal("sign-in set no session cookie")
	}

	ts := httptest.NewServer(f.srv.http.Handler)
	defer ts.Close()

	ctx := context.Background()
	conn := dialWS(t, ctx, ts, sessionID)
	msg := readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})
	if msg.Nick != "austin" {
		t.Errorf("the BBS greeted %q, want austin", msg.Nick)
	}
}

// TestPasskeyEnrolmentSurvivesABarredAccount — an account the sysop disabled
// must not be able to enrol its way back in.
func TestPasskeyBarredAccountCannotEnrol(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateUser(f.ctx, store.CreateUserOptions{
		Nick: "banned", CanLogin: false,
	}); err != nil {
		t.Fatal(err)
	}

	code, hash, err := auth.NewEnrolmentCode()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutEnrolmentCode(f.ctx, "banned", hash,
		f.clock.Now().Add(10*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}

	c := newClient(t, f)
	w := c.post("/auth/enrol/begin", map[string]string{"code": code})
	if w.Code != http.StatusForbidden {
		t.Errorf("a barred account got %d, want 403", w.Code)
	}
}

func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}
