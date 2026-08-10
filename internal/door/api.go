package door

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// The door API socket (§9.1.1).
//
// This is the one channel through which a door reaches the rest of the BBS, and
// everything about its shape follows from what a door IS: a third-party binary,
// running on the sysop's machine, on behalf of a remote user (§9.4). So the
// question the design answers is not "what would be convenient for a door" but
// "what is the smallest authority that makes inter-BBS leagues possible".
//
// # Four levels, and why three of them need no grant
//
// Levels 1 to 3 are always available because none of them can do anything the
// user could not already do, or reach anything outside the door: reading the
// session you are in, keeping notes under your own name, and speaking as
// yourself in a place the sysop chose. Level 4 is act_as_user, which is what
// makes a league feel native and is also a door impersonating a person, so it
// is granted per door and off by default.
//
// # The rule that keeps level 4 honest
//
// Capabilities INTERSECT, never escalate. A door running as a user who lacks
// post_federated cannot federate on their behalf. This is not enforced by a
// check here — it is enforced by there being no path that skips the user's own
// checks: user.post calls the same Post the menu calls, with the same nick, and
// the capability gate it already contains does the work. A door API that took a
// shortcut past that gate would be a privilege-escalation path around §6.7, and
// the way to make sure it never grows one is to have no shortcut to take.

// Host is what the door API can reach into the BBS for. It is deliberately
// small: this list IS the authority a door has, so anything added here widens
// what every installed door can do.
type Host interface {
	// Level 2 — the door's private key/value namespace.
	StateGet(ctx context.Context, door, scope, owner, key string) (string, bool, error)
	StateSet(ctx context.Context, door, scope, owner, key, value string, quota int64) error
	StateDelete(ctx context.Context, door, scope, owner, key string) error
	StateKeys(ctx context.Context, door, scope, owner string) ([]string, error)

	// AreaIsFederated reports whether posting to an area spends mesh airtime.
	AreaIsFederated(ctx context.Context, area string) (bool, error)

	// Level 3 — post as the DOOR's own identity, never as the user.
	Announce(ctx context.Context, door, area, subject, text string) (string, error)

	// Level 4 — act as the user. These must be the SAME calls the menu makes,
	// so that the user's own capability checks apply unchanged.
	PostAs(ctx context.Context, nick, area, subject, text string) (string, error)
	SendDMAs(ctx context.Context, nick, to, subject, text string) (string, error)

	// NoticeNeeded reports whether this user still has to be told that this
	// door can act as them, and records that they have been.
	NoticeNeeded(ctx context.Context, door, nick string) (bool, error)

	// Audit records an action taken under level 4.
	Audit(ctx context.Context, actor, action, target, detail string) error

	// QueueDoorEvent records one game event for the league's next batch (§9.5).
	//
	// One method rather than three — "is this a league", "who is bob@pnw", and
	// "write it down" — because this interface IS the authority a door has, and
	// three entries would widen that list to say a door may interrogate the
	// area table and resolve arbitrary aliases. It may do neither. It may
	// report an event, and the host decides whether that was possible.
	//
	// The typed errors below are how the caller tells a refusal from a fault
	// without parsing prose.
	QueueDoorEvent(ctx context.Context, ev DoorEventRequest) error
}

// DoorEventRequest is one event a door wants on its league (§9.5).
type DoorEventRequest struct {
	Door  string
	Area  string
	Game  string
	Kind  uint8
	Actor string
	// Target may be empty, or a nick, or nick@node in any form
	// ResolveRecipient accepts. The host resolves it; the door never learns a
	// node ID it did not already have.
	Target  string
	Payload []byte
}

// Errors a host returns from QueueDoorEvent, so the API can answer with the
// right code rather than turning every refusal into an internal error.
var (
	// ErrNotALeague means the configured area is not a federated door area.
	ErrNotALeague = errors.New("not a federated door league")
	// ErrUnknownTarget means the target could not be resolved to a user.
	ErrUnknownTarget = errors.New("unknown target")
	// ErrInvalidEvent means the event would not encode (record.DoorEvent's
	// bounds). A door's mistake, not the BBS's.
	ErrInvalidEvent = errors.New("invalid door event")
)

