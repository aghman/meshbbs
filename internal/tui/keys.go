package tui

import (
	"strings"

	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// composeState holds an in-progress post or message.
type composeState struct {
	to      textInput
	subject textInput
	body    textArea
	field   int // 0 = to/subject, 1 = subject/body, 2 = body
	area    string
}

func newCompose(withTo bool) composeState {
	c := composeState{
		to:      newInput("To: ", 32, false),
		subject: newInput("Subject: ", 72, false),
		body:    newTextArea(4000),
	}
	if !withTo {
		c.field = 1
	}
	return c
}

// handleKey dispatches by screen.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Any keypress clears a stale status line, so a message from three screens
	// ago is not still on display.
	if m.status != "" && msg.Type != tea.KeyRunes {
		m.status = ""
	}

	switch m.screen {
	case screenSignup:
		return m.handleSignupKey(msg)
	case screenKeyUnknown:
		return m.leave()
	case screenMenu:
		return m.handleMenuKey(msg)
	case screenAreaList:
		return m.handleAreaListKey(msg)
	case screenFileAreaList:
		return m.handleFileAreaListKey(msg)
	case screenFileArea:
		return m.handleFileAreaKey(msg)
	case screenFileInfo:
		return m.handleFileInfoKey(msg)
	case screenFileDescribe:
		return m.handleFileDescribeKey(msg)
	case screenAreaRead:
		return m.handleAreaReadKey(msg)
	case screenPostCompose:
		return m.handleComposeKey(msg, false)
	case screenMailList:
		return m.handleMailListKey(msg)
	case screenMailRead:
		return m.handleMailReadKey(msg)
	case screenMailCompose:
		return m.handleComposeKey(msg, true)
	case screenUnlock:
		return m.handleUnlockKey(msg)
	case screenKeySetup:
		return m.handleKeySetupKey(msg)
	case screenSysop:
		return m.handleSysopKey(msg)
	case screenChat:
		return m.handleChatKey(msg)
	case screenWho, screenNodeInfo:
		m.screen = screenMenu
		return m, nil
	case screenWebEnrol:
		// Clear the code from session memory on the way out. The store holds
		// only its hash, and there is no reason for the plaintext to outlive
		// the screen showing it.
		m.webCode, m.webCodeExpires = "", 0
		m.screen = screenMenu
		return m, nil
	}
	return m, nil
}

func (m Model) handleMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "m":
		m.screen = screenAreaList
		m.areaIdx = 0
		m.setWhere("forums")
		return m, m.loadAreas()
	case "f":
		// "f" used to be an undocumented second binding for the message areas.
		// It belongs to files, which is what anyone typing it means, and the
		// documented "m" was always the one on screen.
		m.screen = screenFileAreaList
		m.fileAreaIdx = 0
		m.setWhere("files")
		return m, m.loadFileAreas()
	case "e":
		if m.guest {
			return m, errs("Guests cannot read mail. Register with `ssh new@` for an account.")
		}
		if !m.unlocked {
			return m, m.checkMailAccess()
		}
		m.screen = screenMailList
		m.setWhere("mail")
		return m, m.loadMail()
	case "w":
		m.screen = screenWho
		return m, m.loadPeers()
	case "c":
		m.screen = screenChat
		m.chatInput = newInput("> ", 200, false)
		m.setWhere("chat")
		return m, m.enterChat()
	case "s":
		if !m.sysop {
			return m, errs("That is a sysop function.")
		}
		m.screen = screenSysop
		m.sysop_ = sysopState{}
		m.setWhere("sysop")
		return m, tea.Batch(m.loadSysopData(), m.loadPeers())
	case "n":
		m.screen = screenNodeInfo
		return m, nil
	case "p":
		if !m.cfg.WebEnabled {
			return m, nil
		}
		return m, m.issueWebCode()
	case "q":
		return m.leave()
	}
	return m, nil
}

func (m Model) handleFileAreaListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.fileAreaIdx > 0 {
			m.fileAreaIdx--
		}
		return m, nil
	case "down", "j":
		if m.fileAreaIdx < len(m.fileAreas)-1 {
			m.fileAreaIdx++
		}
		return m, nil
	case "enter":
		if len(m.fileAreas) == 0 {
			return m, nil
		}
		area := m.fileAreas[m.fileAreaIdx]
		m.screen = screenFileArea
		m.setWhere("files:" + area.Name)
		return m, m.loadFiles(area.Name)
	case "q", "esc":
		m.screen = screenMenu
		m.setWhere("menu")
		return m, nil
	}
	return m, nil
}

