// Package sshd implements the SSH front end (design §5.1).
//
// # Why auth is complicated here
//
// SSH decides accept-or-reject BEFORE any session exists, so there is no
// prompt at which a new user can type NEW. Registration therefore has to be
// resolved at the auth layer, from nothing but the username and credential the
// client already committed to. That is the whole reason this file exists
// separately from the session code.
package sshd

import (
	"context"
	"errors"
	"strings"

	"github.com/aghman/meshbbs/internal/store"
	gossh "golang.org/x/crypto/ssh"
)

// Intent is what the auth layer concluded a connection wants.
type Intent int

const (
	// IntentUnknown means auth failed and the connection must be rejected.
	IntentUnknown Intent = iota

	// IntentAuthenticated is an existing user who proved who they are.
	IntentAuthenticated

	// IntentSignup is a new-user registration. The offered public key, if any,
	// is enrolled on completion — SSH already handed it to us, so a
	// key-authenticated account costs the user no extra step.
	IntentSignup

	// IntentGuest is anonymous read-only access.
	IntentGuest

	// IntentKeyUnknown is an EXISTING nick presenting a credential we do not
	// recognise.
	//
	// This case must never be routed to signup. Offering "register?" to
	// someone whose key merely changed is how duplicate accounts and confused
	// users happen (§5.1), so it gets its own outcome and its own message.
	IntentKeyUnknown
)

func (i Intent) String() string {
	switch i {
	case IntentAuthenticated:
		return "authenticated"
	case IntentSignup:
		return "signup"
	case IntentGuest:
		return "guest"
	case IntentKeyUnknown:
		return "key-unknown"
	default:
		return "unknown"
	}
}

// Reserved usernames handled at the auth layer (§5.1).
const (
	// NewUser is the documented front door: `ssh new@bbs.example.com`.
	NewUser = "new"
	// GuestUser is anonymous read-only access.
	GuestUser = "guest"
)

// Decision is the outcome of authenticating a connection.
type Decision struct {
	Intent Intent
	// Nick is the account for IntentAuthenticated, or the suggested nick for
	// IntentSignup.
	Nick string
	// User is populated for IntentAuthenticated.
	User store.User
	// PublicKey is the key the client offered, if any. For signup it is
	// enrolled on completion.
	PublicKey string
	// Fingerprint of PublicKey.
	Fingerprint string
	// Reason explains a rejection, for the client and the log.
	Reason string
}

// Authenticator resolves connections against the store.
type Authenticator struct {
	store        *store.Store
	guestEnabled bool
	openSignup   bool
}

// NewAuthenticator builds an Authenticator.
func NewAuthenticator(st *store.Store, guestEnabled, openSignup bool) *Authenticator {
	return &Authenticator{store: st, guestEnabled: guestEnabled, openSignup: openSignup}
}

// FingerprintOf renders an SSH public key fingerprint.
func FingerprintOf(key gossh.PublicKey) string {
	if key == nil {
		return ""
	}
	return gossh.FingerprintSHA256(key)
}

// AuthorizedKeyOf renders a public key in authorized_keys form.
func AuthorizedKeyOf(key gossh.PublicKey) string {
	if key == nil {
		return ""
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(key)))
}

// PublicKey resolves a public-key authentication attempt.
//
// The `new` account accepts ANY key, including one never seen before — that is
// what makes `ssh new@host` work as the front door, and it is why the key is
// available to enrol the moment signup completes.
func (a *Authenticator) PublicKey(ctx context.Context, username string, key gossh.PublicKey) Decision {
	username = strings.TrimSpace(username)
	fp := FingerprintOf(key)
	authorized := AuthorizedKeyOf(key)

	switch strings.ToLower(username) {
	case NewUser:
		if !a.openSignup {
			return Decision{Intent: IntentUnknown, Reason: "registration is closed on this BBS"}
		}
		return Decision{Intent: IntentSignup, PublicKey: authorized, Fingerprint: fp}

	case GuestUser:
		if !a.guestEnabled {
			return Decision{Intent: IntentUnknown, Reason: "guest access is disabled on this BBS"}
		}
		return Decision{Intent: IntentGuest, Nick: GuestUser}
	}

	// A key we recognise authenticates its owner, whatever username was typed
	// — but only if the username matches, so one user cannot log in as another
	// by guessing a nick and offering their own key.
	if u, err := a.store.UserByFingerprint(ctx, fp); err == nil {
		if !strings.EqualFold(u.Nick, username) {
			return Decision{
				Intent: IntentUnknown,
				Reason: "that key belongs to a different account on this BBS",
			}
		}
		if !u.CanLogin {
			return Decision{Intent: IntentUnknown, Reason: "this account cannot log in"}
		}
		return Decision{
			Intent: IntentAuthenticated, Nick: u.Nick, User: u,
			PublicKey: authorized, Fingerprint: fp,
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return Decision{Intent: IntentUnknown, Reason: "internal error"}
	}

	// The key is unknown. Whether this is a new user or an existing one with a
	// new key depends entirely on the nick, and the distinction matters.
	if _, err := a.store.GetUser(ctx, username); err == nil {
		return Decision{
			Intent:      IntentKeyUnknown,
			Nick:        username,
			PublicKey:   authorized,
			Fingerprint: fp,
			Reason: "an account with that name exists, but this key is not enrolled on it. " +
				"Log in with your password to add this key, or use a client that has an enrolled key.",
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return Decision{Intent: IntentUnknown, Reason: "internal error"}
	}

	// Unknown nick, unknown key: the convenience path from §5.1. Someone typed
	// `ssh austin@bbs` for an account that does not exist yet, so offer to
	// create it rather than returning an opaque permission denied.
	if !a.openSignup {
		return Decision{Intent: IntentUnknown, Reason: "registration is closed on this BBS"}
	}
	if err := store.ValidateNick(username); err != nil {
		return Decision{
			Intent: IntentUnknown,
			Reason: "no such account, and " + err.Error() + " — try `ssh new@` to pick another name",
		}
	}
	return Decision{
		Intent: IntentSignup, Nick: username,
		PublicKey: authorized, Fingerprint: fp,
	}
}

// Password resolves a password authentication attempt.
func (a *Authenticator) Password(ctx context.Context, username, password string) Decision {
	username = strings.TrimSpace(username)

	switch strings.ToLower(username) {
	case NewUser:
		if !a.openSignup {
			return Decision{Intent: IntentUnknown, Reason: "registration is closed on this BBS"}
		}
		// Signup over password: the user has no key to offer, and will set a
		// password inside the TUI.
		return Decision{Intent: IntentSignup}

	case GuestUser:
		if !a.guestEnabled {
			return Decision{Intent: IntentUnknown, Reason: "guest access is disabled on this BBS"}
		}
		return Decision{Intent: IntentGuest, Nick: GuestUser}
	}

	u, err := a.store.AuthenticatePassword(ctx, username, password)
	if err != nil {
		// AuthenticatePassword deliberately does not distinguish a missing
		// account from a wrong password, so neither does this.
		return Decision{Intent: IntentUnknown, Reason: "invalid credentials"}
	}
	return Decision{Intent: IntentAuthenticated, Nick: u.Nick, User: u}
}

// KeyboardInteractivePrompt is the banner shown when a client falls back to
// keyboard-interactive auth.
const KeyboardInteractivePrompt = "meshbbs"