// Grant is the per-door half of §9.1.1: what the sysop allowed.
type Grant struct {
	// Level is the highest capability level this door may use.
	Level int
	// AnnounceArea is where level 3 posts go. Empty means the door may not
	// announce, which is a different thing from a rate limit of zero.
	AnnounceArea string
	// AnnouncePerHour bounds level 3.
	AnnouncePerHour int
	// LeagueArea is the federated door area this door reports game events to
	// (§9.5). Empty means it may not emit, which is a different thing from a
	// rate limit of zero — the same distinction AnnounceArea draws.
	//
	// The mirror image of AnnounceArea, deliberately: announce requires a
	// LOCAL area because a door must not spend the mesh's airtime on its own
	// say-so, and this requires a FEDERATED one because spending it is the
	// feature. §11.4's asymmetry, visible in the grant.
	LeagueArea string
	// LeaguePerHour bounds how often this door may emit.
	LeaguePerHour int
	// StateQuota bounds level 2, in bytes across the whole door.
	StateQuota int64
}

// Error codes a door can branch on, so that "you may not" and "that broke" are
// distinguishable without parsing prose.
const (
	codeBadRequest = "bad_request"
	codeForbidden  = "forbidden"
	codeRateLimit  = "rate_limited"
	codeQuota      = "quota"
	codeInternal   = "internal"
)

// maxRequestLine bounds one request. The largest legitimate one is a post,
// which the BBS itself limits well below this.
const maxRequestLine = 64 * 1024

// tokenBytes is the entropy behind a callback token.
//
// crypto/rand, never the seeded rng.Source domain logic uses (§12.1). A token
// is a credential: one derived from a simulator seed would be predictable to
// anyone who could guess the seed, which is the entire point of the seed.
const tokenBytes = 32

// apiServer serves one invocation's socket.
//
// One per invocation, not one per session and not one shared: §9.1.1 scopes a
// token to (user, door, session, invocation) and invalidates it when the door
// exits. A shared listener would have to carry that scoping in every request
// and get it right; a listener that dies with the invocation cannot get it
// wrong.
type apiServer struct {
	mgr   *Manager
	host  Host
	spec  Spec
	sess  Session
	token string

	ln       net.Listener
	wg       sync.WaitGroup
	closing  chan struct{}
	closeOne sync.Once

	// conns is every live door connection.
	//
	// Closing the listener stops new ones and does nothing to those already
	// accepted: their goroutines are parked in a read, waiting for a door that
	// may never speak again. Without this, Close waits forever — and Close runs
	// from Run's teardown, so a single connection left open by something the
	// door forked would wedge the session it was supposed to be handing back.
	connMu sync.Mutex
	conns  map[net.Conn]struct{}
}

// serveAPI starts the socket and returns it. The caller closes it.
func (m *Manager) serveAPI(ln net.Listener, token string, spec Spec, sess Session) *apiServer {
	a := &apiServer{
		mgr: m, host: m.host, spec: spec, sess: sess,
		token: token, ln: ln, closing: make(chan struct{}),
		conns: map[net.Conn]struct{}{},
	}
	a.wg.Add(1)
	go a.accept()
	return a
}

func (a *apiServer) accept() {
	defer a.wg.Done()
	for {
		conn, err := a.ln.Accept()
		if err != nil {
			select {
			case <-a.closing:
			default:
				a.mgr.log.Debug("door api accept", "door", a.spec.Name, "err", err)
			}
			return
		}
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.serveConn(conn)
		}()
	}
}

// Close stops the socket answering and disconnects anything still attached.
//
// Both halves are the revocation §9.1.1 asks for: a token that is invalidated
// when the door exits has to stop working for whoever is holding it, not just
// stop being issued.
func (a *apiServer) Close() error {
	var err error
	a.closeOne.Do(func() {
		close(a.closing)
		err = a.ln.Close()

		a.connMu.Lock()
		for c := range a.conns {
			_ = c.Close()
		}
		a.connMu.Unlock()
	})
	a.wg.Wait()
	return err
}

// track registers a connection, or refuses it because the server is closing.
func (a *apiServer) track(conn net.Conn) bool {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	select {
	case <-a.closing:
		// Accepted in the window between Close closing the listener and this
		// goroutine getting here. Close has already walked the set, so nothing
		// else will ever close this one.
		return false
	default:
	}
	a.conns[conn] = struct{}{}
	return true
}

func (a *apiServer) untrack(conn net.Conn) {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	delete(a.conns, conn)
}

// serveConn runs one door connection.
//
// The token is presented once, in a hello, rather than on every request. The
// socket is already per-invocation and permissioned to the door's account, so
// the token is defence in depth against another local process finding it — and
// defence in depth that has to be re-sent on every line is defence that gets
// logged, cached and copied around.
func (a *apiServer) serveConn(conn net.Conn) {
	defer conn.Close()
	if !a.track(conn) {
		return
	}
	defer a.untrack(conn)

	// A door that connects and says nothing must not hold a goroutine for the
	// whole game.
	//
	// A watchdog rather than SetReadDeadline, because the deadline would have
	// to be a wall-clock instant and the only time this package is allowed to
	// read is the injected clock (§12.1). Handing a Virtual clock's idea of
	// "now plus thirty seconds" to a socket sets a deadline years in the past,
	// and every door under the simulator would be disconnected before it could
	// speak. Closing the connection from a timer keeps one clock in play.
	greeted := make(chan struct{})
	var greetOnce sync.Once
	go func() {
		select {
		case <-greeted:
		case <-a.closing:
		case <-a.mgr.clock.After(helloTimeout):
			_ = conn.Close()
		}
	}()
	defer greetOnce.Do(func() { close(greeted) })

	r := bufio.NewReaderSize(conn, 4096)
	w := bufio.NewWriter(conn)
	hasGreeted := false

	for {
		line, err := readLine(r, maxRequestLine)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				a.mgr.log.Debug("door api read", "door", a.spec.Name, "err", err)
			}
			return
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			a.reply(w, response{OK: false, Code: codeBadRequest,
				Error: "that is not a JSON object this API understands"})
			continue
		}

		if !hasGreeted {
			if req.Op != opHello || !a.tokenMatches(req.Token) {
				// No detail, and no second chance on this connection. Telling a
				// caller whether it failed on the op or the token turns the
				// socket into an oracle for guessing tokens.
				a.reply(w, response{ID: req.ID, OK: false, Code: codeForbidden,
					Error: "say hello with a valid token first"})
				return
			}
			hasGreeted = true
			greetOnce.Do(func() { close(greeted) })
			a.reply(w, response{ID: req.ID, OK: true, Level: a.spec.Grant.Level})
			continue
		}

		a.reply(w, a.dispatch(req))
	}
}