func (m Model) handleFileAreaKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.fileIdx > 0 {
			m.fileIdx--
		}
		return m, nil
	case "down", "j":
		if m.fileIdx < len(m.files)-1 {
			m.fileIdx++
		}
		return m, nil
	case "enter":
		if len(m.files) == 0 {
			return m, nil
		}
		m.screen = screenFileInfo
		return m, nil
	case "d":
		return m.startDescribe()
	case "q", "esc":
		m.screen = screenFileAreaList
		m.setWhere("files")
		return m, m.loadFileAreas()
	}
	return m, nil
}

// handleFileInfoKey handles the file detail screen.
//
// Any key used to return to the listing. That was fine while the screen was
// purely a display, and it is why "d" needs a real handler here rather than
// another arm of the dispatch: without one, pressing "d" on the screen showing
// "(no description)" would navigate away, which is the opposite of what the
// person pressing it wants.
func (m Model) handleFileInfoKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if strings.ToLower(msg.String()) == "d" {
		return m.startDescribe()
	}
	m.screen = screenFileArea
	return m, nil
}

// startDescribe opens the description editor for the selected file.
//
// The permission check is here rather than only in the view, because a hidden
// hint is not a permission check: the browser front end sends the same key
// names an SSH user can type (webui.md §5), so "d" arrives whether or not a
// hint advertised it.
func (m Model) startDescribe() (tea.Model, tea.Cmd) {
	if m.fileIdx < 0 || m.fileIdx >= len(m.files) {
		return m, nil
	}
	f := m.files[m.fileIdx]
	if !f.MayDescribe(m.nick, m.sysop) {
		if m.guest {
			return m, errs("Guests cannot describe files. Register with `ssh new@` for an account.")
		}
		return m, errs("You can only describe files you uploaded. Ask the sysop for anything else.")
	}

	m.descInput = newInput("Description: ", store.MaxFileDescLen, false)
	m.descInput.setValue(f.Description)
	m.screen = screenFileDescribe
	return m, nil
}

func (m Model) handleFileDescribeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.screen = screenFileInfo
		return m, nil
	case tea.KeyEnter:
		if m.fileIdx < 0 || m.fileIdx >= len(m.files) {
			m.screen = screenFileArea
			return m, nil
		}
		f := m.files[m.fileIdx]
		area, text := m.fileArea, m.descInput.String()
		m.screen = screenFileInfo
		// One command, not a sequence — see fileDescribedMsg on why the
		// write and the reload cannot be two.
		return m, m.describeFile(area, f.Name, text)
	}
	m.descInput, _ = m.descInput.Update(msg)
	return m, nil
}

func (m Model) handleAreaListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.areaIdx > 0 {
			m.areaIdx--
		}
		return m, nil
	case "down", "j":
		if m.areaIdx < len(m.areas)-1 {
			m.areaIdx++
		}
		return m, nil
	case "enter":
		if len(m.areas) == 0 {
			return m, nil
		}
		m.screen = screenAreaRead
		area := m.areas[m.areaIdx]
		m.setWhere("area:" + area.Name)
		return m, m.loadPosts(area.Name)
	case "q", "esc":
		m.screen = screenMenu
		m.setWhere("menu")
		return m, nil
	}
	return m, nil
}

func (m Model) handleAreaReadKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "up", "k":
		if m.postIdx > 0 {
			m.postIdx--
		}
		return m, nil
	case "down", "j":
		if m.postIdx < len(m.posts)-1 {
			m.postIdx++
		}
		return m, nil
	case "p":
		if m.guest {
			return m, errs("Guests cannot post. Register with `ssh new@` for an account.")
		}
		m.screen = screenPostCompose
		m.compose = newCompose(false)
		m.compose.area = m.areas[m.areaIdx].Name
		return m, nil
	case "q", "esc":
		m.screen = screenAreaList
		m.setWhere("forums")
		return m, nil
	}
	return m, nil
}

