package door

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
)

// fakeHost records what the API asked the BBS to do, and can be told to refuse.
type fakeHost struct {
	mu sync.Mutex

	state map[string]string
	// federated names areas that spend mesh airtime.
	federated map[string]bool
	// postErr, if set, is what PostAs returns — the shape a capability refusal
	// arrives in.
	postErr error

	announced []string
	posted    []string
	dms       []string
	audits    []string
	noticed   map[string]bool
	quota     int64
	used      int64

	// leagues names areas that are federated door leagues, and events is what
	// was queued for them.
	leagues map[string]bool
	events  []DoorEventRequest
	// knownTargets maps a target reference to the nick it resolves to. A
	// reference that is absent is unresolvable, which is how a league names
	// somebody who is not on any board we know.
	knownTargets map[string]string
	queueFull    bool
	// delivered is what a poll returns; At doubles as the cursor here, which
	// the real store takes from a record's local arrival number.
	delivered     []PolledDoorEvent
	pollTruncated bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		state:        map[string]string{},
		federated:    map[string]bool{},
		noticed:      map[string]bool{},
		leagues:      map[string]bool{},
		knownTargets: map[string]string{},
	}
}

func stateKey(door, scope, owner, key string) string {
	return door + "\x00" + scope + "\x00" + owner + "\x00" + key
}

func (h *fakeHost) StateGet(_ context.Context, door, scope, owner, key string) (string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.state[stateKey(door, scope, owner, key)]
	return v, ok, nil
}

func (h *fakeHost) StateSet(_ context.Context, door, scope, owner, key, value string, quota int64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if quota > 0 && h.used+int64(len(value)) > quota {
		return errors.New("that door has used up its saved-state allowance")
	}
	h.used += int64(len(value))
	h.state[stateKey(door, scope, owner, key)] = value
	return nil
}

func (h *fakeHost) StateDelete(_ context.Context, door, scope, owner, key string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.state, stateKey(door, scope, owner, key))
	return nil
}

func (h *fakeHost) StateKeys(_ context.Context, door, scope, owner string) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := door + "\x00" + scope + "\x00" + owner + "\x00"
	out := []string{}
	for k := range h.state {
		if strings.HasPrefix(k, prefix) {
			out = append(out, strings.TrimPrefix(k, prefix))
		}
	}
	return out, nil
}

func (h *fakeHost) AreaIsFederated(_ context.Context, area string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.federated[area], nil
}

func (h *fakeHost) Announce(_ context.Context, door, area, subject, text string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.announced = append(h.announced, fmt.Sprintf("%s/%s/%s/%s", door, area, subject, text))
	return "rec-announce", nil
}

func (h *fakeHost) PostAs(_ context.Context, nick, area, subject, text string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.postErr != nil {
		return "", h.postErr
	}
	h.posted = append(h.posted, fmt.Sprintf("%s/%s/%s/%s", nick, area, subject, text))
	return "rec-post", nil
}

func (h *fakeHost) SendDMAs(_ context.Context, nick, to, subject, text string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dms = append(h.dms, fmt.Sprintf("%s->%s/%s/%s", nick, to, subject, text))
	return "rec-dm", nil
}

func (h *fakeHost) NoticeNeeded(_ context.Context, door, nick string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := door + "/" + nick
	if h.noticed[k] {
		return false, nil
	}
	h.noticed[k] = true
	return true, nil
}

func (h *fakeHost) Audit(_ context.Context, actor, action, target, detail string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.audits = append(h.audits, fmt.Sprintf("%s/%s/%s/%s", actor, action, target, detail))
	return nil
}

func (h *fakeHost) snapshot(f func(*fakeHost)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// apiRig is an API server with a client attached, without launching a door.
type apiRig struct {
	host  *fakeHost
	mgr   *Manager
	inv   *invocation
	spec  Spec
	token string
	desc  Descriptor
}

func newAPIRig(t *testing.T, level int, tune func(*Spec, *Session)) *apiRig {
	t.Helper()

	host := newFakeHost()
	mgr := New(realClock(), discardLogger())
	mgr.SetHost(host)

	spec := Spec{
		Name:      "tradewars",
		Path:      mustExecutable(t),
		Dir:       t.TempDir(),
		WallClock: time.Minute,
		Grant: Grant{
			Level:           level,
			AnnounceArea:    "games",
			AnnouncePerHour: 2,
			StateQuota:      1024,
		},
	}
	sess := Session{
		Nick: "alice", RealName: "Alice A", Node: 3,
		Term: "xterm", Width: 80, Height: 24, ANSI: true, Encoding: "cp437",
	}
	if tune != nil {
		tune(&spec, &sess)
	}

	inv, err := mgr.startAPI(&spec, sess)
	if err != nil {
		t.Fatalf("start api: %v", err)
	}
	if inv == nil {
		t.Fatal("no API was started")
	}
	t.Cleanup(func() { inv.close(mgr) })

	// The descriptor is the only place the token exists, which is the property
	// under test elsewhere and the way in here.
	desc := readDescriptor(t, spec)
	return &apiRig{host: host, mgr: mgr, inv: inv, spec: spec, token: desc.Token, desc: desc}
}

func readDescriptor(t *testing.T, spec Spec) Descriptor {
	t.Helper()
	path := envValue(spec.Env, "MESHBBS_DOOR_DESCRIPTOR")
	if path == "" {
		t.Fatal("the door was given no descriptor path")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("descriptor is not valid JSON: %v", err)
	}
	return d
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, key+"="); ok {
			return after
		}
	}
	return ""
}

