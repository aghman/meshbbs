package door

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
)

// The door side of the API (§9.1.1).
//
// Every other file in this package is the BBS. This one is what a DOOR uses,
// and it ships for two reasons.
//
// The first is proof: the reference doors are written against it, so the
// protocol has a client that is exercised on every test run rather than only a
// document that might have drifted.
//
// The second is that it is the shortest honest answer to "how do I write a
// door in Go". A door in Python or C cannot import this, which is why
// docs/doors.md specifies the wire protocol rather than pointing here — but a
// Go door author should not have to reimplement a JSON handshake to find out
// whose turn it is.
//
// # There is nothing privileged here
//
// This is an ordinary client of an ordinary socket. It holds no capability the
// door does not already have: the token comes from the descriptor the door was
// given, the level was fixed by the sysop before the door started, and every
// refusal is decided on the server's side. Reading this file tells you what a
// door can ask for. It does not tell you how to ask for more.

// Client is a door's connection to the BBS.
type Client struct {
	conn  net.Conn
	r     *bufio.Reader
	level int

	mu sync.Mutex
	id int
}

// ErrNoDescriptor means this program was not started by meshbbs as a door, or
// was started without an API.
//
// A distinct error because it is the first thing a door author hits, and
// "connection refused" would send them looking in the wrong place. §9.1 makes
// the socket optional: a door can be installed without one.
var ErrNoDescriptor = errors.New(
	"no session descriptor: this program was not launched as a meshbbs door, " +
		"or the door was installed without API access")

