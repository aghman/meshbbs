// Package tui is the Bubble Tea session interface (design §5).
//
// One tea.Program per SSH session. The model is a small state machine over
// screens; each screen is a method rather than a separate model, which keeps
// the shared state (user, theme, sizing) in one place and avoids threading it
// through a dozen sub-models for a UI this size.
package tui

import (
	"context"
	"log/slog"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/term"
	"github.com/aghman/meshbbs/internal/theme"
	tea "github.com/charmbracelet/bubbletea"
)

// Intent mirrors sshd.Intent, duplicated to avoid an import cycle.
type Intent int

const (
	IntentUnknown Intent = iota
	IntentAuthenticated
	IntentSignup
	IntentGuest
	IntentKeyUnknown
)

// Peer is a connected user, for the who's-online screen.
type Peer struct {
	Node  int
	Nick  string
	Guest bool
	Where string
}

// PresenceTracker is the subset of presence the TUI needs.
type PresenceTracker interface {
	Join(id, nick, remote string, guest bool) int
	Leave(id string)
	SetLocation(id, where string)
	List() []Peer
}

// screen identifies the current view.
type screen int

const (
	screenSignup screen = iota
	screenKeyUnknown
	screenMenu
	screenAreaList
	screenAreaRead
	screenPostCompose
	screenMailList
	screenMailRead
	screenMailCompose
	screenUnlock
	screenKeySetup
	screenWho
	screenSysop
	screenChat
	screenNodeInfo
	screenGoodbye
)

// Config is everything a session needs.
type Config struct {
	Service   *bbs.Service
	Store     *store.Store
	Presence  PresenceTracker
	Themes    *theme.Set
	ThemeName string
	Encoding  term.Encoding
	Width     int
	Height    int
	SessionID string
	Remote    string
	Intent    Intent
	Nick      string
	User      store.User
	PublicKey string
	KeyFP     string
	AuthNote  string
	Chat      *ChatRoom
	Clock     clock.Clock
	Location  *time.Location
	// DisableChatPolling stops a session from starting the background chat
	// watcher. It exists for tests: the watcher parks until someone speaks,
	// which is correct in a real program (commands run in goroutines) and a
	// deadlock in a harness that drives Update synchronously.
	DisableChatPolling bool
	Logger             *slog.Logger
	Ctx                context.Context
}

// Model is the session state.
type Model struct {
	cfg    Config
	styles theme.Styles
	ctx    context.Context

	screen screen
	width  int
	height int

	// Identity for this session.
	nick     string
	guest    bool
	sysop    bool
	nodeNum  int
	joined   bool
	unlocked bool
	// passphrase is held only while a session is unlocked for reading mail.
	// It never leaves this process and is cleared on lock and on exit.
	passphrase string

	// Transient UI state.
	status    string
	statusErr bool

	signup      signupState
	areas       []store.Area
	areaIdx     int
	posts       []store.Post
	postIdx     int
	compose     composeState
	mail        []store.DM
	mailIdx     int
	mailBody    string
	mailSubject string
	unlockPW    textInput
	setupPW     textInput
	setupPW2    textInput
	setupIdx    int
	peers       []Peer
	sysop_      sysopState
	chatInput   textInput
	chatLines   []ChatLine

	quitting bool
}

// New builds the session model.
func New(cfg Config) Model {
	if cfg.Ctx == nil {
		cfg.Ctx = context.Background()
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	th := cfg.Themes.Get(cfg.ThemeName)
	m := Model{
		cfg:    cfg,
		styles: th.Styles(cfg.Encoding == term.EncodingUTF8),
		ctx:    cfg.Ctx,
		width:  cfg.Width,
		height: cfg.Height,
		nick:   cfg.Nick,
	}

	switch cfg.Intent {
	case IntentSignup:
		m.screen = screenSignup
		m.signup = newSignupState(cfg.Nick, cfg.PublicKey != "")
	case IntentKeyUnknown:
		m.screen = screenKeyUnknown
	case IntentGuest:
		m.screen = screenMenu
		m.guest = true
		m.nick = "guest"
	default:
		m.screen = screenMenu
		m.sysop = cfg.User.IsSysop
	}
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	if m.screen == screenMenu {
		return tea.Batch(m.join(), m.loadAreas())
	}
	return nil
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Ignore nonsense sizes rather than adopting them. A client that
		// cannot report its geometry — a scripted session, some telnet
		// clients, a terminal mid-resize — sends 0x0, and taking that at face
		// value collapses every screen to the minimum width. The size
		// negotiated at connection time is a better guess than zero.
		if msg.Width >= 20 {
			m.width = msg.Width
		}
		if msg.Height >= 5 {
			m.height = msg.Height
		}
		return m, nil

	case tea.KeyMsg:
		// Ctrl+C always leaves, from any screen. A BBS that can trap a user is
		// a BBS people stop trusting.
		if msg.Type == tea.KeyCtrlC {
			return m.leave()
		}
		return m.handleKey(msg)

	case statusMsg:
		m.status, m.statusErr = msg.text, msg.isErr
		return m, nil

	case areasLoadedMsg:
		m.areas = msg.areas
		return m, nil

	case postsLoadedMsg:
		m.posts, m.postIdx = msg.posts, 0
		if len(m.posts) > 0 {
			m.postIdx = len(m.posts) - 1
		}
		return m, nil

	case mailLoadedMsg:
		m.mail, m.mailIdx = msg.mail, 0
		return m, nil

	case mailOpenedMsg:
		m.mailSubject, m.mailBody = msg.subject, msg.body
		m.screen = screenMailRead
		return m, nil

	case peersLoadedMsg:
		m.peers = msg.peers
		return m, nil

	case joinedMsg:
		m.nodeNum, m.joined = msg.node, true
		return m, nil

	case needKeySetupMsg:
		// The account has no DM key — typically created by the CLI, which
		// cannot know a passphrase. Offer to create one rather than dead-ending
		// on "no DM key yet", which the user has no way to act on.
		m.screen = screenKeySetup
		m.setupPW = newInput("Passphrase: ", 64, true)
		m.setupPW2 = newInput("Confirm: ", 64, true)
		m.setupIdx = 0
		return m, nil

	case needUnlockMsg:
		m.screen = screenUnlock
		m.unlockPW = newInput("Passphrase: ", 64, true)
		return m, nil

	case sysopDataMsg:
		m.sysop_.users = msg.users
		m.sysop_.caps = msg.caps
		m.sysop_.areas = msg.areas
		m.sysop_.aliases = msg.aliases
		return m, nil

	case sysopActionMsg:
		m.status, m.statusErr = msg.text, false
		return m, m.loadSysopData()

	case chatUpdatedMsg:
		m.chatLines = msg.lines
		// Re-arm the watcher so the next message also wakes us.
		if m.screen == screenChat {
			return m, m.pollChat()
		}
		return m, nil

	case unlockedMsg:
		// The passphrase lives only in this session's memory, only while
		// unlocked, and is cleared on exit (§8.2 tier 2).
		m.passphrase = msg.passphrase
		m.unlocked = true
		m.screen = screenMailList
		m.setWhere("mail")
		return m, m.loadMail()

	case signupDoneMsg:
		m.nick = msg.nick
		m.sysop = false
		m.unlocked = true
		m.passphrase = msg.passphrase
		m.screen = screenMenu
		m.status = "Welcome to the BBS, " + msg.nick + "."
		return m, tea.Batch(m.join(), m.loadAreas())
	}

	return m, nil
}