func (m Model) handleComposeKey(msg tea.KeyMsg, isMail bool) (tea.Model, tea.Cmd) {
	c := m.compose

	switch msg.Type {
	case tea.KeyEsc:
		if isMail {
			m.screen = screenMailList
		} else {
			m.screen = screenAreaRead
		}
		return m, nil

	case tea.KeyTab:
		c.field++
		maxField := 2
		if !isMail {
			maxField = 2
		}
		if c.field > maxField {
			c.field = 0
			if !isMail {
				c.field = 1
			}
		}
		m.compose = c
		return m, nil

	case tea.KeyCtrlD:
		// Ctrl+D sends, because Enter has to insert a newline in the body.
		if strings.TrimSpace(c.body.String()) == "" {
			return m, errs("Nothing to send — the message is empty.")
		}
		if isMail {
			to := strings.TrimSpace(c.to.String())
			if to == "" {
				return m, errs("No recipient.")
			}
			m.screen = screenMailList
			m.compose = newCompose(true)
			// Sequence, not Batch: Batch runs both concurrently, so the
			// reload can read the list before the send has committed and the
			// user sees their own message missing.
			return m, tea.Sequence(m.sendMail(to, c.subject.String(), c.body.String()), m.loadMail())
		}
		area := c.area
		m.screen = screenAreaRead
		m.compose = newCompose(false)
		// Sequence, not Batch — see the comment on the mail path above.
		return m, tea.Sequence(m.submitPost(area, c.subject.String(), c.body.String()), m.loadPosts(area))
	}

	// Enter advances out of the single-line fields. Only the body treats it as
	// a newline — without this, typing a recipient and pressing enter silently
	// did nothing, and the subject and body then concatenated into the To
	// field.
	switch c.field {
	case 0:
		if msg.Type == tea.KeyEnter {
			c.field = 1
		} else {
			c.to, _ = c.to.Update(msg)
		}
	case 1:
		if msg.Type == tea.KeyEnter {
			c.field = 2
		} else {
			c.subject, _ = c.subject.Update(msg)
		}
	default:
		c.body, _ = c.body.Update(msg)
	}
	m.compose = c
	return m, nil
}

func (m Model) handleMailListKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "up", "k":
		if m.mailIdx > 0 {
			m.mailIdx--
		}
		return m, nil
	case "down", "j":
		if m.mailIdx < len(m.mail)-1 {
			m.mailIdx++
		}
		return m, nil
	case "enter":
		if len(m.mail) == 0 {
			return m, nil
		}
		return m, m.openMail(m.mail[m.mailIdx])
	case "c":
		m.screen = screenMailCompose
		m.compose = newCompose(true)
		return m, nil
	case "q", "esc":
		m.screen = screenMenu
		m.setWhere("menu")
		return m, nil
	}
	return m, nil
}

func (m Model) handleMailReadKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch strings.ToLower(msg.String()) {
	case "r":
		m.screen = screenMailCompose
		m.compose = newCompose(true)
		m.compose.to.value = m.mail[m.mailIdx].Sender
		m.compose.subject.value = "Re: " + m.mailSubject
		m.compose.field = 2
		return m, nil
	default:
		m.screen = screenMailList
		m.mailBody, m.mailSubject = "", ""
		return m, m.loadMail()
	}
}

func (m Model) handleUnlockKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.screen = screenMenu
		return m, nil
	case tea.KeyEnter:
		return m, m.tryUnlock(m.unlockPW.String())
	}
	m.unlockPW, _ = m.unlockPW.Update(msg)
	return m, nil
}

func (m Model) handleKeySetupKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.screen = screenMenu
		return m, nil
	case tea.KeyEnter:
		if m.setupIdx == 0 {
			if len(m.setupPW.String()) < 8 {
				return m, errs("Passphrase must be at least 8 characters.")
			}
			m.setupIdx = 1
			return m, nil
		}
		if m.setupPW.String() != m.setupPW2.String() {
			m.setupPW2.Clear()
			return m, errs("Passphrases do not match.")
		}
		return m, m.createDMKey(m.setupPW.String())
	}
	if m.setupIdx == 0 {
		m.setupPW, _ = m.setupPW.Update(msg)
	} else {
		m.setupPW2, _ = m.setupPW2.Update(msg)
	}
	return m, nil
}

func (m *Model) setWhere(where string) {
	if m.joined && m.cfg.Presence != nil {
		m.cfg.Presence.SetLocation(m.cfg.SessionID, where)
	}
}
