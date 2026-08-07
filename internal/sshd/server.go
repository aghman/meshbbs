package sshd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/theme"
	"github.com/aghman/meshbbs/internal/tui"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	gossh "golang.org/x/crypto/ssh"
)

// HostKeyFile is the SSH host key filename within the keys directory.
const HostKeyFile = "ssh_host_ed25519_key"

// Options configures the server.
type Options struct {
	Bind         string
	Port         int
	KeysDir      string
	FilesDir     string
	GuestEnabled bool
	OpenSignup   bool
	Themes       *theme.Set
	DefaultTheme string
	// WebEnabled and WebURL surface the passkey-enrolment path ([D18]). An SSH
	// session is where a browser credential is bootstrapped from, so the SSH
	// front end has to know whether there is a browser front end to bootstrap
	// onto.
	WebEnabled bool
	WebURL     string
	Clock      clock.Clock
	Location   *time.Location
	Logger     *slog.Logger
}

// Server is the SSH front end.
type Server struct {
	opts     Options
	svc      *bbs.Service
	store    *store.Store
	blobs    *blobstore.Store
	authn    *Authenticator
	log      *slog.Logger
	wish     *ssh.Server
	presence *Presence
	chat     *tui.ChatRoom
}

// sessionKey is the context key under which the auth Decision is stashed.
//
// Auth runs before the session handler, so the handler needs to recover what
// auth concluded. Passing it through the context is how wish makes that
// possible without a side table keyed on connection.
type sessionKey struct{}

// NewServer builds the SSH server.
func NewServer(svc *bbs.Service, st *store.Store, opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Themes == nil {
		set, err := theme.Load("")
		if err != nil {
			return nil, err
		}
		opts.Themes = set
	}
	if opts.DefaultTheme == "" {
		opts.DefaultTheme = theme.DefaultName
	}
	if opts.Clock == nil {
		opts.Clock = clock.NewReal()
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}

	s := &Server{
		opts:     opts,
		svc:      svc,
		store:    st,
		authn:    NewAuthenticator(st, opts.GuestEnabled, opts.OpenSignup),
		log:      opts.Logger,
		presence: NewPresence(),
		// Node chat is shared by every session on this instance, and bounded:
		// it is a live conversation, not a message base. Anything worth
		// keeping belongs in a forum area, where it becomes a signed record.
		chat: tui.NewChatRoom(200),
	}

	// The blob store is where file contents live (§6.5). A BBS with no file
	// directory configured runs fine and simply has no file areas, so this is
	// optional rather than fatal — the SFTP handler says so when asked.
	if opts.FilesDir != "" {
		blobs, err := blobstore.Open(filepath.Join(opts.FilesDir, "blobs"))
		if err != nil {
			return nil, err
		}
		s.blobs = blobs
	}

	hostKey, err := ensureHostKey(opts.KeysDir)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(opts.Bind, fmt.Sprint(opts.Port))
	srv, err := wish.NewServer(
		wish.WithAddress(addr),
		wish.WithHostKeyPath(hostKey),
		ssh.PublicKeyAuth(s.publicKeyHandler),
		ssh.PasswordAuth(s.passwordHandler),
		// SFTP is a subsystem, so it bypasses the interactive middleware
		// entirely — a client running `sftp` never gets a PTY or a TUI.
		wish.WithSubsystem("sftp", func(sess ssh.Session) { s.sftpHandler(sess) }),
		wish.WithMiddleware(s.sessionMiddleware()),
	)
	if err != nil {
		return nil, fmt.Errorf("create SSH server: %w", err)
	}
	s.wish = srv
	return s, nil
}

// Addr returns the listen address.
func (s *Server) Addr() string { return s.wish.Addr }

// ListenAndServe runs the server until the context is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_ = s.wish.Shutdown(shutdownCtx)
	}()

	s.log.Info("ssh listening", "addr", s.wish.Addr)
	if err := s.wish.ListenAndServe(); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
		return err
	}
	return nil
}