// Screen describes what this session is looking at, independent of how it will
// be drawn (webui.md §2).
//
// This is the ONLY way a screen becomes visible. View() goes through it, and so
// does the web front end — which is what stops a screen from existing over SSH
// and being quietly missing from the browser.
func (m Model) Screen() Screen {
	switch m.screen {
	case screenSignup:
		return m.buildSignup()
	case screenKeyUnknown:
		return m.buildKeyUnknown()
	case screenMenu:
		return m.buildMenu()
	case screenAreaList:
		return m.buildAreaList()
	case screenAreaRead:
		return m.buildAreaRead()
	case screenPostCompose:
		return m.buildCompose()
	case screenMailList:
		return m.buildMailList()
	case screenMailRead:
		return m.buildMailRead()
	case screenMailCompose:
		return m.buildMailCompose()
	case screenUnlock:
		return m.buildUnlock()
	case screenKeySetup:
		return m.buildKeySetup()
	case screenWho:
		return m.buildWho()
	case screenSysop:
		return m.buildSysop()
	case screenChat:
		return m.buildChat()
	case screenNodeInfo:
		return m.buildNodeInfo()
	default:
		return m.buildMenu()
	}
}

// View implements tea.Model.
func (m Model) View() string {
	if m.quitting {
		// The goodbye is not a screen: there is no frame, no help line and
		// nothing to navigate. Giving it one would mean a Screen kind that
		// every renderer has to special-case anyway.
		return m.styles.Title.Render("\nDisconnecting. Thanks for calling.\n\n")
	}
	r := ansiRenderer{styles: m.styles, width: m.frameWidth(), height: m.height}
	return r.render(m.Screen())
}

// leave tears the session down cleanly.
func (m Model) leave() (tea.Model, tea.Cmd) {
	// Clear the passphrase before the process forgets about this session. It
	// is the one piece of user secret material the server holds, and only for
	// as long as a session is unlocked (§8.2).
	m.passphrase = ""
	m.unlocked = false
	m.quitting = true
	if m.screen == screenChat {
		m.leaveChat()
	}
	if m.joined && m.cfg.Presence != nil {
		m.cfg.Presence.Leave(m.cfg.SessionID)
	}
	return m, tea.Quit
}

// clockNow reads the injected clock (§12.1), falling back to a real one for
// callers that did not supply it.
func (m Model) clockNow() time.Time {
	if m.cfg.Clock != nil {
		return m.cfg.Clock.Now()
	}
	return clock.NewReal().Now()
}

// location is the timezone every rendered timestamp is formatted in.
//
// It comes from node.timezone rather than the host's local zone, so the same
// message reads the same regardless of where the server runs — and so a test
// asserting on a rendered time is not machine-dependent.
func (m Model) location() *time.Location {
	if m.cfg.Location != nil {
		return m.cfg.Location
	}
	return time.Local
}

// at formats a unix timestamp in the BBS's configured timezone.
func (m Model) at(unix int64, layout string) string {
	return time.Unix(unix, 0).In(m.location()).Format(layout)
}

// sanitize prepares untrusted text for display (§5.4).
func sanitize(s string) string { return term.SanitizeForDisplay(s) }

// sanitizeLine prepares an untrusted single-line field.
func sanitizeLine(s string) string { return term.SanitizeLine(s) }

// truncate shortens a string for a fixed-width column.
func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// contains reports whether a string slice holds a value.
func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