func (a *apiServer) tokenMatches(got string) bool {
	// Constant time, because the comparison is against a secret and an early
	// exit leaks its prefix.
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

func (a *apiServer) reply(w *bufio.Writer, res response) {
	if err := json.NewEncoder(w).Encode(res); err != nil {
		return
	}
	_ = w.Flush()
}

// Operations. Named by level so that reading a door's source says which
// authority it is using.
const (
	opHello       = "hello"
	opSession     = "session.get"
	opStateGet    = "state.get"
	opStateSet    = "state.set"
	opStateDelete = "state.delete"
	opStateKeys   = "state.keys"
	opAnnounce    = "announce"
	opEventEmit   = "event.emit"
	opUserPost    = "user.post"
	opUserDM      = "user.dm"
)

// levelFor says what each operation costs. An operation missing from here is
// unknown, which is a bad request rather than a forbidden one.
var levelFor = map[string]int{
	opSession:     1,
	opStateGet:    2,
	opStateSet:    2,
	opStateDelete: 2,
	opStateKeys:   2,
	opAnnounce:    3,
	// Level 3 plus a league grant, not a level of its own. Levels nest and 4
	// is act_as_user; a fifth above impersonation would claim that reporting a
	// game result is more authority than posting as the user.
	opEventEmit: 3,
	opUserPost:  4,
	opUserDM:    4,
}

func (a *apiServer) dispatch(req request) response {
	need, known := levelFor[req.Op]
	if !known {
		return response{ID: req.ID, OK: false, Code: codeBadRequest,
			Error: fmt.Sprintf("unknown operation %q", req.Op)}
	}
	if a.spec.Grant.Level < need {
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: fmt.Sprintf("%s needs API level %d and this door has %d",
				req.Op, need, a.spec.Grant.Level)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	switch req.Op {
	case opSession:
		info := a.sessionInfo()
		return response{ID: req.ID, OK: true, Session: &info}
	case opStateGet:
		return a.stateGet(ctx, req)
	case opStateSet:
		return a.stateSet(ctx, req)
	case opStateDelete:
		return a.stateDelete(ctx, req)
	case opStateKeys:
		return a.stateKeys(ctx, req)
	case opAnnounce:
		return a.announce(ctx, req)
	case opEventEmit:
		return a.eventEmit(ctx, req)
	case opUserPost:
		return a.userPost(ctx, req)
	case opUserDM:
		return a.userDM(ctx, req)
	}
	return response{ID: req.ID, OK: false, Code: codeInternal, Error: "unreachable"}
}

// ---------------------------------------------------------------------------
// Level 2 — state
// ---------------------------------------------------------------------------

// owner resolves a scope to the row owner, refusing anything that would let a
// door name a user other than the one playing.
//
// The door never sends a nick. That is the whole of "no cross-user reach": if
// the owner came from the request, level 2 would be a way to read any player's
// saved game by asking for it.
func (a *apiServer) owner(scope string) (string, string, error) {
	switch scope {
	case "", scopeUser:
		if a.sess.Nick == "" {
			return "", "", errors.New("this session has no account, so it has no private state")
		}
		return scopeUser, a.sess.Nick, nil
	case scopeGlobal:
		return scopeGlobal, "", nil
	default:
		return "", "", fmt.Errorf("unknown scope %q (want %q or %q)", scope, scopeUser, scopeGlobal)
	}
}

const (
	scopeUser   = "user"
	scopeGlobal = "global"
)

func (a *apiServer) stateGet(ctx context.Context, req request) response {
	scope, owner, err := a.owner(req.Scope)
	if err != nil {
		return badRequest(req, err)
	}
	v, ok, err := a.host.StateGet(ctx, a.spec.Name, scope, owner, req.Key)
	if err != nil {
		return internal(req, err)
	}
	return response{ID: req.ID, OK: true, Value: v, Found: ok}
}

func (a *apiServer) stateSet(ctx context.Context, req request) response {
	scope, owner, err := a.owner(req.Scope)
	if err != nil {
		return badRequest(req, err)
	}
	err = a.host.StateSet(ctx, a.spec.Name, scope, owner, req.Key, req.Value, a.spec.Grant.StateQuota)
	if err != nil {
		if isQuota(err) {
			return response{ID: req.ID, OK: false, Code: codeQuota, Error: err.Error()}
		}
		return badRequest(req, err)
	}
	return response{ID: req.ID, OK: true}
}

func (a *apiServer) stateDelete(ctx context.Context, req request) response {
	scope, owner, err := a.owner(req.Scope)
	if err != nil {
		return badRequest(req, err)
	}
	if err := a.host.StateDelete(ctx, a.spec.Name, scope, owner, req.Key); err != nil {
		return internal(req, err)
	}
	return response{ID: req.ID, OK: true}
}

func (a *apiServer) stateKeys(ctx context.Context, req request) response {
	scope, owner, err := a.owner(req.Scope)
	if err != nil {
		return badRequest(req, err)
	}
	keys, err := a.host.StateKeys(ctx, a.spec.Name, scope, owner)
	if err != nil {
		return internal(req, err)
	}
	return response{ID: req.ID, OK: true, Keys: keys}
}

// ---------------------------------------------------------------------------
// Level 3 — announce
// ---------------------------------------------------------------------------

// announce posts as the door's own identity.
//
// # Why the area may not be federated
//
// §9.1.1 notes that a sysop CAN point a door at a local-only area so it never
// touches airtime. This implementation requires it, which is stricter, and the
// reason is §11.4: a sysop gets wide latitude over their own instance and
// narrow latitude over shared airtime. A door announcement on a federated area
// spends the mesh's budget on the say-so of a third-party binary, and the
// budget is roughly ten originated packets per node per day (§1.1).
//
// It is also the more honest place to draw the line, because the mechanism for
// putting door activity on the mesh already exists and is designed for it:
// DOOR_EVENT records, which are sized and batched for the purpose and given
// their own sub-budget (§9.5). Letting announce leak onto the mesh would be
// that feature arriving by accident, unbudgeted.
func (a *apiServer) announce(ctx context.Context, req request) response {
	area := strings.TrimSpace(a.spec.Grant.AnnounceArea)
	if area == "" {
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: "this door has no announce area; the sysop has not chosen one"}
	}
	if strings.TrimSpace(req.Text) == "" {
		return badRequest(req, errors.New("an announcement needs some text"))
	}

	federated, err := a.host.AreaIsFederated(ctx, area)
	if err != nil {
		return internal(req, err)
	}
	if federated {
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: fmt.Sprintf("%s is a federated area, and a door may not spend the "+
				"mesh's airtime; ask the sysop to point this door at a local area", area)}
	}

	if !a.mgr.allowAnnounce(a.spec.Name, a.spec.Grant.AnnouncePerHour) {
		return response{ID: req.ID, OK: false, Code: codeRateLimit,
			Error: fmt.Sprintf("this door may announce %d times an hour",
				a.spec.Grant.AnnouncePerHour)}
	}

	id, err := a.host.Announce(ctx, a.spec.Name, area, req.Subject, req.Text)
	if err != nil {
		return internal(req, err)
	}
	return response{ID: req.ID, OK: true, Record: id}
}

