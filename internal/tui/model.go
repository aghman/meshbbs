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
	"github.com/aghman/meshbbs/internal/record"
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
	screenWebEnrol
	screenFileAreaList
	screenFileArea
	screenFileInfo
	screenFileDescribe
	screenDoorList
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
	// WebEnabled reveals the passkey-enrolment path. The menu item is hidden
	// when the sysop has not turned the web UI on, because a code that leads
	// nowhere is worse than no offer at all.
	WebEnabled bool
	// WebURL is where the code gets typed, shown alongside it.
	WebURL string
	// SSHPort is the port this instance listens on, so the file browser can
	// show a fetch command that actually works. Files move over SFTP, never
	// through the TUI (§5.1) — so a browser that cannot say how to get a file
	// is only half a browser.
	SSHPort   int
	Intent    Intent
	Nick      string
	User      store.User
	PublicKey string
	KeyFP     string
	Chat      *ChatRoom
	Clock     clock.Clock
	Location  *time.Location
	// SessionLimit ends a session after this long (§11.5). Zero means no
	// limit, which is the default and what every board ran on until someone
	// needed the lines back.
	SessionLimit time.Duration
	// Doors runs a door on this session's terminal, or explains why this front
	// end cannot. Nil means this connection offers no doors at all.
	Doors DoorLauncher
	// TermType is the client's declared terminal, passed through to doors as
	// TERM. Empty is normal for a browser, which has no such thing.
	TermType string
	// DisableWatchers stops a session from starting its background watchers:
	// the chat poller and the session-time watcher.
	//
	// It exists for tests. Both park on something that has not happened yet —
	// somebody speaking, or the clock reaching the next mark — which is correct
	// in a real program, where commands run in goroutines, and a stall in a
	// harness that drives Update synchronously and waits out a timeout on each
	// one. Tests advance the clock and send the message instead.
	DisableWatchers bool
	Logger          *slog.Logger
	Ctx             context.Context
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

	// doors installed on this BBS, and the cursor within them.
	doors   []store.Door
	doorIdx int

	// startedAt is when this session began, for the time limit. Read from the
	// injected clock, so a virtual one makes a two-hour limit testable.
	startedAt time.Time
	// timeWarned is the warning mark already given, or zero. Marks are
	// descending, so "already warned at 5m" is timeWarned == 5m.
	timeWarned time.Duration
	// farewell is what the user is told on the way out, when it is not the
	// ordinary goodbye.
	farewell string

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
	fileAreas   []store.Area
	fileAreaIdx int
	files       []store.CatalogEntry
	fileIdx     int
	// fileArea is the area the file list belongs to, held because the list
	// itself carries the name on every row and the title needs it once.
	fileArea string
	// descInput is an in-progress file description. It holds the file's
	// current text when the screen opens, so "d" is an edit rather than a
	// retype — most uses of this are fixing a word, not writing from scratch.
	descInput textInput
	// requested is the content this session's user has already asked for
	// (§6.5), keyed by the truncated hash a catalog entry carries. Held as a
	// set rather than re-queried per row: the file browser asks the question
	// once per file drawn, and the answer changes only when this session makes
	// a request.
	requested map[[record.FileHashLen]byte]bool
	// arrivals are requests that have landed and not been mentioned to this
	// user yet. Shown on the menu once and then marked told, because the
	// person who asked was not there when a sysop imported the stick.
	arrivals  []store.FileRequest
	sysop_    sysopState
	chatInput textInput
	chatLines []ChatLine
	// webCode is a live passkey-enrolment code ([D18]). It is shown once and
	// held only to keep it on screen; the store keeps nothing but its hash.
	webCode        string
	webCodeExpires int64

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
	if m.screen == screenMenu {
		m.join()
	}
	m.startedAt = m.clockNow()
	return m
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{}
	if m.screen == screenMenu {
		cmds = append(cmds, m.loadAreas())
		// A guest has no account to have asked with, so there is nothing to
		// read and nobody to tell (§6.5).
		if !m.guest {
			cmds = append(cmds, m.loadFileRequests())
		}
	}
	// Started here rather than at the first keypress: a session that connects
	// and sits there is exactly the one a time limit is for.
	if cmd := m.watchTimeLimit(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	switch len(cmds) {
	case 0:
		return nil
	case 1:
		return cmds[0]
	default:
		return tea.Batch(cmds...)
	}
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

	case timeCheckMsg:
		return m.enforceTimeLimit()

	case doorsLoadedMsg:
		m.doors = msg.doors
		if m.doorIdx >= len(m.doors) {
			m.doorIdx = 0
		}
		return m, nil

	case doorDoneMsg:
		// Back on the list with the outcome. The door owned the screen while it
		// ran and the renderer's output was discarded, so what the user is
		// looking at is whatever the door left — the front end repaints.
		m.status, m.statusErr = msg.status, msg.isErr
		m.setWhere("doors")
		return m, nil

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

	case fileAreasLoadedMsg:
		m.fileAreas = msg.areas
		if m.fileAreaIdx >= len(m.fileAreas) {
			m.fileAreaIdx = 0
		}
		return m, nil

	case filesLoadedMsg:
		m.files, m.fileIdx, m.fileArea = msg.files, 0, msg.area
		return m, nil

	case fileRequestsLoadedMsg:
		m.requested, m.arrivals = msg.requested, msg.arrivals
		return m, nil

	case fileRequestedMsg:
		// Recorded in the session's own set rather than re-read, so the
		// listing under the cursor updates without a round trip. The store is
		// the truth and this is a copy of one bit of it, which is safe because
		// the only thing that sets that bit is this session.
		if m.requested == nil {
			m.requested = map[[record.FileHashLen]byte]bool{}
		}
		m.requested[msg.hash] = true
		m.status, m.statusErr = msg.status.text, msg.status.isErr
		return m, nil

	case fileDescribedMsg:
		// Unlike a plain load, this keeps the cursor on the file rather than
		// resetting to the top. The user is looking at the detail screen for
		// the file they just described, and moving the selection out from under
		// them would redraw it as a different file.
		name := ""
		if m.fileIdx >= 0 && m.fileIdx < len(m.files) {
			name = m.files[m.fileIdx].Name
		}
		m.files, m.fileArea = msg.files, msg.area
		m.fileIdx = 0
		for i, f := range msg.files {
			if f.Name == name {
				m.fileIdx = i
				break
			}
		}
		m.status, m.statusErr = msg.status.text, msg.status.isErr
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

	case webCodeMsg:
		m.webCode, m.webCodeExpires = msg.code, msg.expires
		m.screen = screenWebEnrol
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
		m.join()
		return m, m.loadAreas()
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
	case screenWebEnrol:
		return m.buildWebEnrol()
	case screenFileAreaList:
		return m.buildFileAreaList()
	case screenFileArea:
		return m.buildFileArea()
	case screenFileInfo:
		return m.buildFileInfo()
	case screenFileDescribe:
		return m.buildFileDescribe()
	case screenDoorList:
		return m.buildDoorList()
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
		goodbye := m.farewell
		if goodbye == "" {
			goodbye = "Disconnecting. Thanks for calling."
		}
		return m.styles.Title.Render("\n" + goodbye + "\n\n")
	}
	r := ansiRenderer{styles: m.styles, width: m.frameWidth(), height: m.height}
	return r.render(m.Screen())
}