// APIError is a refusal from the BBS, with the code that says what kind.
//
// Separated from a transport failure on purpose. "You may not post to that
// area" is an answer and a door should show it to the player; "the socket
// closed" is a fault and a door should give up. A door that cannot tell them
// apart retries the one thing that will never work.
type APIError struct {
	Op   string
	Code string
	Msg  string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %s", e.Op, e.Msg) }

// Forbidden reports whether the BBS refused rather than failed.
func (e *APIError) Forbidden() bool { return e.Code == codeForbidden }

// RateLimited reports whether the door should slow down and try later.
func (e *APIError) RateLimited() bool { return e.Code == codeRateLimit }

// OutOfRoom reports whether the door has used up its state allowance, which it
// can fix by deleting something.
func (e *APIError) OutOfRoom() bool { return e.Code == codeQuota }

// ReadDescriptor loads the session descriptor this door was given.
func ReadDescriptor() (Descriptor, error) {
	path := os.Getenv("MESHBBS_DOOR_DESCRIPTOR")
	if path == "" {
		return Descriptor{}, ErrNoDescriptor
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, fmt.Errorf("read session descriptor: %w", err)
	}
	var d Descriptor
	if err := json.Unmarshal(body, &d); err != nil {
		return Descriptor{}, fmt.Errorf("parse session descriptor: %w", err)
	}
	if d.Version != descriptorVersion {
		// Worth refusing rather than guessing. The version exists so that a
		// door built against one format can say so instead of misreading
		// another (see descriptor.go).
		return Descriptor{}, fmt.Errorf(
			"session descriptor is version %d and this door understands %d",
			d.Version, descriptorVersion)
	}
	return d, nil
}

// Open connects to the BBS using the descriptor in the environment.
func Open() (*Client, error) {
	desc, err := ReadDescriptor()
	if err != nil {
		return nil, err
	}
	conn, err := dialAPI(desc.Socket)
	if err != nil {
		return nil, fmt.Errorf("connect to the BBS: %w", err)
	}
	c := &Client{conn: conn, r: bufio.NewReader(conn)}

	res, err := c.do(request{Op: opHello, Token: desc.Token})
	if err != nil {
		conn.Close()
		return nil, err
	}
	c.level = res.Level
	return c, nil
}

// Close hangs up.
func (c *Client) Close() error { return c.conn.Close() }

// Level is the §9.1.1 level the sysop granted this door.
//
// Worth checking rather than discovering: a door that asks the level once can
// hide a feature it may not use, instead of offering it and failing in front of
// the player.
func (c *Client) Level() int { return c.level }

// Session returns who is playing and how long they have (§9.1, level 1).
func (c *Client) Session() (SessionInfo, error) {
	res, err := c.do(request{Op: opSession})
	if err != nil {
		return SessionInfo{}, err
	}
	if res.Session == nil {
		return SessionInfo{}, errors.New("the BBS returned no session")
	}
	return *res.Session, nil
}

// State scopes, for the calls below.
const (
	// ScopeMine is private to the player at the keyboard.
	ScopeMine = scopeUser
	// ScopeShared is common to every player of this door — a high score table.
	ScopeShared = scopeGlobal
)

// StateGet reads a saved value. A key never written is not an error.
func (c *Client) StateGet(scope, key string) (string, bool, error) {
	res, err := c.do(request{Op: opStateGet, Scope: scope, Key: key})
	if err != nil {
		return "", false, err
	}
	return res.Value, res.Found, nil
}

// StateSet saves a value.
func (c *Client) StateSet(scope, key, value string) error {
	_, err := c.do(request{Op: opStateSet, Scope: scope, Key: key, Value: value})
	return err
}

// StateDelete removes a value. Removing what is not there succeeds.
func (c *Client) StateDelete(scope, key string) error {
	_, err := c.do(request{Op: opStateDelete, Scope: scope, Key: key})
	return err
}

// StateKeys lists what this door has saved in a scope.
func (c *Client) StateKeys(scope string) ([]string, error) {
	res, err := c.do(request{Op: opStateKeys, Scope: scope})
	if err != nil {
		return nil, err
	}
	return res.Keys, nil
}

// Announce posts to the door's area as the DOOR, never as the player (level 3).
func (c *Client) Announce(subject, text string) (string, error) {
	res, err := c.do(request{Op: opAnnounce, Subject: subject, Text: text})
	if err != nil {
		return "", err
	}
	return res.Record, nil
}

// EmitEvent reports one game event to the door's league (§9.5).
//
// Level 3 plus a league grant, which is why it can be refused on a door that
// announces perfectly well: the sysop chose an announce area and not a league
// area, and those are separate decisions. Treat a Forbidden here as "this board
// is not in a league" rather than as a fault.
//
// # What the two return values are for, and why neither is a record ID
//
// queued says the BBS wrote the event down for a later batch. It is not a
// promise that anything was transmitted, and there is deliberately no ID to
// print: the batch goes out when the league area's share of the airtime budget
// allows, which may be hours away, and nothing has been signed yet. A door that
// tells a player "sent" here is making a promise the mesh never made.
//
// notice is §9.1.1's one-time message, and carries the same obligation PostAs
// describes — a door putting a player's nick on other people's mesh is the same
// category of surprise as one posting as them. Show it.
//
// The actor is NOT a parameter. It is the session's nick, chosen by the BBS, so
// that a door cannot attribute a result to somebody who did not play. `to` may
// name somebody on another board ("bob@pnw"); the BBS resolves it, and the door
// never learns a node ID it did not already have.
//
// The payload's 48-byte ceiling is not restated here. It belongs to the record
// codec and is enforced on the server, and a bound copied into two places is a
// bound that will eventually disagree with itself — this client holds no
// authority the door does not already have.
func (c *Client) EmitEvent(game string, kind uint8, to string, payload []byte) (queued bool, notice string, err error) {
	req := request{Op: opEventEmit, Game: game, Kind: kind, To: to}
	if len(payload) > 0 {
		// Base64 because the payload is arbitrary bytes and JSON strings are
		// text. Empty stays empty rather than becoming "": absent and empty are
		// the same thing on the wire, and sending one for the other would put a
		// zero-length payload where the door meant none.
		req.Payload = base64.StdEncoding.EncodeToString(payload)
	}
	res, err := c.do(req)
	if err != nil {
		return false, "", err
	}
	return res.Queued, res.Notice, nil
}

// PollEvents reads the league back from a cursor (§9.5).
//
// # The cursor is the door's to keep
//
// after is the Cursor a previous poll returned, and zero means "everything this
// node still holds". The BBS tracks no per-door read position on purpose: the
// operation stays stateless, and two invocations of the same door — two players
// at once — cannot fight over one position. A door that wants to resume where it
// left off saves the returned Cursor in its own level-2 state.
//
// It is a LOCAL ARRIVAL number, not a timestamp and not a sequence. Records
// arrive out of order on a mesh as a matter of course, so a door that invented
// its own ordering from At would step past a result repaired an hour late.
//
// # Check Truncated
//
// It says retention pruned records this cursor had not reached: there is a gap
// no further polling will fill. A door that ignores it prints an incomplete
// league table and calls it complete, which is the one thing a league table must
// not do.
func (c *Client) PollEvents(game string, after int64) (DoorEventBatch, error) {
	res, err := c.do(request{Op: opEventPoll, Game: game, After: after})
	if err != nil {
		return DoorEventBatch{}, err
	}
	// The same type the host side returns, rather than a client-shaped copy of
	// it: a poll's answer is one thing, and two structs with the same three
	// fields would be two places to add the fourth.
	return DoorEventBatch{
		Cursor:    res.Cursor,
		Events:    res.Events,
		Truncated: res.Truncated,
	}, nil
}

// PayloadBytes decodes a polled event's door-defined payload.
//
// A method rather than a decoded field, because the wire type is shared with the
// host side and the host has the bytes already — only a door receives the
// base64. It is the reading half of what EmitEvent encoded, so it lives beside
// it rather than with the wire types.
func (e PolledDoorEvent) PayloadBytes() ([]byte, error) {
	if e.Payload == "" {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("event payload is not base64: %w", err)
	}
	return b, nil
}

// PostAs posts as the player (level 4).
//
// The second return is §9.1.1's one-time notice, non-empty the first time this
// door acts as this user. A door MUST show it. Nothing here can force that —
// the terminal belongs to the door — so it is stated in the spec and recorded
// in the audit log either way. A door that swallows it is a door to uninstall.
func (c *Client) PostAs(area, subject, text string) (record string, notice string, err error) {
	res, err := c.do(request{Op: opUserPost, Area: area, Subject: subject, Text: text})
	if err != nil {
		return "", "", err
	}
	return res.Record, res.Notice, nil
}

// SendDMAs sends a private message as the player (level 4). See PostAs about
// the notice.
func (c *Client) SendDMAs(to, subject, text string) (record string, notice string, err error) {
	res, err := c.do(request{Op: opUserDM, To: to, Subject: subject, Text: text})
	if err != nil {
		return "", "", err
	}
	return res.Record, res.Notice, nil
}

// do sends one request and reads its reply.
//
// Serialised, because the protocol is one reply per request in order and a door
// that called from two goroutines would otherwise read each other's answers.
// Doors are single-player programs, so this costs nothing and removes a bug
// nobody would enjoy finding.
func (c *Client) do(req request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.id++
	req.ID = c.id

	body, err := json.Marshal(req)
	if err != nil {
		return response{}, err
	}
	if _, err := c.conn.Write(append(body, '\n')); err != nil {
		return response{}, fmt.Errorf("%s: %w", req.Op, err)
	}

	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return response{}, fmt.Errorf("%s: %w", req.Op, err)
	}
	var res response
	if err := json.Unmarshal(line, &res); err != nil {
		return response{}, fmt.Errorf("%s: the BBS sent something unreadable: %w", req.Op, err)
	}
	if !res.OK {
		return res, &APIError{Op: req.Op, Code: res.Code, Msg: res.Error}
	}
	return res, nil
}
