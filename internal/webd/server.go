package webd

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/theme"
	"github.com/aghman/meshbbs/internal/tui"
	"github.com/go-webauthn/webauthn/webauthn"
)

//go:embed static
var staticFS embed.FS

// Options configures the browser front end.
type Options struct {
	Bind string
	Port int
	// Origin is the public origin browsers reach this BBS at. It is also the
	// WebAuthn Relying Party origin, which is why it has no default and is
	// validated at startup: a mismatch does not degrade, it fails every sign-in
	// with a browser error that says nothing about the cause (webui.md §7.1).
	Origin string

	TLSCert string
	TLSKey  string

	DisplayName string

	MaxSessions             int
	MaxSessionsPerUser      int
	IdleTimeoutMins         int
	UnlockedIdleTimeoutMins int
	SessionTTLHours         int

	// Presence and Chat are the SSH front end's, shared rather than duplicated:
	// a browser user gets a node number and appears in who's-online next to
	// everyone else, and joins the same conversation. One BBS, three doors.
	Presence tui.PresenceTracker
	Chat     *tui.ChatRoom

	Themes    *theme.Set
	ThemeName string

	Clock    clock.Clock
	Location *time.Location
	Logger   *slog.Logger
}

// Server is the browser front end.
type Server struct {
	opts  Options
	svc   *bbs.Service
	store *store.Store
	log   *slog.Logger

	wa         *webauthn.WebAuthn
	sessions   *sessionStore
	ceremonies *ceremonyStore

	http  *http.Server
	ready chan struct{}
}

// NewServer builds the web front end.
func NewServer(svc *bbs.Service, st *store.Store, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Clock == nil {
		opts.Clock = clock.NewReal()
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.DisplayName == "" {
		opts.DisplayName = "MeshBBS"
	}
	if opts.Themes == nil {
		set, err := theme.Load("")
		if err != nil {
			return nil, err
		}
		opts.Themes = set
	}
	if opts.ThemeName == "" {
		opts.ThemeName = theme.DefaultName
	}
	if opts.Chat == nil {
		// A web-only instance still has node chat; it just has it to itself.
		opts.Chat = tui.NewChatRoom(200)
	}

	u, err := url.Parse(opts.Origin)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("web.origin %q is not a usable origin", opts.Origin)
	}

	// RPID is the origin's HOSTNAME with no scheme and no port. Getting this
	// wrong is the single most common WebAuthn deployment failure, and it fails
	// silently at the browser rather than here — hence deriving it rather than
	// asking the sysop for it separately.
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          u.Hostname(),
		RPDisplayName: opts.DisplayName,
		RPOrigins:     []string{originOf(u)},
	})
	if err != nil {
		return nil, fmt.Errorf("configure webauthn: %w", err)
	}

	s := &Server{
		opts:       opts,
		svc:        svc,
		store:      st,
		log:        opts.Logger,
		wa:         wa,
		sessions:   newSessionStore(opts.Clock, opts),
		ceremonies: newCeremonyStore(opts.Clock),
		ready:      make(chan struct{}),
	}

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("open static assets: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("POST /auth/login/begin", s.handleLoginBegin)
	mux.HandleFunc("POST /auth/login/finish", s.handleLoginFinish)
	mux.HandleFunc("POST /auth/enrol/begin", s.handleEnrolBegin)
	mux.HandleFunc("POST /auth/enrol/finish", s.handleEnrolFinish)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("GET /ws", s.handleWS)

	s.http = &http.Server{
		Addr:    net.JoinHostPort(opts.Bind, fmt.Sprint(opts.Port)),
		Handler: s.withSecurityHeaders(mux),
		// A browser that opens a socket and says nothing must not hold it
		// forever, for the same reason telnet caps its sessions.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return s, nil
}

// originOf renders a parsed URL back to a bare scheme://host[:port] origin.
func originOf(u *url.URL) string { return u.Scheme + "://" + u.Host }

// Addr returns the listen address.
func (s *Server) Addr() string { return s.http.Addr }

// Ready closes once the listener is accepting.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Sessions returns the number of live browser sessions.
func (s *Server) Sessions() int { return s.sessions.Count() }

// ListenAndServe runs until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("web listen: %w", err)
	}
	return s.Serve(ctx, ln)
}

// Serve runs on an existing listener. Tests use this to bind port 0.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.http.Shutdown(shutdownCtx)
	}()

	tlsEnabled := s.opts.TLSCert != "" && s.opts.TLSKey != ""
	s.log.Info("web listening", "addr", ln.Addr().String(),
		"origin", s.opts.Origin, "tls", tlsEnabled)
	close(s.ready)

	var err error
	if tlsEnabled {
		s.http.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		err = s.http.ServeTLS(ln, s.opts.TLSCert, s.opts.TLSKey)
	} else {
		// Plaintext is reachable only on loopback, which config validation
		// enforces — browsers treat localhost as a secure context, so passkeys
		// work there and nowhere else without a certificate.
		err = s.http.Serve(ln)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }
