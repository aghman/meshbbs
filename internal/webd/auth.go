package webd

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// Passkey sign-in and enrolment (webui.md §7, §8).

// handleLoginBegin starts a discoverable login.
//
// No nick is asked for and none is sent. The authenticator knows which
// credential belongs to this site and hands back the user handle, which is
// most of why the browser path is less work than SSH rather than more ([D17]).
func (s *Server) handleLoginBegin(w http.ResponseWriter, r *http.Request) {
	options, sessionData, err := s.wa.BeginDiscoverableLogin()
	if err != nil {
		s.log.Error("begin login", "err", err)
		httpError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}

	id, err := s.ceremonies.Put("", *sessionData)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not start sign-in")
		return
	}
	s.setCookie(w, ceremonyCookie, id, int(ceremonyTTL.Seconds()))
	writeJSON(w, http.StatusOK, options)
}

// handleLoginFinish verifies an assertion and opens a session.
func (s *Server) handleLoginFinish(w http.ResponseWriter, r *http.Request) {
	cer, ok := s.takeCeremony(w, r)
	if !ok {
		httpError(w, http.StatusBadRequest, "that sign-in attempt expired — try again")
		return
	}

	parsed, err := protocol.ParseCredentialRequestResponseBody(r.Body)
	if err != nil {
		httpError(w, http.StatusBadRequest, "could not read the response from your passkey")
		return
	}

	// The handler is how a discoverable credential resolves to an account: the
	// authenticator asserts a credential ID and a user handle, and we look up
	// what we stored at enrolment.
	var resolved store.User
	user, credential, err := s.wa.ValidatePasskeyLogin(
		func(rawID, userHandle []byte) (webauthn.User, error) {
			u, err := s.store.UserByWebAuthnHandle(r.Context(), userHandle)
			if err != nil {
				return nil, err
			}
			resolved = u
			return loadWebUser(r.Context(), s.store, u.Nick)
		}, cer.Data, parsed)
	if err != nil {
		s.log.Warn("passkey login rejected", "err", err, "remote", r.RemoteAddr)
		httpError(w, http.StatusUnauthorized, "that passkey was not accepted")
		return
	}
	_ = user

	if !resolved.CanLogin || resolved.State != "active" {
		s.log.Warn("passkey login for a barred account", "nick", resolved.Nick, "state", resolved.State)
		httpError(w, http.StatusForbidden, "that account cannot sign in — ask the sysop")
		return
	}

	// Advance the signature counter. A counter that did not move means the
	// credential is on two devices, and the store refuses rather than quietly
	// writing the lower value.
	if err := s.store.UseWebAuthnCredential(r.Context(), credential.ID,
		credential.Authenticator.SignCount); err != nil {
		if errors.Is(err, store.ErrCloned) {
			s.log.Error("CLONED AUTHENTICATOR: signature counter did not advance",
				"nick", resolved.Nick, "remote", r.RemoteAddr)
			httpError(w, http.StatusUnauthorized,
				"that passkey looks like a copy and was refused — tell the sysop")
			return
		}
		s.log.Error("record passkey use", "err", err)
		httpError(w, http.StatusInternalServerError, "could not complete sign-in")
		return
	}

	sess, err := s.sessions.Create(resolved.Nick)
	if err != nil {
		if errors.Is(err, ErrTooManySessions) {
			httpError(w, http.StatusServiceUnavailable,
				"this BBS is full. Try again shortly, or connect over SSH, which has its own capacity")
			return
		}
		httpError(w, http.StatusInternalServerError, "could not open a session")
		return
	}
	if err := s.store.RecordLogin(r.Context(), resolved.Nick); err != nil {
		s.log.Warn("record login", "err", err)
	}

	s.setCookie(w, sessionCookie, sess.ID, 0)
	s.log.Info("passkey sign-in", "nick", resolved.Nick, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]string{"nick": resolved.Nick})
}

