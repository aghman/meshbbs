package door

import (
	"bufio"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// A door is a third-party binary the sysop installed, running with the server's
// privileges on behalf of a remote user (§9.4). §12.5 asks for a malicious-peer
// fixture for the mesh, "a node that replays records, forges signatures … and
// every §8.4 defence should have a test in which this fixture attacks it and
// loses". This is the same idea for the other untrusted party.
//
// Each test here is an attack that should fail. What is NOT here matters as
// much: a door writing outside its working directory is not attacked, because
// it is not defended — cwd is set, not confined, and design.md §9.4 says so.
// Inventing a test for a boundary that does not exist would be worse than
// having none, because a green suite would imply one.

// §9.1.1 scopes a token to (user, door, session, invocation) and invalidates it
// when the door exits. So a door that keeps its token and comes back must find
// it worth nothing — including against a LATER invocation of the same door,
// which is the case a socket-is-gone test does not cover.
func TestAStolenTokenIsWorthlessOnTheNextInvocation(t *testing.T) {
	host := newFakeHost()
	mgr := New(realClock(), discardLogger())
	mgr.SetHost(host)

	newInvocation := func() (Descriptor, func()) {
		t.Helper()
		spec := Spec{
			Name: "thief", Path: mustExecutable(t), Dir: t.TempDir(),
			WallClock: time.Minute, Grant: Grant{Level: 4, StateQuota: 1024},
		}
		inv, err := mgr.startAPI(&spec, Session{Nick: "alice", Node: 1})
		if err != nil {
			t.Fatal(err)
		}
		return readDescriptor(t, spec), func() { inv.close(mgr) }
	}

	first, closeFirst := newInvocation()
	stolen := first.Token
	closeFirst()

	second, closeSecond := newInvocation()
	defer closeSecond()

	if stolen == second.Token {
		t.Fatal("two invocations were issued the same token")
	}

	conn, err := dialAPI(second.Socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	c := &client{conn: conn, r: bufio.NewReader(conn)}

	if res := c.send(t, request{ID: 1, Op: opHello, Token: stolen}); res.OK {
		t.Error("a token from a previous invocation opened this one")
	}
}

// §9.1.1: no cross-door reach. The store enforces it and this proves the whole
// path, because the door's name comes from the SPEC and never from the request
// — there is no field an attacking door could put another door's name in.
func TestADoorCannotReachAnotherDoorsState(t *testing.T) {
	host := newFakeHost()
	mgr := New(realClock(), discardLogger())
	mgr.SetHost(host)

	open := func(name string) (*client, func()) {
		t.Helper()
		spec := Spec{
			Name: name, Path: mustExecutable(t), Dir: t.TempDir(),
			WallClock: time.Minute, Grant: Grant{Level: 4, StateQuota: 1024},
		}
		inv, err := mgr.startAPI(&spec, Session{Nick: "alice", Node: 1})
		if err != nil {
			t.Fatal(err)
		}
		desc := readDescriptor(t, spec)
		conn, err := dialAPI(desc.Socket)
		if err != nil {
			t.Fatal(err)
		}
		c := &client{conn: conn, r: bufio.NewReader(conn)}
		if res := c.send(t, request{ID: 1, Op: opHello, Token: desc.Token}); !res.OK {
			t.Fatalf("hello: %s", res.Error)
		}
		return c, func() { conn.Close(); inv.close(mgr) }
	}

	victim, closeVictim := open("victim")
	defer closeVictim()
	attacker, closeAttacker := open("attacker")
	defer closeAttacker()

	if res := victim.send(t, request{ID: 2, Op: opStateSet,
		Key: "secret", Value: "the treasure is in sector 42"}); !res.OK {
		t.Fatalf("victim could not save: %s", res.Error)
	}

	// The same key, the same user, a different door.
	res := attacker.send(t, request{ID: 2, Op: opStateGet, Key: "secret"})
	if !res.OK {
		t.Fatalf("attacker's read failed outright: %s", res.Error)
	}
	if res.Found || res.Value != "" {
		t.Errorf("a door read another door's state: %q", res.Value)
	}
}

// A door talking to the socket from many goroutines at once must not be able to
// wedge it or make it answer somebody else's question.
func TestManyConnectionsAtOnceDoNotWedgeTheAPI(t *testing.T) {
	rig := newAPIRig(t, 4, nil)

	const callers = 24
	var wg sync.WaitGroup
	errs := make(chan string, callers)

	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := dialAPI(rig.desc.Socket)
			if err != nil {
				errs <- "dial: " + err.Error()
				return
			}
			defer conn.Close()
			c := &client{conn: conn, r: bufio.NewReader(conn)}

			if res := c.send(t, request{ID: 1, Op: opHello, Token: rig.token}); !res.OK {
				errs <- "hello: " + res.Error
				return
			}
			// Each connection asks a question whose answer names it, so a
			// reply delivered to the wrong socket would be visible rather than
			// merely suspected.
			key := "k" + string(rune('a'+i))
			if res := c.send(t, request{ID: 2, Op: opStateSet, Key: key, Value: key}); !res.OK {
				errs <- "set: " + res.Error
				return
			}
			res := c.send(t, request{ID: 3, Op: opStateGet, Key: key})
			if !res.OK || res.Value != key {
				errs <- "got " + res.Value + " for " + key
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// And it still serves afterwards.
	c := rig.hello(t)
	if res := c.send(t, request{ID: 9, Op: opSession}); !res.OK {
		t.Errorf("the API stopped serving: %s", res.Error)
	}
}

// A door that never says hello holds a goroutine and nothing else, and the
// server must still shut down promptly rather than waiting for it.
func TestSilentConnectionsDoNotBlockShutdown(t *testing.T) {
	rig := newAPIRig(t, 1, nil)

	for range 8 {
		conn, err := dialAPI(rig.desc.Socket)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer conn.Close() //nolint:revive // deliberately held open
	}

	done := make(chan struct{})
	go func() {
		rig.inv.close(rig.mgr)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutting down waited for doors that never spoke")
	}
}

// A door that floods announce is throttled rather than served, and the throttle
// does not lose count under concurrency — which is when a door would try it.
func TestAnnounceFloodIsThrottled(t *testing.T) {
	const allowed = 3
	rig := newAPIRig(t, 3, func(s *Spec, _ *Session) { s.Grant.AnnouncePerHour = allowed })

	const attempts = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0

	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := dialAPI(rig.desc.Socket)
			if err != nil {
				return
			}
			defer conn.Close()
			c := &client{conn: conn, r: bufio.NewReader(conn)}
			if res := c.send(t, request{ID: 1, Op: opHello, Token: rig.token}); !res.OK {
				return
			}
			if res := c.send(t, request{ID: 2, Op: opAnnounce, Text: "spam"}); res.OK {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if accepted > allowed {
		t.Errorf("%d of %d announcements were accepted against a limit of %d",
			accepted, attempts, allowed)
	}
	rig.host.snapshot(func(h *fakeHost) {
		if len(h.announced) > allowed {
			t.Errorf("%d announcements reached the BBS, limit is %d",
				len(h.announced), allowed)
		}
	})
}

// A door that sends a very large number of requests on one connection must not
// grow the server without bound or slow it to a stop.
func TestASustainedRequestFloodIsSurvived(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	const requests = 5000
	deadline := time.Now().Add(60 * time.Second)
	for i := range requests {
		if time.Now().After(deadline) {
			t.Fatalf("the API served only %d of %d requests in a minute", i, requests)
		}
		if res := c.send(t, request{ID: i, Op: opSession}); !res.OK {
			t.Fatalf("request %d refused: %s", i, res.Error)
		}
	}
}

// A door cannot use a level it was not granted, however it phrases the request.
// The refusal is by OPERATION, so there is no spelling that reaches past it.
func TestNoPhrasingReachesPastTheGrantedLevel(t *testing.T) {
	rig := newAPIRig(t, 1, nil) // session only
	c := rig.hello(t)

	forbidden := []request{
		{Op: opStateSet, Key: "k", Value: "v"},
		{Op: opStateGet, Key: "k"},
		{Op: opAnnounce, Text: "hi"},
		{Op: opUserPost, Area: "general", Text: "hi"},
		{Op: opUserDM, To: "bob", Text: "hi"},
	}
	for i, req := range forbidden {
		req.ID = i
		if res := c.send(t, req); res.OK {
			t.Errorf("%q was served at level 1", req.Op)
		}
	}

	// Case and whitespace are not a way in either. Asserted at level 4, where
	// the operation WOULD be permitted if it were recognised — at level 1 every
	// spelling is refused anyway, so the level check would hide a loose match
	// rather than the match being tested.
	top := newAPIRig(t, 4, nil)
	tc := top.hello(t)
	for i, op := range []string{"STATE.SET", " announce", "user.post ", "State.Get"} {
		res := tc.send(t, request{ID: i, Op: op, Key: "k", Value: "v",
			Area: "general", Text: "hi"})
		if res.OK {
			t.Errorf("%q was served; operations must match exactly", op)
		}
		if res.Code != codeBadRequest {
			t.Errorf("%q was refused as %q, want %q — a misspelling is not a "+
				"missing permission, and telling an author to ask the sysop for "+
				"one sends them the wrong way", op, res.Code, codeBadRequest)
		}
	}
	top.host.snapshot(func(h *fakeHost) {
		if len(h.state) != 0 || len(h.announced) != 0 || len(h.posted) != 0 {
			t.Errorf("a loosely spelled operation reached the BBS")
		}
	})

	rig.host.snapshot(func(h *fakeHost) {
		if len(h.state) != 0 || len(h.announced) != 0 || len(h.posted) != 0 || len(h.dms) != 0 {
			t.Errorf("a level-1 door reached the BBS: state=%d announce=%d post=%d dm=%d",
				len(h.state), len(h.announced), len(h.posted), len(h.dms))
		}
	})
}

// Nothing a door sends can make the server produce something that is not one
// JSON object per line. A door that could split or merge replies could make the
// NEXT door on the same board misread its own answers.
func TestEveryReplyIsOneJSONObject(t *testing.T) {
	rig := newAPIRig(t, 4, nil)
	c := rig.hello(t)

	sends := []string{
		`{"id":1,"op":"session.get"}`,
		`{"id":2,"op":"state.set","key":"a","value":"line\nbreak\r\nhere"}`,
		`{"id":3,"op":"state.get","key":"a"}`,
		`{"id":4,"op":"announce","subject":"a\nb","text":"c\r\nd"}`,
		`{"id":5,"op":"user.post","area":"general","subject":"}","text":"{\"ok\":true}"}`,
	}
	for _, raw := range sends {
		if _, err := c.conn.Write([]byte(raw + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read after %s: %v", raw, err)
		}
		var res response
		if err := json.Unmarshal(line, &res); err != nil {
			t.Errorf("reply to %s is not one JSON object: %q", raw, line)
		}
		// And a value carrying newlines came back whole rather than as two
		// replies, which is what a naive line protocol would do.
		if res.Found && strings.Count(res.Value, "\n") == 0 && res.ID == 3 {
			t.Errorf("a stored newline did not survive the round trip: %q", res.Value)
		}
	}
}
