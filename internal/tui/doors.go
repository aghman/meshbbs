package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// Doors in the session layer (§9.1).
//
// # Why launching is injected and listing is not
//
// Everything else on this menu is the same everywhere: a post is a post over
// SSH, over telnet and in a browser, because the model emits a Screen and each
// renderer draws it (webui.md §2). A door is not. It is a third-party program
// writing raw bytes at a terminal, and a browser does not have one — [D16]
// deleted the terminal emulator on purpose, and a screenful of ANSI is exactly
// what the Screen abstraction exists to avoid re-inventing.
//
// So the door LIST is ordinary model code, identical in all three front ends,
// and only the act of running one goes through an injected launcher. That keeps
// the divergence to a single named seam instead of letting it spread: a browser
// user can see which doors this BBS has, read what they are, and be told
// precisely how to play them, rather than finding a menu entry missing and
// having no way to know it ever existed.
//
// The seam is the same shape as PresenceTracker, which is the other thing a
// front end has to supply.

// DoorLauncher runs a door on this session's terminal.
type DoorLauncher interface {
	// CanRun reports whether this front end can give a door a terminal. When it
	// cannot, the second return says what to tell the user instead — which is
	// shown on the list, before they try, rather than only after.
	CanRun() (bool, string)

	// Launch blocks until the door has finished and returns a line to show the
	// user. An error means the door could not be started at all, which is a
	// different thing from a door that ran and failed.
	Launch(ctx context.Context, d store.Door, sess DoorSession) (string, error)
}

// DoorSession is what a launcher needs to know about who is playing.
type DoorSession struct {
	Nick     string
	RealName string
	Node     int
	Width    int
	Height   int
	Term     string
	ANSI     bool
	Encoding string
	// Sysop selects the security level a dropfile reports (§9.2). It is not a
	// gate: what this caller may run was settled before the launcher was asked.
	Sysop bool
	// TimeRemaining reports the session's remaining time and whether it is
	// limited at all. A door handed a bare zero cannot tell "no limit" from
	// "no time left" (§9.1).
	TimeRemaining func() (time.Duration, bool)
}

type (
	doorsLoadedMsg struct{ doors []store.Door }
	doorDoneMsg    struct {
		status string
		isErr  bool
	}
)

// loadDoors lists the doors a caller could run here.
//
// Disabled doors are left out, because a sysop turning one off means it is not
// on offer. Doors the user lacks the capability for are NOT left out: the
// board's software is public information, and a menu that silently omits things
// leaves someone unable to ask for what they cannot see. The refusal, with the
// capability named, comes when they try — which is how §6.7's other gates work.
func (m Model) loadDoors() tea.Cmd {
	return func() tea.Msg {
		if m.cfg.Store == nil {
			return doorsLoadedMsg{}
		}
		all, err := m.cfg.Store.ListDoors(m.ctx)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		out := make([]store.Door, 0, len(all))
		for _, d := range all {
			if d.Enabled {
				out = append(out, d)
			}
		}
		return doorsLoadedMsg{doors: out}
	}
}

// mayRunDoor reports whether this user may run a door, and why not.
func (m Model) mayRunDoor(ctx context.Context, d store.Door) error {
	if m.guest {
		return fmt.Errorf("Guests cannot run doors. Register with `ssh new@` for an account.")
	}
	if m.cfg.Store == nil {
		return fmt.Errorf("Doors are not available on this BBS.")
	}
	for _, capability := range []string{store.CapRunDoors, d.RequiredCapability} {
		if capability == "" {
			continue
		}
		ok, err := m.cfg.Store.HasCapability(ctx, m.nick, capability)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("You need the %s capability to run %s. Ask the sysop.",
				capability, d.Name)
		}
	}
	return nil
}

// launchDoor runs a door and reports how it went.
//
// It blocks for the door's whole lifetime, which is correct: this is a command,
// commands run on their own goroutine, and the connection has been lent to the
// door for the duration. The model is still live underneath — its chat watcher
// keeps firing — which is exactly why the connection mux discards whatever it
// renders until the door is finished.
func (m Model) launchDoor(d store.Door) tea.Cmd {
	sess := DoorSession{
		Nick: m.nick, RealName: m.cfg.User.DisplayName, Node: m.nodeNum,
		Width: m.width, Height: m.height,
		Term: m.cfg.TermType, ANSI: true, Encoding: m.cfg.Encoding.String(),
		Sysop:         m.sysop,
		TimeRemaining: m.Remaining,
	}
	launcher, ctx := m.cfg.Doors, m.ctx
	return func() tea.Msg {
		if err := m.mayRunDoor(ctx, d); err != nil {
			return doorDoneMsg{status: err.Error(), isErr: true}
		}
		if launcher == nil {
			return doorDoneMsg{
				status: "Doors are not available on this connection.", isErr: true}
		}
		status, err := launcher.Launch(ctx, d, sess)
		if err != nil {
			return doorDoneMsg{status: err.Error(), isErr: true}
		}
		return doorDoneMsg{status: status}
	}
}

func (m Model) handleDoorListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.doorIdx > 0 {
			m.doorIdx--
		}
		return m, nil
	case "down", "j":
		if m.doorIdx < len(m.doors)-1 {
			m.doorIdx++
		}
		return m, nil
	case "enter":
		if len(m.doors) == 0 || m.doorIdx >= len(m.doors) {
			return m, nil
		}
		d := m.doors[m.doorIdx]
		m.status, m.statusErr = "Starting "+d.Name+"…", false
		m.setWhere("door:" + d.Name)
		return m, m.launchDoor(d)
	case "q", "esc":
		m.screen = screenMenu
		m.setWhere("menu")
		return m, nil
	}
	return m, nil
}

func (m Model) buildDoorList() Screen {
	rows := make([]Row, 0, len(m.doors))
	for _, d := range m.doors {
		rows = append(rows, Row{Cells: []string{
			sanitizeLine(d.Name),
			doorNote(d),
		}})
	}

	blocks := []Block{
		TableBlock{
			Header:   []string{"Door", "Notes"},
			Columns:  []Column{{Width: 16}, {Width: 46}},
			Rows:     rows,
			Selected: m.doorIdx,
			Gap:      2,
			Empty:    "No doors are installed on this BBS.",
		},
	}

	// Said here, on the list, rather than only when someone presses enter: a
	// browser user should learn that doors need a terminal before choosing one,
	// not after.
	if m.cfg.Doors != nil {
		if ok, why := m.cfg.Doors.CanRun(); !ok && len(m.doors) > 0 {
			blocks = append(blocks, Prose(LevelMuted, why))
		}
	}

	return Screen{
		Kind: "doorlist", Title: "Door Games", Blocks: blocks,
		Status: m.statusLine(),
		Help:   hints("up/down", "move", "enter", "play", "q", "back"),
	}
}

// doorNote is the short right-hand column: what a caller needs to know about a
// door before choosing it.
func doorNote(d store.Door) string {
	var parts []string
	if d.RequiredCapability != "" {
		parts = append(parts, "needs "+d.RequiredCapability)
	}
	if d.NodeLock {
		parts = append(parts, "one player per node")
	}
	if d.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("up to %d at once", d.MaxConcurrent))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}