// join registers this session with the presence tracker and takes its node
// number.
//
// It is a direct call rather than a tea.Cmd, and that is the whole point.
// Leave and SetLocation are direct calls, so a join that lands on a command
// goroutine can be overtaken by the departure for the same session: a browser
// tab closed in the moments after connecting ran leave() while joined was
// still false, skipped Presence.Leave, and then the join goroutine registered a
// node that nothing would ever remove — a permanent ghost in who's-online.
// Joining where the model is built keeps registration ordered before every
// path that can unregister it, since those all run under the driver's lock.
//
// Presence is an in-memory registry (sshd.Presence), so there is nothing here
// worth taking off the pump for.
func (m *Model) join() {
	if m.joined {
		return
	}
	m.joined = true
	m.nodeNum = 1
	if m.cfg.Presence != nil {
		m.nodeNum = m.cfg.Presence.Join(m.cfg.SessionID, m.nick, m.cfg.Remote, m.guest)
	}
}

// leaveBecause ends the session with something other than the usual goodbye.
//
// The reason matters more than it looks. A caller dropped without explanation
// assumes a fault and reconnects, which on a board with a time limit is exactly
// the behaviour the limit was meant to prevent.
func (m Model) leaveBecause(reason string) (tea.Model, tea.Cmd) {
	m.farewell = reason
	return m.leave()
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