// ---------------------------------------------------------------------------
// Level 3 + a league grant — inter-BBS door events (§9.5)
// ---------------------------------------------------------------------------

// eventEmit queues one game event for the door's league.
//
// # What it does not do
//
// It does not name the actor. There is no actor field on the request, for the
// same reason state.get has no nick field: if the actor came from the door, a
// door could attribute a kill to anybody on the board. The actor is the
// session's nick, always.
//
// It does not transmit, and it does not claim to. The response says "queued",
// because that is what happened — the batch goes out when the league area's
// share of the budget allows, which may be hours. §6.5 spent a paragraph on
// why a false promise is worse than a spinner, and returning a record ID here
// would be exactly that: a door would print an id for something that has not
// been signed yet.
//
// The TARGET is different from the actor and has to be. A league exists so
// that "alice slew bob@pnw" can cross boards, so the door must be able to name
// somebody who is not here. That is a CLAIM by this node, not a verified fact,
// and §9.5 already says integrity against your own league members needs
// game-level design rather than a signature layer.
func (a *apiServer) eventEmit(ctx context.Context, req request) response {
	area := strings.TrimSpace(a.spec.Grant.LeagueArea)
	if area == "" {
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: "this door has no league area; the sysop has not chosen one"}
	}
	if a.sess.Nick == "" {
		// A guest has no name to attribute a result to, and inventing one
		// would put an unaccountable actor on other people's mesh.
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: "this session has no account, so there is nobody to report a result for"}
	}
	if strings.TrimSpace(req.Game) == "" {
		return badRequest(req, errors.New("an event needs a game name"))
	}

	// Base64 in JSON because the payload is arbitrary bytes and JSON strings
	// are text. Decoded here so an unencodable payload is the door's mistake
	// rather than a mystery 48 bytes further down.
	var payload []byte
	if req.Payload != "" {
		var err error
		payload, err = base64.StdEncoding.DecodeString(req.Payload)
		if err != nil {
			return badRequest(req, fmt.Errorf("payload is not base64: %w", err))
		}
	}

	if !a.mgr.allowLeague(a.spec.Name, a.spec.Grant.LeaguePerHour) {
		return response{ID: req.ID, OK: false, Code: codeRateLimit,
			Error: fmt.Sprintf("this door may report %d events an hour",
				a.spec.Grant.LeaguePerHour)}
	}

	err := a.host.QueueDoorEvent(ctx, DoorEventRequest{
		Door:    a.spec.Name,
		Area:    area,
		Game:    strings.TrimSpace(req.Game),
		Kind:    req.Kind,
		Actor:   a.sess.Nick,
		Target:  strings.TrimSpace(req.To),
		Payload: payload,
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrNotALeague):
		return response{ID: req.ID, OK: false, Code: codeForbidden, Error: err.Error()}
	case errors.Is(err, ErrUnknownTarget), errors.Is(err, ErrInvalidEvent):
		return badRequest(req, err)
	case isQueueFull(err):
		return response{ID: req.ID, OK: false, Code: codeQuota, Error: err.Error()}
	default:
		return internal(req, err)
	}

	// The same one-time notice level 4 uses. A door putting a player's nick on
	// other people's mesh is the same category of surprise as one posting as
	// them, and the machinery for saying so once already exists.
	return response{ID: req.ID, OK: true, Queued: true, Notice: a.noticeFor(ctx)}
}

// isQueueFull matches the store's queue ceiling on its message, the same way
// isQuota matches the state allowance: the rule is the store's and this package
// does not get to redefine it.
func isQueueFull(err error) bool {
	return err != nil && strings.Contains(err.Error(), "more queued events than the mesh can carry")
}