// handleEnrolBegin redeems an SSH-minted code and starts a registration ([D18]).
//
// # Why the code is spent HERE and not at finish
//
// Redeeming at finish would make this endpoint an oracle: a caller could test
// codes all day and only burn one when it worked. Spending it up front means
// every guess costs the guesser their attempt, which is what makes a
// rate-limited 64-bit code sufficient.
//
// The cost is that abandoning the browser prompt wastes the code. That is a
// deliberate trade — the remedy is pressing P on the SSH session again, which
// takes seconds.
func (s *Server) handleEnrolBegin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, "could not read that request")
		return
	}

	code := auth.NormaliseEnrolmentCode(body.Code)
	if code == "" {
		httpError(w, http.StatusBadRequest, "enter the code shown on your SSH session")
		return
	}

	user, err := s.store.RedeemEnrolmentCode(r.Context(), auth.HashEnrolmentCode(code))
	switch {
	case errors.Is(err, store.ErrCodeExpired):
		// Distinct from "unknown" on purpose: "expired, get another" is
		// actionable, "wrong code" sends someone off to re-read what they typed
		// correctly. Learning that a guessed 64-bit value was once real is not
		// a meaningful disclosure.
		httpError(w, http.StatusBadRequest, "that code has expired — press P on your SSH session for a new one")
		return
	case errors.Is(err, store.ErrNotFound):
		s.log.Warn("enrolment code rejected", "remote", r.RemoteAddr)
		httpError(w, http.StatusBadRequest, "that code is not valid — it may already have been used")
		return
	case err != nil:
		s.log.Error("redeem enrolment code", "err", err)
		httpError(w, http.StatusInternalServerError, "could not check that code")
		return
	}

	if !user.CanLogin || user.State != "active" {
		httpError(w, http.StatusForbidden, "that account cannot sign in — ask the sysop")
		return
	}

	wu, err := loadWebUser(r.Context(), s.store, user.Nick)
	if err != nil {
		s.log.Error("load account for enrolment", "err", err)
		httpError(w, http.StatusInternalServerError, "could not start enrolment")
		return
	}

	options, sessionData, err := s.wa.BeginRegistration(wu,
		// Resident keys are the whole point: without a discoverable credential
		// the user would have to type a nick to sign in, which is the friction
		// [D17] set out to remove.
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		// Attestation says which MAKE of authenticator this is. A BBS has no
		// use for that, and asking produces a scarier browser prompt plus a
		// privacy signal nobody wanted.
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		s.log.Error("begin registration", "err", err)
		httpError(w, http.StatusInternalServerError, "could not start enrolment")
		return
	}

	// The nick is held SERVER-SIDE in the ceremony, never sent to the browser.
	// If the client could name the account, a stolen code would not even be
	// needed — anyone could enrol a passkey onto anyone.
	id, err := s.ceremonies.Put(user.Nick, *sessionData)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not start enrolment")
		return
	}
	s.setCookie(w, ceremonyCookie, id, int(ceremonyTTL.Seconds()))

	s.log.Info("passkey enrolment started", "nick", user.Nick, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, options)
}

// handleEnrolFinish stores the newly registered passkey.
//
// It deliberately does NOT open a session. The user signs in with the passkey
// they just made, which keeps [D18]'s invariant literally true — no path leads
// from a code to a session — and has the practical benefit of proving the
// credential works before they walk away from the terminal that minted it.
func (s *Server) handleEnrolFinish(w http.ResponseWriter, r *http.Request) {
	cer, ok := s.takeCeremony(w, r)
	if !ok || cer.Nick == "" {
		httpError(w, http.StatusBadRequest, "that enrolment expired — press P on your SSH session for a new code")
		return
	}

	wu, err := loadWebUser(r.Context(), s.store, cer.Nick)
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not finish enrolment")
		return
	}

	credential, err := s.wa.FinishRegistration(wu, cer.Data, r)
	if err != nil {
		s.log.Warn("registration rejected", "nick", cer.Nick, "err", err)
		httpError(w, http.StatusBadRequest, "that passkey could not be registered")
		return
	}

	transports := make([]string, 0, len(credential.Transport))
	for _, t := range credential.Transport {
		transports = append(transports, string(t))
	}

	err = s.store.AddWebAuthnCredential(r.Context(), cer.Nick, store.WebAuthnCredential{
		CredentialID: credential.ID,
		PublicKey:    credential.PublicKey,
		SignCount:    credential.Authenticator.SignCount,
		AAGUID:       credential.Authenticator.AAGUID,
		Transports:   transports,
	})
	if err != nil {
		if errors.Is(err, store.ErrCredentialExists) {
			httpError(w, http.StatusConflict, "that passkey is already enrolled")
			return
		}
		s.log.Error("store passkey", "err", err)
		httpError(w, http.StatusInternalServerError, "could not save that passkey")
		return
	}

	s.log.Info("passkey enrolled", "nick", cer.Nick, "remote", r.RemoteAddr)
	writeJSON(w, http.StatusOK, map[string]string{"nick": cer.Nick})
}

// handleLogout ends the session.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		s.sessions.Delete(c.Value)
	}
	s.clearCookie(w, sessionCookie)
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// handleMe reports who this session belongs to.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		httpError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nick":     sess.Nick,
		"unlocked": sess.Unlocked,
	})
}

// takeCeremony consumes the in-flight ceremony named by the request's cookie.
func (s *Server) takeCeremony(w http.ResponseWriter, r *http.Request) (ceremony, bool) {
	s.clearCookie(w, ceremonyCookie)

	c, err := r.Cookie(ceremonyCookie)
	if err != nil || c.Value == "" {
		return ceremony{}, false
	}
	return s.ceremonies.Take(c.Value)
}