// Serve runs the server on an existing listener. Tests use this to bind port 0.
func (s *Server) Serve(ln net.Listener) error {
	if err := s.wish.Serve(ln); err != nil && !errors.Is(err, ssh.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.wish.Shutdown(ctx) }

// Presence returns the who's-online tracker.
func (s *Server) Presence() *Presence { return s.presence }

// Chat returns the shared node chat room, so a telnet listener joins the same
// conversation rather than a parallel one.
func (s *Server) Chat() *tui.ChatRoom { return s.chat }

func (s *Server) publicKeyHandler(ctx ssh.Context, key ssh.PublicKey) bool {
	d := s.authn.PublicKey(ctx, ctx.User(), key)
	s.log.Debug("public key auth", "user", ctx.User(), "intent", d.Intent.String())

	// A key-unknown outcome is ACCEPTED at the transport layer on purpose.
	//
	// Rejecting here would give the user an opaque "Permission denied
	// (publickey)" with no way to learn why. Letting them in to a screen that
	// explains the situation — and offers the password path to enrol this key
	// — is the difference between a recoverable state and a support email.
	// The session handler grants no account access for this intent.
	if d.Intent == IntentUnknown {
		return false
	}
	ctx.SetValue(sessionKey{}, d)
	return true
}

func (s *Server) passwordHandler(ctx ssh.Context, password string) bool {
	d := s.authn.Password(ctx, ctx.User(), password)
	s.log.Debug("password auth", "user", ctx.User(), "intent", d.Intent.String())
	if d.Intent == IntentUnknown {
		return false
	}
	ctx.SetValue(sessionKey{}, d)
	return true
}

// decisionFrom recovers the auth outcome for a session.
func decisionFrom(sess ssh.Session) (Decision, bool) {
	v := sess.Context().Value(sessionKey{})
	d, ok := v.(Decision)
	return d, ok
}

// ensureHostKey generates the SSH host key on first run.
//
// This is distinct from the node identity key (§6.1): the host key identifies
// the SSH endpoint to clients, the node key identifies the BBS to the network.
// Conflating them would mean rotating one forced rotating the other.
func ensureHostKey(keysDir string) (string, error) {
	path := filepath.Join(keysDir, HostKeyFile)
	if _, err := os.Stat(path); err == nil {
		if err := checkHostKeyPerms(path); err != nil {
			return "", err
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat host key: %w", err)
	}

	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return "", fmt.Errorf("create keys directory: %w", err)
	}
	// wish generates an Ed25519 host key when the path does not exist; it is
	// created with restrictive permissions by the library.
	return path, nil
}

func checkHostKeyPerms(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("SSH host key %s has permissions %04o; it must not be readable "+
			"by other users (chmod 600 %s)", path, perm, path)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Presence (§5, multi-node presence)
// ---------------------------------------------------------------------------

// Presence tracks who is connected. It is deliberately in-memory: presence is
// ephemeral, and persisting it would mean stale rows after a crash.
type Presence struct {
	mu       sync.RWMutex
	sessions map[string]*SessionInfo
	nextNode int
}

// SessionInfo describes one connected user.
type SessionInfo struct {
	ID     string
	Nick   string
	Node   int // BBS "node number", the classic per-line identifier
	Guest  bool
	Remote string
	Where  string
}

// NewPresence builds a tracker.
func NewPresence() *Presence {
	return &Presence{sessions: map[string]*SessionInfo{}, nextNode: 1}
}

// Join registers a session and assigns a node number.
func (p *Presence) Join(id, nick, remote string, guest bool) *SessionInfo {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Reuse the lowest free node number, so a busy BBS does not drift upward
	// forever — node numbers are how users refer to each other in chat.
	used := map[int]bool{}
	for _, s := range p.sessions {
		used[s.Node] = true
	}
	node := 1
	for used[node] {
		node++
	}

	info := &SessionInfo{ID: id, Nick: nick, Node: node, Guest: guest, Remote: remote, Where: "menu"}
	p.sessions[id] = info
	return info
}

// Leave removes a session.
func (p *Presence) Leave(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
}

// SetLocation updates where a user is in the BBS.
func (p *Presence) SetLocation(id, where string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sessions[id]; ok {
		s.Where = where
	}
}

// List returns connected sessions, ordered by node number.
func (p *Presence) List() []SessionInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make([]SessionInfo, 0, len(p.sessions))
	for _, s := range p.sessions {
		out = append(out, *s)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Node < out[j-1].Node; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Count returns the number of connected sessions.
func (p *Presence) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.sessions)
}

var _ = gossh.MarshalAuthorizedKey
