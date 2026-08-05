package webd

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Sessions are in memory, like Presence and for the same reason: they are
// ephemeral, and persisting them would leave stale rows after a crash. A
// restart logs everyone out, which is exactly what a restart does to SSH.

// Session is one signed-in browser.
type Session struct {
	ID   string
	Nick string
	// Unlocked reports whether this session holds a mail passphrase in memory.
	// It selects the SHORTER idle timeout, because a browser tab closing is a
	// far less reliable goodbye than an SSH disconnect — a killed tab, a locked
	// phone and a sleeping laptop may send nothing at all — so here the timer is
	// doing real security work rather than tidying up (webui.md §9).
	Unlocked  bool
	CreatedAt time.Time
	LastSeen  time.Time
}

var (
	// ErrNoSession is returned when a session ID is unknown or has expired.
	ErrNoSession = errors.New("no such session")
	// ErrTooManySessions is returned when a cap would be exceeded.
	ErrTooManySessions = errors.New("too many sessions")
)

type sessionStore struct {
	mu   sync.Mutex
	byID map[string]*Session

	clock        clock.Clock
	idle         time.Duration
	unlockedIdle time.Duration
	ttl          time.Duration
	max          int
	maxPerUser   int
}

func newSessionStore(clk clock.Clock, o Options) *sessionStore {
	return &sessionStore{
		byID:         map[string]*Session{},
		clock:        clk,
		idle:         time.Duration(o.IdleTimeoutMins) * time.Minute,
		unlockedIdle: time.Duration(o.UnlockedIdleTimeoutMins) * time.Minute,
		ttl:          time.Duration(o.SessionTTLHours) * time.Hour,
		max:          o.MaxSessions,
		maxPerUser:   o.MaxSessionsPerUser,
	}
}

// Create opens a session for an account.
func (s *sessionStore) Create(nick string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	// A public listener with no cap is a file-descriptor exhaustion away from
	// taking SSH down with it, which is the same reasoning telnet's cap rests
	// on.
	if s.max > 0 && len(s.byID) >= s.max {
		return nil, ErrTooManySessions
	}
	if s.maxPerUser > 0 {
		n := 0
		for _, sess := range s.byID {
			if sess.Nick == nick {
				n++
			}
		}
		if n >= s.maxPerUser {
			return nil, ErrTooManySessions
		}
	}

	id, err := newToken()
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	sess := &Session{ID: id, Nick: nick, CreatedAt: now, LastSeen: now}
	s.byID[id] = sess
	return sess, nil
}

// Get returns a live session and stamps it as seen.
func (s *sessionStore) Get(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()

	sess, ok := s.byID[id]
	if !ok {
		return nil, ErrNoSession
	}
	sess.LastSeen = s.clock.Now()
	// Return a copy: callers must not mutate shared state without the lock,
	// and a session is small enough that copying is cheaper than reasoning
	// about who holds a pointer.
	out := *sess
	return &out, nil
}

// SetUnlocked records that a session now holds a mail passphrase, which
// tightens its idle bound.
func (s *sessionStore) SetUnlocked(id string, unlocked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.byID[id]; ok {
		sess.Unlocked = unlocked
	}
}

// Delete ends a session.
func (s *sessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// Count returns the number of live sessions.
func (s *sessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	return len(s.byID)
}

// sweepLocked drops expired sessions. It runs on every access rather than on a
// timer: the map is small, and a background sweeper would be one more goroutine
// whose failure mode is silent.
func (s *sessionStore) sweepLocked() {
	now := s.clock.Now()
	for id, sess := range s.byID {
		if s.expiredLocked(sess, now) {
			delete(s.byID, id)
		}
	}
}

func (s *sessionStore) expiredLocked(sess *Session, now time.Time) bool {
	if s.ttl > 0 && now.Sub(sess.CreatedAt) >= s.ttl {
		return true
	}
	idle := s.idle
	if sess.Unlocked && s.unlockedIdle > 0 && s.unlockedIdle < idle {
		idle = s.unlockedIdle
	}
	return idle > 0 && now.Sub(sess.LastSeen) >= idle
}

// ---------------------------------------------------------------------------
// Ceremonies
// ---------------------------------------------------------------------------

// ceremonyTTL bounds how long a half-finished WebAuthn exchange stays open. The
// browser prompt is in front of the user the whole time, so this is generous.
const ceremonyTTL = 5 * time.Minute

// ceremony is the server half of an in-flight WebAuthn exchange.
//
// Nick is empty for a discoverable login, where the whole point is that the
// server does not know who is signing in until the authenticator says. For
// enrolment it is the account the code resolved to — and it is held HERE rather
// than sent to the browser, so the client cannot choose whose account a
// credential lands on.
type ceremony struct {
	Nick    string
	Data    webauthn.SessionData
	Expires time.Time
}

type ceremonyStore struct {
	mu    sync.Mutex
	byID  map[string]ceremony
	clock clock.Clock
}

func newCeremonyStore(clk clock.Clock) *ceremonyStore {
	return &ceremonyStore{byID: map[string]ceremony{}, clock: clk}
}

func (c *ceremonyStore) Put(nick string, data webauthn.SessionData) (string, error) {
	id, err := newToken()
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()
	c.byID[id] = ceremony{Nick: nick, Data: data, Expires: c.clock.Now().Add(ceremonyTTL)}
	return id, nil
}

// Take returns a ceremony and removes it. Single use: a challenge that could be
// answered twice is a replay waiting to happen.
func (c *ceremonyStore) Take(id string) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked()

	cer, ok := c.byID[id]
	if !ok {
		return ceremony{}, false
	}
	delete(c.byID, id)
	return cer, true
}

func (c *ceremonyStore) sweepLocked() {
	now := c.clock.Now()
	for id, cer := range c.byID {
		if !now.Before(cer.Expires) {
			delete(c.byID, id)
		}
	}
}

// newToken returns an unguessable identifier.
//
// crypto/rand, never the seeded rng.Source that domain logic uses (§12.1): a
// session ID drawn from a reproducible stream is a session anybody can forge.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