// client is a door talking to the API.
type client struct {
	conn net.Conn
	r    *bufio.Reader
}

func (rig *apiRig) dial(t *testing.T) *client {
	t.Helper()
	conn, err := dialAPI(rig.desc.Socket)
	if err != nil {
		t.Fatalf("dial %s: %v", rig.desc.Socket, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &client{conn: conn, r: bufio.NewReader(conn)}
}

func (c *client) send(t *testing.T, req request) response {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var res response
	if err := json.Unmarshal(line, &res); err != nil {
		t.Fatalf("response is not valid JSON: %v (%q)", err, line)
	}
	return res
}

// hello performs the handshake and fails the test if it is refused.
func (rig *apiRig) hello(t *testing.T) *client {
	t.Helper()
	c := rig.dial(t)
	res := c.send(t, request{ID: 1, Op: opHello, Token: rig.token})
	if !res.OK {
		t.Fatalf("hello refused: %s", res.Error)
	}
	return c
}

func mustExecutable(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return self
}

// ---------------------------------------------------------------------------
// the handshake
// ---------------------------------------------------------------------------

// The token is the only thing standing between another local process and the
// socket, so a wrong one buys nothing and does not get a second try.
func TestAPIRefusesAWrongToken(t *testing.T) {
	rig := newAPIRig(t, 4, nil)

	c := rig.dial(t)
	res := c.send(t, request{ID: 1, Op: opHello, Token: "not-the-token"})
	if res.OK {
		t.Fatal("a wrong token was accepted")
	}
	if res.Code != codeForbidden {
		t.Errorf("code %q, want %q", res.Code, codeForbidden)
	}

	// The connection is finished; guessing is not a thing you get to do in a
	// loop on one connection.
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.r.ReadBytes('\n'); err == nil {
		t.Error("the connection stayed open after a bad token")
	}
}

// Nothing works before hello, however valid the request would otherwise be.
func TestAPIRefusesWorkBeforeHello(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.dial(t)

	res := c.send(t, request{ID: 1, Op: opSession})
	if res.OK {
		t.Fatal("session.get was served before hello")
	}
	if res.Code != codeForbidden {
		t.Errorf("code %q, want %q", res.Code, codeForbidden)
	}
}

// A door learns its level from hello rather than by trying things and being
// refused.
func TestAPIHelloReportsTheGrantedLevel(t *testing.T) {
	rig := newAPIRig(t, 2, nil)
	c := rig.dial(t)
	res := c.send(t, request{ID: 7, Op: opHello, Token: rig.token})
	if !res.OK || res.Level != 2 {
		t.Errorf("hello returned ok=%v level=%d, want true and 2", res.OK, res.Level)
	}
	if res.ID != 7 {
		t.Errorf("response id %d, want 7", res.ID)
	}
}

// ---------------------------------------------------------------------------
// levels
// ---------------------------------------------------------------------------

// §9.1.1: levels nest, and a door may not reach past the one it was granted.
func TestAPIEnforcesTheGrantedLevel(t *testing.T) {
	ops := []struct {
		op   string
		need int
		req  request
	}{
		{opSession, 1, request{Op: opSession}},
		{opStateSet, 2, request{Op: opStateSet, Key: "k", Value: "v"}},
		{opAnnounce, 3, request{Op: opAnnounce, Text: "hello"}},
		{opUserPost, 4, request{Op: opUserPost, Area: "general", Text: "hi"}},
	}

	for granted := 1; granted <= 4; granted++ {
		t.Run(fmt.Sprintf("level%d", granted), func(t *testing.T) {
			rig := newAPIRig(t, granted, nil)
			c := rig.hello(t)
			for i, tc := range ops {
				req := tc.req
				req.ID = i + 10
				res := c.send(t, req)
				allowed := granted >= tc.need
				if allowed && !res.OK {
					t.Errorf("%s at level %d was refused: %s", tc.op, granted, res.Error)
				}
				if !allowed {
					if res.OK {
						t.Errorf("%s was served at level %d, and needs %d",
							tc.op, granted, tc.need)
					} else if res.Code != codeForbidden {
						t.Errorf("%s refused with code %q, want %q",
							tc.op, res.Code, codeForbidden)
					}
				}
			}
		})
	}
}

func TestAPIRejectsAnUnknownOperation(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)
	res := c.send(t, request{ID: 2, Op: "state.drop_everything"})
	if res.OK {
		t.Fatal("an unknown operation was served")
	}
	// Unknown, not forbidden: a door that mistyped an op should not be told it
	// needs a higher level, which is the wrong thing to go and ask the sysop for.
	if res.Code != codeBadRequest {
		t.Errorf("code %q, want %q", res.Code, codeBadRequest)
	}
}

// ---------------------------------------------------------------------------
// level 1
// ---------------------------------------------------------------------------

func TestAPISessionReportsContext(t *testing.T) {
	rig := newAPIRig(t, 1, nil)
	c := rig.hello(t)
	res := c.send(t, request{ID: 1, Op: opSession})
	if !res.OK || res.Session == nil {
		t.Fatalf("session.get returned ok=%v session=%v", res.OK, res.Session)
	}
	s := *res.Session
	if s.Handle != "alice" || s.Node != 3 || s.Width != 80 || s.Height != 24 {
		t.Errorf("session context is %+v", s)
	}
	if !s.ANSI || s.Encoding != "cp437" || s.Terminal != "xterm" {
		t.Errorf("terminal capability is %+v", s)
	}
	// No limit configured, so the door is told that explicitly rather than
	// being handed a zero it would read as "no time left".
	if s.TimeLimited {
		t.Errorf("time_limited is true with no limit configured")
	}
}

// A door asking how long it has left wants the answer now: the whole reason it
// asks is that the number moves.
func TestAPISessionTimeRemainingIsLive(t *testing.T) {
	var left = 600 * time.Second
	var mu sync.Mutex
	rig := newAPIRig(t, 1, func(_ *Spec, s *Session) {
		s.TimeRemaining = func() time.Duration {
			mu.Lock()
			defer mu.Unlock()
			return left
		}
	})
	c := rig.hello(t)

	res := c.send(t, request{ID: 1, Op: opSession})
	if !res.Session.TimeLimited || res.Session.TimeRemainingSecs != 600 {
		t.Fatalf("first read: limited=%v secs=%d",
			res.Session.TimeLimited, res.Session.TimeRemainingSecs)
	}

	mu.Lock()
	left = 30 * time.Second
	mu.Unlock()

	res = c.send(t, request{ID: 2, Op: opSession})
	if res.Session.TimeRemainingSecs != 30 {
		t.Errorf("second read is %d seconds; the descriptor's copy was served",
			res.Session.TimeRemainingSecs)
	}
}

// ---------------------------------------------------------------------------
// level 2
// ---------------------------------------------------------------------------

func TestAPIStateRoundTrips(t *testing.T) {
	rig := newAPIRig(t, 2, nil)
	c := rig.hello(t)

	if res := c.send(t, request{ID: 1, Op: opStateGet, Key: "save"}); !res.OK || res.Found {
		t.Errorf("first read returned ok=%v found=%v", res.OK, res.Found)
	}
	if res := c.send(t, request{ID: 2, Op: opStateSet, Key: "save", Value: "sector=42"}); !res.OK {
		t.Fatalf("set: %s", res.Error)
	}
	res := c.send(t, request{ID: 3, Op: opStateGet, Key: "save"})
	if !res.OK || !res.Found || res.Value != "sector=42" {
		t.Errorf("read back ok=%v found=%v value=%q", res.OK, res.Found, res.Value)
	}
	if res := c.send(t, request{ID: 4, Op: opStateKeys}); !res.OK || len(res.Keys) != 1 {
		t.Errorf("keys are %v", res.Keys)
	}
	if res := c.send(t, request{ID: 5, Op: opStateDelete, Key: "save"}); !res.OK {
		t.Errorf("delete: %s", res.Error)
	}
}

// The one security property of level 2: a door cannot name whose state it
// wants. The owner comes from the session, so there is no field to put another
// player's nick in.
func TestAPIStateCannotNameAnotherUser(t *testing.T) {
	rig := newAPIRig(t, 2, nil)
	c := rig.hello(t)

	// The request type has no owner field at all, so the closest a door can get
	// is an invented scope — which is refused rather than defaulted.
	res := c.send(t, request{ID: 1, Op: opStateSet, Scope: "bob", Key: "k", Value: "v"})
	if res.OK {
		t.Fatal("an invented scope was accepted")
	}
	if res.Code != codeBadRequest {
		t.Errorf("code %q, want %q", res.Code, codeBadRequest)
	}

	// And what a door does write lands under the session's own nick.
	if res := c.send(t, request{ID: 2, Op: opStateSet, Key: "k", Value: "v"}); !res.OK {
		t.Fatalf("set: %s", res.Error)
	}
	rig.host.snapshot(func(h *fakeHost) {
		if _, ok := h.state[stateKey("tradewars", scopeUser, "alice", "k")]; !ok {
			t.Errorf("state was not filed under alice: %v", h.state)
		}
	})
}

func TestAPIStateReportsQuotaDistinctly(t *testing.T) {
	rig := newAPIRig(t, 2, func(s *Spec, _ *Session) { s.Grant.StateQuota = 8 })
	c := rig.hello(t)

	res := c.send(t, request{ID: 1, Op: opStateSet, Key: "k", Value: strings.Repeat("x", 64)})
	if res.OK {
		t.Fatal("a write past the quota succeeded")
	}
	// Distinct from forbidden and from internal: a door that is out of room can
	// tidy up and retry, which is not true of either of the others.
	if res.Code != codeQuota {
		t.Errorf("code %q, want %q", res.Code, codeQuota)
	}
}

// A guest has no account, so there is no private namespace to write to — and
// the door is told that rather than having its writes filed under "".
func TestAPIStateRefusesAGuestSession(t *testing.T) {
	rig := newAPIRig(t, 2, func(_ *Spec, s *Session) { s.Nick = "" })
	c := rig.hello(t)
	res := c.send(t, request{ID: 1, Op: opStateSet, Key: "k", Value: "v"})
	if res.OK {
		t.Fatal("a guest session wrote user-scoped state")
	}

	// Global state is still fine: it belongs to the door, not the player.
	if res := c.send(t, request{ID: 2, Op: opStateSet, Scope: scopeGlobal, Key: "k", Value: "v"}); !res.OK {
		t.Errorf("global state was refused for a guest: %s", res.Error)
	}
}

// ---------------------------------------------------------------------------
// level 3
// ---------------------------------------------------------------------------

func TestAPIAnnouncePostsAsTheDoor(t *testing.T) {
	rig := newAPIRig(t, 3, nil)
	c := rig.hello(t)

	res := c.send(t, request{ID: 1, Op: opAnnounce, Subject: "New champion", Text: "alice won"})
	if !res.OK {
		t.Fatalf("announce: %s", res.Error)
	}
	rig.host.snapshot(func(h *fakeHost) {
		if len(h.announced) != 1 {
			t.Fatalf("announcements: %v", h.announced)
		}
		// The door's name, not the user's: §9.1.1 says never as the user.
		if !strings.HasPrefix(h.announced[0], "tradewars/games/") {
			t.Errorf("announced as %q", h.announced[0])
		}
		if len(h.posted) != 0 {
			t.Errorf("announce went through the user's post path: %v", h.posted)
		}
	})
}

// §11.4: a sysop gets wide latitude over their own instance and narrow latitude
// over shared airtime. A third-party binary does not get to spend the mesh's
// budget.
func TestAPIAnnounceRefusesAFederatedArea(t *testing.T) {
	rig := newAPIRig(t, 3, nil)
	rig.host.snapshot(func(h *fakeHost) { h.federated["games"] = true })

	c := rig.hello(t)
	res := c.send(t, request{ID: 1, Op: opAnnounce, Text: "alice won"})
	if res.OK {
		t.Fatal("a door announced into a federated area")
	}
	if res.Code != codeForbidden {
		t.Errorf("code %q, want %q", res.Code, codeForbidden)
	}
	if !strings.Contains(res.Error, "airtime") {
		t.Errorf("the refusal does not explain itself: %q", res.Error)
	}
}

// An unset area is not a rate limit of zero: there is nowhere to post.
func TestAPIAnnounceNeedsAnArea(t *testing.T) {
	rig := newAPIRig(t, 3, func(s *Spec, _ *Session) { s.Grant.AnnounceArea = "" })
	c := rig.hello(t)
	res := c.send(t, request{ID: 1, Op: opAnnounce, Text: "hi"})
	if res.OK {
		t.Fatal("a door with no announce area announced")
	}
	if !strings.Contains(res.Error, "sysop") {
		t.Errorf("the refusal does not say whose decision it is: %q", res.Error)
	}
}

func TestAPIAnnounceIsRateLimited(t *testing.T) {
	rig := newAPIRig(t, 3, nil) // two per hour
	c := rig.hello(t)

	for i := range 2 {
		if res := c.send(t, request{ID: i, Op: opAnnounce, Text: "spam"}); !res.OK {
			t.Fatalf("announcement %d was refused: %s", i, res.Error)
		}
	}
	res := c.send(t, request{ID: 3, Op: opAnnounce, Text: "spam"})
	if res.OK {
		t.Fatal("the rate limit did not apply")
	}
	if res.Code != codeRateLimit {
		t.Errorf("code %q, want %q", res.Code, codeRateLimit)
	}
}

// The limit is per DOOR, so relaunching is not a way around it.
func TestAPIAnnounceLimitSurvivesARelaunch(t *testing.T) {
	first := newAPIRig(t, 3, nil)
	c := first.hello(t)
	for i := range 2 {
		if res := c.send(t, request{ID: i, Op: opAnnounce, Text: "x"}); !res.OK {
			t.Fatalf("announcement %d: %s", i, res.Error)
		}
	}

	// A second invocation of the same door on the same Manager.
	spec := first.spec
	spec.Env = nil
	inv, err := first.mgr.startAPI(&spec, Session{Nick: "bob", Node: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.close(first.mgr)

	desc := readDescriptor(t, spec)
	conn, err := dialAPI(desc.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	second := &client{conn: conn, r: bufio.NewReader(conn)}
	if res := second.send(t, request{ID: 1, Op: opHello, Token: desc.Token}); !res.OK {
		t.Fatalf("hello: %s", res.Error)
	}
	if res := second.send(t, request{ID: 2, Op: opAnnounce, Text: "x"}); res.OK {
		t.Error("relaunching the door reset its announce allowance")
	}
}

// ---------------------------------------------------------------------------
// level 4
// ---------------------------------------------------------------------------

func TestAPIActAsUserGoesThroughTheUsersOwnPath(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	res := c.send(t, request{ID: 1, Op: opUserPost, Area: "general",
		Subject: "I won", Text: "against all odds"})
	if !res.OK {
		t.Fatalf("user.post: %s", res.Error)
	}
	rig.host.snapshot(func(h *fakeHost) {
		if len(h.posted) != 1 || !strings.HasPrefix(h.posted[0], "alice/general/") {
			t.Errorf("posted as %v", h.posted)
		}
	})
}

// §9.1.1: capabilities intersect, never escalate. The door does not get a
// second opinion when the user's own path says no — and it is told that as a
// refusal rather than a fault, so it does not retry forever.
func TestAPIActAsUserCannotEscalatePastTheUser(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	refusal := errors.New("general is a federated area, and posting there spends " +
		"the mesh network's shared airtime")
	rig.host.snapshot(func(h *fakeHost) { h.postErr = refusal })

	c := rig.hello(t)
	res := c.send(t, request{ID: 1, Op: opUserPost, Area: "general", Text: "hi"})
	if res.OK {
		t.Fatal("the door posted despite the user's own path refusing")
	}
	if res.Code != codeForbidden {
		t.Errorf("code %q, want %q — a refusal is not a fault", res.Code, codeForbidden)
	}
	if !strings.Contains(res.Error, "airtime") {
		t.Errorf("the user's own reason was lost: %q", res.Error)
	}
}

// Every level-4 action is audited with the door, the user and the record.
func TestAPIActAsUserIsAudited(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	if res := c.send(t, request{ID: 1, Op: opUserPost, Area: "general", Text: "hi"}); !res.OK {
		t.Fatal(res.Error)
	}
	if res := c.send(t, request{ID: 2, Op: opUserDM, To: "bob", Text: "hi"}); !res.OK {
		t.Fatal(res.Error)
	}

	rig.host.snapshot(func(h *fakeHost) {
		if len(h.audits) != 2 {
			t.Fatalf("audit rows: %v", h.audits)
		}
		for _, a := range h.audits {
			if !strings.HasPrefix(a, "alice/") {
				t.Errorf("audit row does not name the user: %q", a)
			}
			if !strings.Contains(a, "door=tradewars") {
				t.Errorf("audit row does not name the door: %q", a)
			}
			if !strings.Contains(a, "record=rec-") {
				t.Errorf("audit row does not name the record: %q", a)
			}
		}
	})
}

// A refusal must not be audited as an action: an audit log that records things
// that did not happen is worse than none, because it cannot be trusted to
// answer the question it exists for.
func TestAPIRefusedActionsAreNotAudited(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	rig.host.snapshot(func(h *fakeHost) { h.postErr = errors.New("no") })

	c := rig.hello(t)
	if res := c.send(t, request{ID: 1, Op: opUserPost, Area: "general", Text: "hi"}); res.OK {
		t.Fatal("the post succeeded")
	}
	rig.host.snapshot(func(h *fakeHost) {
		if len(h.audits) != 0 {
			t.Errorf("a refused action was audited: %v", h.audits)
		}
	})
}

// §9.1.1: the user is told, once, the first time a door acts as them.
func TestAPIActAsUserNoticeIsShownOnce(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	first := c.send(t, request{ID: 1, Op: opUserPost, Area: "general", Text: "one"})
	if !first.OK || first.Notice == "" {
		t.Fatalf("first post returned ok=%v notice=%q", first.OK, first.Notice)
	}
	if !strings.Contains(first.Notice, "tradewars") || !strings.Contains(first.Notice, "alice") {
		t.Errorf("the notice names neither the door nor the user: %q", first.Notice)
	}

	second := c.send(t, request{ID: 2, Op: opUserPost, Area: "general", Text: "two"})
	if second.Notice != "" {
		t.Errorf("the notice was repeated: %q", second.Notice)
	}
}

// ---------------------------------------------------------------------------
// the descriptor
// ---------------------------------------------------------------------------

// §9.1.1: never in argv or the environment, both of which are readable by any
// other process on the box.
func TestTokenIsNotInTheEnvironment(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	if rig.token == "" {
		t.Fatal("no token was minted")
	}
	for _, kv := range rig.spec.Env {
		if strings.Contains(kv, rig.token) {
			t.Errorf("the token is in the environment: %q", kv)
		}
	}
	for _, a := range rig.spec.Args {
		if strings.Contains(a, rig.token) {
			t.Errorf("the token is in argv: %q", a)
		}
	}
}

func TestDescriptorIsPrivate(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	path := envValue(rig.spec.Env, "MESHBBS_DOOR_DESCRIPTOR")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 && !isWindows() {
		t.Errorf("the descriptor is mode %04o; it must not be readable by others", perm)
	}

	dirInfo, err := os.Stat(rig.inv.dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 && !isWindows() {
		t.Errorf("the door's directory is mode %04o", perm)
	}
}

// The token dies with the invocation: §9.1.1 wants no stale-credential case to
// reason about, which is achieved by there being no credential left.
func TestClosingAnInvocationRevokesEverything(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	path := envValue(rig.spec.Env, "MESHBBS_DOOR_DESCRIPTOR")
	socket := rig.desc.Socket

	rig.hello(t) // it works before
	rig.inv.close(rig.mgr)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the descriptor survived the invocation: %v", err)
	}
	if _, err := os.Stat(rig.inv.dir); !os.IsNotExist(err) {
		t.Errorf("the door's directory survived the invocation: %v", err)
	}
	if conn, err := dialAPI(socket); err == nil {
		conn.Close()
		t.Error("the socket still answers after the invocation ended")
	}
}

// The environment carries the context a shell door would rather read than
// parse (§9.1).
func TestDescriptorEnvironmentCarriesTheContext(t *testing.T) {
	rig := newAPIRig(t, 3, nil)
	want := map[string]string{
		"MESHBBS_DOOR":           "tradewars",
		"MESHBBS_USER":           "alice",
		"MESHBBS_NODE":           "3",
		"MESHBBS_COLUMNS":        "80",
		"MESHBBS_LINES":          "24",
		"MESHBBS_DOOR_API_LEVEL": "3",
	}
	for k, v := range want {
		if got := envValue(rig.spec.Env, k); got != v {
			t.Errorf("%s is %q, want %q", k, got, v)
		}
	}
}

// A Manager with no Host runs doors without an API at all: §9.1 calls the
// socket optional and means it.
func TestNoHostMeansNoAPI(t *testing.T) {
	mgr := New(realClock(), discardLogger())
	spec := Spec{Name: "plain", Grant: Grant{Level: 4}}
	inv, err := mgr.startAPI(&spec, Session{Nick: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if inv != nil {
		t.Error("an API was started with no host configured")
	}
	if len(spec.Env) != 0 {
		t.Errorf("the door was given API environment with no host: %v", spec.Env)
	}
}

// ---------------------------------------------------------------------------
// the parser
// ---------------------------------------------------------------------------

// A hostile door is on the other end of this socket (§12.5), so malformed input
// gets an answer rather than a crash or a closed connection.
func TestAPISurvivesMalformedInput(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	for _, junk := range []string{
		`not json at all`,
		`{"op":`,
		`[]`,
		`{"op": 5}`,
		`null`,
		`{"op":"state.set","key":"` + strings.Repeat("k", 200) + `"}`,
	} {
		if _, err := c.conn.Write([]byte(junk + "\n")); err != nil {
			t.Fatalf("write %q: %v", junk, err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("the connection died on %q: %v", junk, err)
		}
		var res response
		if err := json.Unmarshal(line, &res); err != nil {
			t.Fatalf("reply to %q is not JSON: %q", junk, line)
		}
	}

	// And it is still serving afterwards.
	if res := c.send(t, request{ID: 99, Op: opSession}); !res.OK {
		t.Errorf("the API stopped working after malformed input: %s", res.Error)
	}
}

// An over-long line is refused as a request, not treated as the door hanging up.
func TestAPIRefusesAnOversizedRequest(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	huge := fmt.Sprintf(`{"id":1,"op":"state.set","key":"k","value":%q}`,
		strings.Repeat("x", maxRequestLine+1024))
	if _, err := c.conn.Write([]byte(huge + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The connection ends rather than the server buffering without bound.
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.r.ReadBytes('\n'); err == nil {
		t.Error("an oversized request was served")
	}
}

// A door that connects and never speaks must not hold a goroutine for the whole
// game — and the timeout has to run on the injected clock, not on a wall-clock
// deadline handed to the socket.
//
// The distinction is not academic. Under a Virtual clock, "now plus thirty
// seconds" is an instant years in the past, so a socket deadline would
// disconnect every door in the simulator before it could say hello. This test
// runs on a virtual clock precisely so that a return to SetReadDeadline fails
// it: nothing here advances time, so a real deadline would fire at once.
func TestAPISilentDoorIsDisconnectedOnTheInjectedClock(t *testing.T) {
	clk := clock.NewVirtual(time.Unix(1_700_000_000, 0))
	host := newFakeHost()
	mgr := New(clk, discardLogger())
	mgr.SetHost(host)

	spec := Spec{Name: "quiet", Path: mustExecutable(t), Dir: t.TempDir(),
		WallClock: time.Minute, Grant: Grant{Level: 1}}
	inv, err := mgr.startAPI(&spec, Session{Nick: "alice", Node: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer inv.close(mgr)

	desc := readDescriptor(t, spec)
	conn, err := dialAPI(desc.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Say nothing. The connection stays up while virtual time does not move,
	// which is what a wall-clock deadline would get wrong.
	closed := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(conn).ReadBytes('\n')
		closed <- err
	}()

	select {
	case err := <-closed:
		t.Fatalf("the connection was dropped before virtual time moved: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	clk.Advance(2 * helloTimeout)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Error("a silent door was never disconnected")
	}
}

// The client, against the real server.
//
// It is the reference doors' only way in, so a break here breaks them — and it
// is what a Go door author will copy, so it is worth exercising rather than
// trusting.
func TestClientSpeaksTheProtocol(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	t.Setenv("MESHBBS_DOOR_DESCRIPTOR", envValue(rig.spec.Env, "MESHBBS_DOOR_DESCRIPTOR"))

	c, err := Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	if c.Level() != 4 {
		t.Errorf("client reports level %d, want 4", c.Level())
	}

	sess, err := c.Session()
	if err != nil || sess.Handle != "alice" || sess.Node != 3 {
		t.Errorf("session is %+v (err %v)", sess, err)
	}

	if err := c.StateSet(ScopeMine, "save", "sector=42"); err != nil {
		t.Fatalf("state set: %v", err)
	}
	v, ok, err := c.StateGet(ScopeMine, "save")
	if err != nil || !ok || v != "sector=42" {
		t.Errorf("state get returned %q ok=%v err=%v", v, ok, err)
	}
	keys, err := c.StateKeys(ScopeMine)
	if err != nil || len(keys) != 1 {
		t.Errorf("state keys are %v (err %v)", keys, err)
	}
	if err := c.StateDelete(ScopeMine, "save"); err != nil {
		t.Errorf("state delete: %v", err)
	}

	if _, err := c.Announce("Champion", "alice won"); err != nil {
		t.Errorf("announce: %v", err)
	}

	// The notice arrives on the first act-as-user call and not the second.
	_, notice, err := c.PostAs("general", "hi", "from a door")
	if err != nil {
		t.Fatalf("post as user: %v", err)
	}
	if notice == "" {
		t.Error("the first post as the user carried no notice")
	}
	if _, again, _ := c.PostAs("general", "hi", "again"); again != "" {
		t.Errorf("the notice was repeated: %q", again)
	}
}

// The league half of the client, against the real server (§9.5).
//
// The reference arena door is written against exactly these two calls, so a
// break here is a league that silently stops reporting — and the encoding is
// the part worth pinning: a payload is bytes to the door, base64 on the wire,
// and bytes again at the far end. A door author who has to know that has been
// handed the protocol rather than a client.
func TestClientReportsAndReadsALeague(t *testing.T) {
	rig := leagueRig(t, nil)
	t.Setenv("MESHBBS_DOOR_DESCRIPTOR", envValue(rig.spec.Env, "MESHBBS_DOOR_DESCRIPTOR"))

	c, err := Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Close()

	queued, notice, err := c.EmitEvent("lord", 3, "bob@pnw", []byte{9, 9})
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !queued {
		t.Error("the client reported the event was not queued")
	}
	if notice == "" {
		// The one-time notice rides on the emit response for the same reason it
		// rides on a level-4 post: a door putting a player's nick on other
		// people's mesh has to say so, and a client that dropped it would make
		// that impossible to honour.
		t.Error("the first emit carried no notice for the client to show")
	}

	rig.host.snapshot(func(h *fakeHost) {
		if len(h.events) != 1 {
			t.Fatalf("the host queued %d events, want 1", len(h.events))
		}
		ev := h.events[0]
		if ev.Actor != "alice" {
			t.Errorf("the event was attributed to %q, want the session's nick", ev.Actor)
		}
		if ev.Target != "bob" {
			t.Errorf("the target resolved to %q, want bob", ev.Target)
		}
		if ev.Kind != 3 || len(ev.Payload) != 2 || ev.Payload[0] != 9 {
			t.Errorf("the event arrived at the host as %+v", ev)
		}
	})

	rig.host.snapshot(func(h *fakeHost) {
		h.delivered = []PolledDoorEvent{
			{Origin: "K7QM4X2P", At: 4, Kind: 1, Actor: "alice", Payload: "AwE="},
			{Origin: "K7QM4X2P", At: 9, Kind: 2, Actor: "bob"},
		}
	})

	batch, err := c.PollEvents("lord", 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(batch.Events) != 2 {
		t.Fatalf("poll returned %d events, want 2", len(batch.Events))
	}
	if batch.Cursor != 9 {
		t.Errorf("poll returned cursor %d, want the last event's, 9", batch.Cursor)
	}
	payload, err := batch.Events[0].PayloadBytes()
	if err != nil {
		t.Fatalf("decoding a payload: %v", err)
	}
	if len(payload) != 2 || payload[0] != 3 || payload[1] != 1 {
		t.Errorf("a payload decoded to %v, want [3 1]", payload)
	}
	// An absent payload decodes to nothing rather than to an error: absent and
	// empty are the same single zero length on the wire, and a door that treated
	// "no payload" as a fault would refuse to show half a league.
	if p, err := batch.Events[1].PayloadBytes(); err != nil || len(p) != 0 {
		t.Errorf("an empty payload decoded to %v (err %v)", p, err)
	}

	// Reading from the returned cursor shows nothing twice, which is what makes
	// the door's saved cursor worth saving.
	next, err := c.PollEvents("lord", batch.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 0 {
		t.Errorf("polling from the returned cursor replayed %d events", len(next.Events))
	}

	// And truncation reaches the client, or a door draws an incomplete league
	// table and calls it complete.
	rig.host.snapshot(func(h *fakeHost) { h.pollTruncated = true })
	pruned, err := c.PollEvents("lord", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !pruned.Truncated {
		t.Error("the client did not surface that events had been pruned before it read them")
	}
}

// A door with no league area gets an answer it can act on, not a fault.
//
// The distinction matters more here than for announce: a board that is not in a
// league is the NORMAL case — most boards are not — so an arena door has to be
// able to say "this board is not in a league" and carry on rather than crash in
// front of the player.
func TestClientLeagueRefusalIsAnAnswer(t *testing.T) {
	rig := newAPIRig(t, 3, nil) // level 3, and no league area
	t.Setenv("MESHBBS_DOOR_DESCRIPTOR", envValue(rig.spec.Env, "MESHBBS_DOOR_DESCRIPTOR"))

	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var apiErr *APIError
	if _, _, err := c.EmitEvent("lord", 1, "", nil); !errors.As(err, &apiErr) {
		t.Fatalf("emit without a league returned %T: %v", err, err)
	} else if !apiErr.Forbidden() {
		t.Errorf("the refusal is not marked forbidden: %+v", apiErr)
	}
	if _, err := c.PollEvents("lord", 0); !errors.As(err, &apiErr) {
		t.Fatalf("poll without a league returned %T: %v", err, err)
	} else if !apiErr.Forbidden() {
		t.Errorf("the poll refusal is not marked forbidden: %+v", apiErr)
	}
}

// A refusal and a fault must be distinguishable, or a door retries the one
// thing that will never succeed.
func TestClientDistinguishesRefusalFromFailure(t *testing.T) {
	rig := newAPIRig(t, 2, nil) // level 2: announce is above its grant
	t.Setenv("MESHBBS_DOOR_DESCRIPTOR", envValue(rig.spec.Env, "MESHBBS_DOOR_DESCRIPTOR"))

	c, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	_, err = c.Announce("nope", "text")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("announce above the grant returned %T: %v", err, err)
	}
	if !apiErr.Forbidden() {
		t.Errorf("the refusal is not marked forbidden: %+v", apiErr)
	}

	// And a dead socket is a fault, not a refusal.
	rig.inv.close(rig.mgr)
	if _, err := c.Session(); err == nil {
		t.Error("a closed connection reported success")
	} else if errors.As(err, &apiErr) {
		t.Errorf("a transport failure was reported as a refusal: %+v", apiErr)
	}
}

// A door started outside meshbbs says so plainly, because it is the first
// thing a door author hits and "connection refused" points the wrong way.
func TestClientWithoutADescriptor(t *testing.T) {
	t.Setenv("MESHBBS_DOOR_DESCRIPTOR", "")
	if _, err := Open(); !errors.Is(err, ErrNoDescriptor) {
		t.Errorf("Open returned %v, want %v", err, ErrNoDescriptor)
	}
}

func (h *fakeHost) QueueDoorEvent(ctx context.Context, ev DoorEventRequest) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.leagues[ev.Area] {
		return fmt.Errorf("%w: %s", ErrNotALeague, ev.Area)
	}
	if ev.Target != "" {
		nick, ok := h.knownTargets[ev.Target]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnknownTarget, ev.Target)
		}
		ev.Target = nick
	}
	if len(ev.Payload) > 48 {
		return fmt.Errorf("%w: payload is %d bytes", ErrInvalidEvent, len(ev.Payload))
	}
	if h.queueFull {
		return errors.New("this door has more queued events than the mesh can carry: 100 waiting")
	}
	h.events = append(h.events, ev)
	return nil
}

func (h *fakeHost) PollDoorEvents(ctx context.Context, p DoorEventPoll) (DoorEventBatch, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.leagues[p.Area] {
		return DoorEventBatch{}, fmt.Errorf("%w: %s", ErrNotALeague, p.Area)
	}
	out := DoorEventBatch{Cursor: p.After, Truncated: h.pollTruncated}
	for _, ev := range h.delivered {
		if ev.At <= p.After {
			continue
		}
		out.Cursor = ev.At
		out.Events = append(out.Events, ev)
	}
	return out, nil
}