// ---------------------------------------------------------------------------
// Level 4 — act as the user
// ---------------------------------------------------------------------------

func (a *apiServer) userPost(ctx context.Context, req request) response {
	if a.sess.Nick == "" {
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: "this session has no account to act as"}
	}
	// Post is the same call the menu makes, with the user's own nick, so the
	// capability gate inside it applies unchanged. That is the intersection
	// rule: not a check here, but the absence of any path that skips theirs.
	id, err := a.host.PostAs(ctx, a.sess.Nick, req.Area, req.Subject, req.Text)
	if err != nil {
		return a.refused(req, err)
	}
	a.recordActAsUser(ctx, "door.post_as_user", req.Area, id)
	return response{ID: req.ID, OK: true, Record: id, Notice: a.noticeFor(ctx)}
}

func (a *apiServer) userDM(ctx context.Context, req request) response {
	if a.sess.Nick == "" {
		return response{ID: req.ID, OK: false, Code: codeForbidden,
			Error: "this session has no account to act as"}
	}
	id, err := a.host.SendDMAs(ctx, a.sess.Nick, req.To, req.Subject, req.Text)
	if err != nil {
		return a.refused(req, err)
	}
	a.recordActAsUser(ctx, "door.dm_as_user", req.To, id)
	return response{ID: req.ID, OK: true, Record: id, Notice: a.noticeFor(ctx)}
}

// recordActAsUser writes the §9.1.1 audit row: which door, which user, and the
// record it produced.
func (a *apiServer) recordActAsUser(ctx context.Context, action, target, recordID string) {
	detail := fmt.Sprintf("door=%s record=%s", a.spec.Name, recordID)
	if err := a.host.Audit(ctx, a.sess.Nick, action, target, detail); err != nil {
		a.mgr.log.Error("could not audit a door acting as a user",
			"door", a.spec.Name, "user", a.sess.Nick, "action", action, "err", err)
	}
}

// noticeFor returns the one-time notice, if this user has not had it.
//
// It rides on the RESPONSE rather than being written to the door's terminal,
// because the terminal belongs to the door and anything written there would be
// drawn over. The door is required to show it; the audit row is what holds the
// sysop's copy either way. A door that swallows the notice is a door to
// uninstall, and the honest way to say so is in the spec rather than by
// pretending we can force it.
func (a *apiServer) noticeFor(ctx context.Context) string {
	need, err := a.host.NoticeNeeded(ctx, a.spec.Name, a.sess.Nick)
	if err != nil {
		a.mgr.log.Error("could not check the door notice",
			"door", a.spec.Name, "user", a.sess.Nick, "err", err)
		return ""
	}
	if !need {
		return ""
	}
	return fmt.Sprintf("%s is posting and sending messages as you, %s. "+
		"The sysop granted it that. Every time it does, it is recorded against "+
		"your name in the audit log.", a.spec.Name, a.sess.Nick)
}

// refused turns a BBS refusal into a forbidden rather than an internal error.
//
// A door being told "that user may not post to a federated area" is the
// intersection rule working, not a fault, and it must be distinguishable from
// the database being down — or a door will retry the one thing that will never
// succeed.
func (a *apiServer) refused(req request, err error) response {
	return response{ID: req.ID, OK: false, Code: codeForbidden, Error: err.Error()}
}

func (a *apiServer) sessionInfo() SessionInfo {
	info := SessionInfo{
		Handle:   a.sess.Nick,
		RealName: a.sess.RealName,
		Node:     a.sess.Node,
		Width:    a.sess.Width,
		Height:   a.sess.Height,
		Terminal: a.sess.Term,
		ANSI:     a.sess.ANSI,
		Encoding: a.sess.Encoding,
	}
	// Read now rather than at launch: a door asking how long it has left wants
	// the answer now, and the whole reason it asks is that the number moves.
	if a.sess.TimeRemaining != nil {
		info.TimeRemainingSecs = int(a.sess.TimeRemaining().Seconds())
		info.TimeLimited = true
	}
	return info
}

// ---------------------------------------------------------------------------
// rate limiting
// ---------------------------------------------------------------------------

