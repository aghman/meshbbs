package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Key names as a browser reports them, mapped onto the keys the model already
// handles (webui.md §5).
//
// This is the whole of the browser's input vocabulary. Clicking the [M] menu
// row sends "m"; tapping the `enter open` button sends "enter". The model never
// learns it was a click, which is why handleKey needed no web-specific branch
// and why an SSH user and a browser user cannot end up able to do different
// things.

var namedKeys = map[string]tea.KeyType{
	"enter":     tea.KeyEnter,
	"escape":    tea.KeyEsc,
	"esc":       tea.KeyEsc,
	"tab":       tea.KeyTab,
	"shift+tab": tea.KeyShiftTab,
	"backspace": tea.KeyBackspace,
	"delete":    tea.KeyDelete,
	"space":     tea.KeySpace,
	"up":        tea.KeyUp,
	"down":      tea.KeyDown,
	"left":      tea.KeyLeft,
	"right":     tea.KeyRight,
	"home":      tea.KeyHome,
	"end":       tea.KeyEnd,
	"pgup":      tea.KeyPgUp,
	"pgdown":    tea.KeyPgDown,

	// Ctrl chords the BBS actually binds. Deliberately not a general
	// "ctrl+<any letter>" rule: the browser should only be able to send
	// bindings this UI defines, so a malformed or hostile frame cannot reach
	// some control code the terminal path would never produce.
	"ctrl+c": tea.KeyCtrlC,
	"ctrl+d": tea.KeyCtrlD,
	"ctrl+u": tea.KeyCtrlU,
}

// keyMsg converts a browser key name into a keypress.
//
// Unknown names are REFUSED rather than guessed at. A key the UI does not bind
// has no meaning to send, and silently inventing one is how a stray frame ends
// up navigating somebody's session.
func keyMsg(name string) (tea.KeyMsg, bool) {
	if name == "" {
		return tea.KeyMsg{}, false
	}
	if t, ok := namedKeys[strings.ToLower(name)]; ok {
		return tea.KeyMsg{Type: t}, true
	}

	// A single printable character is a rune press. Anything longer that is not
	// a known name is not a key.
	r := []rune(name)
	if len(r) != 1 {
		return tea.KeyMsg{}, false
	}
	if r[0] < 0x20 || r[0] == 0x7f {
		return tea.KeyMsg{}, false
	}
	if r[0] == ' ' {
		return tea.KeyMsg{Type: tea.KeySpace}, true
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: r}, true
}

// selectIndex moves the current screen's list cursor.
//
// Out-of-range values are clamped rather than refused. The browser's idea of
// how long a list is can lag the model's by one update — somebody posts while
// a click is in flight — and clamping lands on a sensible row where rejecting
// would leave the cursor somewhere the user did not click.
func (m Model) selectIndex(i int) Model {
	clamp := func(i, n int) int {
		if i < 0 {
			return 0
		}
		if i >= n {
			return n - 1
		}
		return i
	}

	switch m.screen {
	case screenAreaList:
		if len(m.areas) > 0 {
			m.areaIdx = clamp(i, len(m.areas))
		}
	case screenMailList:
		if len(m.mail) > 0 {
			m.mailIdx = clamp(i, len(m.mail))
		}
	case screenAreaRead:
		if len(m.posts) > 0 {
			m.postIdx = clamp(i, len(m.posts))
		}
	case screenSysop:
		switch m.sysop_.tab {
		case 0:
			if len(m.sysop_.users) > 0 {
				m.sysop_.userIdx = clamp(i, len(m.sysop_.users))
			}
		case 1:
			if len(m.sysop_.areas) > 0 {
				m.sysop_.areaIdx = clamp(i, len(m.sysop_.areas))
			}
		case 2:
			if len(m.sysop_.aliases) > 0 {
				m.sysop_.aliasIdx = clamp(i, len(m.sysop_.aliases))
			}
		}
	}
	return m
}

// setField sets a named input to a whole value (webui.md §5.1).
//
// The names match the Field.Name values the Screen advertises, so the browser
// echoes back what it was given rather than knowing anything about the model's
// internals.
func (m Model) setField(name, value string) Model {
	switch m.screen {
	case screenPostCompose, screenMailCompose:
		switch name {
		case "to":
			m.compose.to.setValue(value)
			m.compose.field = 0
		case "subject":
			m.compose.subject.setValue(value)
			m.compose.field = 1
		case "body":
			m.compose.body.setValue(value)
			m.compose.field = 2
		}

	case screenUnlock:
		if name == "passphrase" {
			m.unlockPW.setValue(value)
		}

	case screenKeySetup:
		switch name {
		case "passphrase":
			m.setupPW.setValue(value)
			m.setupIdx = 0
		case "confirm":
			m.setupPW2.setValue(value)
			m.setupIdx = 1
		}

	case screenChat:
		if name == "say" {
			m.chatInput.setValue(value)
		}

	case screenSignup:
		switch name {
		case "nick":
			m.signup.nick.setValue(value)
		case "password":
			m.signup.pass.setValue(value)
		case "password2":
			m.signup.pass2.setValue(value)
		case "passphrase":
			m.signup.phrase.setValue(value)
		case "passphrase2":
			m.signup.phrase2.setValue(value)
		}
	}
	return m
}
