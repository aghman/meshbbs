package webd

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	sessionCookie  = "meshbbs_session"
	ceremonyCookie = "meshbbs_ceremony"
)

// withSecurityHeaders wraps the mux with the protections a cookie-authenticated
// app needs.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()

		// Everything this page needs is served from this origin — the artifact
		// is self-contained by design, with no CDN and no external font. Saying
		// so in a CSP turns "we do not load third-party script" from a habit
		// into something the browser enforces.
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self'; style-src 'self'; "+
				"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")

		// A cookie-authenticated POST is not protected by the same-origin
		// policy the way fetch() is, so the Origin header is what stands
		// between this and cross-site request forgery. SameSite=Strict on the
		// cookie is the belt; this is the braces.
		if r.Method == http.MethodPost && !s.originAllowed(r) {
			s.log.Warn("rejected cross-origin request",
				"origin", r.Header.Get("Origin"), "path", r.URL.Path, "remote", r.RemoteAddr)
			httpError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether a request came from this BBS's own pages.
//
// A MISSING Origin header is refused rather than allowed. Browsers send it on
// every fetch that matters here, so absence means the request did not come from
// a page — and treating "no opinion" as "permitted" is how origin checks end up
// decorative.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSuffix(origin, "/"),
		strings.TrimSuffix(s.opts.Origin, "/"))
}

// currentSession resolves the session cookie, or nil.
func (s *Server) currentSession(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, err := s.sessions.Get(c.Value)
	if err != nil {
		return nil
	}
	return sess
}

// setCookie writes a cookie with the protections every cookie here wants.
func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		// Secure is set from the ORIGIN rather than unconditionally: a
		// loopback deployment is served over plaintext by design (browsers
		// grant it a secure context anyway), and a Secure cookie there would
		// simply never be stored, making local testing fail in a way that
		// looks like a bug in the auth code.
		Secure:   strings.HasPrefix(s.opts.Origin, "https://"),
		SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	s.setCookie(w, name, "", -1)
}

// writeJSON sends a JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// httpError sends a JSON error.
//
// The message is meant to be SHOWN. A sign-in failure the user cannot act on is
// how people conclude a site is broken, so these say what to do next rather
// than naming an internal condition.
func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