// allowAnnounce applies the per-door announce rate limit.
//
// It lives on the Manager rather than the server because the limit is per DOOR
// and a server is per invocation: a limit that reset every time a door was
// relaunched would be no limit at all for a door that announces on startup.
func (m *Manager) allowAnnounce(door string, perHour int) bool {
	return m.allowPerHour(m.announces, door, perHour)
}

// allowLeague applies the per-door league emit rate limit, on its own bucket.
//
// Separate from announces rather than shared: they are separate grants with
// separate limits, and one budget for both would let a chatty announcer
// silence a league or the reverse.
func (m *Manager) allowLeague(door string, perHour int) bool {
	return m.allowPerHour(m.leagueEmits, door, perHour)
}

func (m *Manager) allowPerHour(buckets map[string][]time.Time, door string, perHour int) bool {
	if perHour <= 0 {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock.Now()
	cutoff := now.Add(-time.Hour)
	kept := buckets[door][:0]
	for _, t := range buckets[door] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= perHour {
		buckets[door] = kept
		return false
	}
	buckets[door] = append(kept, now)
	return true
}

// ---------------------------------------------------------------------------
// wire types
// ---------------------------------------------------------------------------

type request struct {
	ID    int    `json:"id"`
	Op    string `json:"op"`
	Token string `json:"token,omitempty"`

	Scope string `json:"scope,omitempty"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`

	Area    string `json:"area,omitempty"`
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Text    string `json:"text,omitempty"`

	// Game and Kind belong to event.emit (§9.5). Payload is base64: it is
	// arbitrary door-defined bytes and JSON strings are text.
	Game    string `json:"game,omitempty"`
	Kind    uint8  `json:"kind,omitempty"`
	Payload string `json:"payload,omitempty"`
}

type response struct {
	ID    int    `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`

	// Level is the door's granted level, returned by hello so a door can find
	// out what it may do without trying and being refused.
	Level int `json:"level,omitempty"`

	Session *SessionInfo `json:"session,omitempty"`
	Value   string       `json:"value,omitempty"`
	Found   bool         `json:"found,omitempty"`
	Keys    []string     `json:"keys,omitempty"`
	Record  string       `json:"record,omitempty"`

	// Queued says an event was recorded for a later batch. There is
	// deliberately no record ID to go with it and no estimate of when: nothing
	// has been signed yet, and §6.5 is explicit that a promise nothing will
	// keep is worse than saying less.
	Queued bool `json:"queued,omitempty"`

	// Notice is the §9.1.1 one-time message the door must show the user.
	Notice string `json:"notice,omitempty"`
}

// SessionInfo is level 1: read-only context about who is playing (§9.1).
type SessionInfo struct {
	Handle   string `json:"handle"`
	RealName string `json:"real_name"`
	Node     int    `json:"node"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Terminal string `json:"terminal"`
	ANSI     bool   `json:"ansi"`
	Encoding string `json:"encoding"`

	// TimeLimited says whether TimeRemainingSecs means anything. Without it, a
	// door cannot tell "no limit" from "no time left", and the two call for
	// opposite behaviour.
	TimeLimited       bool `json:"time_limited"`
	TimeRemainingSecs int  `json:"time_remaining_secs"`
}

func badRequest(req request, err error) response {
	return response{ID: req.ID, OK: false, Code: codeBadRequest, Error: err.Error()}
}

func internal(req request, err error) response {
	return response{ID: req.ID, OK: false, Code: codeInternal, Error: err.Error()}
}

// isQuota reports whether err is the state quota being reached. Matched on the
// host's error rather than a sentinel here, because the quota is the store's
// rule and this package does not get to redefine it.
func isQuota(err error) bool {
	return err != nil && strings.Contains(err.Error(), "saved-state allowance")
}

// helloTimeout bounds how long a connected door has to identify itself.
const helloTimeout = 30 * time.Second

// readLine reads one newline-terminated message, refusing an oversized one.
//
// bufio.Scanner would be the obvious choice and is the wrong one: it reports an
// over-long line by stopping, which is indistinguishable from the door hanging
// up. This is a parser reachable by a hostile door (§12.5), so the difference
// between "too long" and "finished" has to survive.
func readLine(r *bufio.Reader, limit int) ([]byte, error) {
	var out []byte
	for {
		chunk, more, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > limit {
			return nil, fmt.Errorf("a request may be at most %d bytes", limit)
		}
		if !more {
			return out, nil
		}
	}
}

// newToken mints a callback token.
func newToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate door token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
