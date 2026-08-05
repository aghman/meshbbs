// Package webd is the browser front end (webui.md).
//
// It is the third transport onto the same BBS, after SSH and telnet, and it
// reuses the session layer rather than growing one of its own: a web session
// joins the same Presence, sees the same chat room and renders the same
// Screen values as an SSH session. One BBS, three doors.
//
// Authentication is the part that could NOT be shared. SSH hands the server a
// public key before a session exists; a browser has nothing equivalent, so the
// web front end authenticates with passkeys ([D17]) and bootstraps them from an
// SSH-minted enrolment code ([D18]).
package webd

import (
	"context"

	"github.com/aghman/meshbbs/internal/store"
	"github.com/go-webauthn/webauthn/webauthn"
)

// webUser adapts a BBS account to the interface go-webauthn expects.
//
// The credentials are loaded eagerly rather than fetched through a callback,
// because a ceremony must see a consistent set: a passkey enrolled halfway
// through a login would otherwise appear in one check and not the next.
type webUser struct {
	user   store.User
	handle []byte
	creds  []webauthn.Credential
}

// loadWebUser builds the adapter, issuing a user handle on first use.
func loadWebUser(ctx context.Context, st *store.Store, nick string) (*webUser, error) {
	u, err := st.GetUser(ctx, nick)
	if err != nil {
		return nil, err
	}
	handle, err := st.EnsureWebAuthnHandle(ctx, nick)
	if err != nil {
		return nil, err
	}
	stored, err := st.WebAuthnCredentials(ctx, nick)
	if err != nil {
		return nil, err
	}

	creds := make([]webauthn.Credential, 0, len(stored))
	for _, c := range stored {
		creds = append(creds, toWebAuthnCredential(c))
	}
	return &webUser{user: u, handle: handle, creds: creds}, nil
}

func (u *webUser) WebAuthnID() []byte                         { return u.handle }
func (u *webUser) WebAuthnName() string                       { return u.user.Nick }
func (u *webUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// WebAuthnDisplayName is what a passkey manager shows. It falls back to the
// nick, since display_name is optional on this BBS and an empty entry in
// somebody's password manager is worse than a terse one.
func (u *webUser) WebAuthnDisplayName() string {
	if u.user.DisplayName != "" {
		return u.user.DisplayName
	}
	return u.user.Nick
}

// toWebAuthnCredential converts a stored passkey into the library's shape.
func toWebAuthnCredential(c store.WebAuthnCredential) webauthn.Credential {
	out := webauthn.Credential{
		ID:        c.CredentialID,
		PublicKey: c.PublicKey,
	}
	out.Authenticator.AAGUID = c.AAGUID
	out.Authenticator.SignCount = c.SignCount
	return out
}
