package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ChatLine is one message in the node chat.
type ChatLine struct {
	At   time.Time
	Node int
	Nick string
	Text string
	// System marks joins, parts and notices rather than user speech.
	System bool
}

// ChatRoom is the multi-node chat shared by every session on this BBS.
//
// It is deliberately in-memory and bounded: node chat is a live conversation
// between people currently connected, not a message base. Anything worth
// keeping belongs in a forum area, which is a signed record and replicates.
type ChatRoom struct {
	mu       sync.RWMutex
	lines    []ChatLine
	max      int
	watchers map[string]chan struct{}
}

// NewChatRoom builds a room holding the last max lines.
func NewChatRoom(max int) *ChatRoom {
	if max <= 0 {
		max = 200
	}
	return &ChatRoom{max: max, watchers: map[string]chan struct{}{}}
}

// Say appends a line and wakes every watcher.
func (c *ChatRoom) Say(line ChatLine) {
	c.mu.Lock()
	line.Text = strings.TrimSpace(line.Text)
	c.lines = append(c.lines, line)
	if len(c.lines) > c.max {
		c.lines = c.lines[len(c.lines)-c.max:]
	}
	watchers := make([]chan struct{}, 0, len(c.watchers))
	for _, ch := range c.watchers {
		watchers = append(watchers, ch)
	}
	c.mu.Unlock()

	// Notify without holding the lock, and never block: a session that is not
	// currently waiting will pick the line up on its next poll.
	for _, ch := range watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Lines returns a snapshot of the recent history.
func (c *ChatRoom) Lines() []ChatLine {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ChatLine, len(c.lines))
	copy(out, c.lines)
	return out
}

// Watch registers a session for wake-ups.
func (c *ChatRoom) Watch(id string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan struct{}, 1)
	c.watchers[id] = ch
	return ch
}

// Unwatch removes a session.
func (c *ChatRoom) Unwatch(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.watchers, id)
}

// ---------------------------------------------------------------------------
// Session integration
// ---------------------------------------------------------------------------

type chatUpdatedMsg struct{ lines []ChatLine }

// enterChat joins the room and starts listening.
func (m Model) enterChat() tea.Cmd {
	if m.cfg.Chat == nil {
		return errs("Chat is not available.")
	}
	m.cfg.Chat.Say(ChatLine{
		At: m.clockNow(), Node: m.nodeNum, Nick: m.nick,
		Text: m.nick + " joined", System: true,
	})
	return tea.Batch(m.pollChat(), m.refreshChat())
}

func (m Model) refreshChat() tea.Cmd {
	return func() tea.Msg {
		if m.cfg.Chat == nil {
			return chatUpdatedMsg{}
		}
		return chatUpdatedMsg{lines: m.cfg.Chat.Lines()}
	}
}

// pollChat waits for someone to speak, then asks for a redraw.
//
// It waits on a channel rather than ticking, so an idle chat costs nothing and
// a message appears immediately. The session context is the other wake-up:
// without it the goroutine would outlive a session that ended without
// unwatching.
//
// Note there is deliberately no timer here. A timeout would need a wall clock,
// and §12.1 forbids reaching for one in domain code — the determinism checker
// catches it, as it did when this was first written with time.After.
func (m Model) pollChat() tea.Cmd {
	if m.cfg.Chat == nil || m.cfg.DisableChatPolling {
		return nil
	}
	ch := m.cfg.Chat.Watch(m.cfg.SessionID)
	return func() tea.Msg {
		select {
		case <-ch:
		case <-m.ctx.Done():
		}
		return chatUpdatedMsg{lines: m.cfg.Chat.Lines()}
	}
}

func (m Model) sendChat(text string) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		m.cfg.Chat.Say(ChatLine{
			At: m.clockNow(), Node: m.nodeNum, Nick: m.nick, Text: text,
		})
		return chatUpdatedMsg{lines: m.cfg.Chat.Lines()}
	}
}

func (m Model) leaveChat() {
	if m.cfg.Chat == nil {
		return
	}
	m.cfg.Chat.Unwatch(m.cfg.SessionID)
	m.cfg.Chat.Say(ChatLine{
		At: m.clockNow(), Node: m.nodeNum, Nick: m.nick,
		Text: m.nick + " left", System: true,
	})
}

func (m Model) handleChatKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.leaveChat()
		m.screen = screenMenu
		m.setWhere("menu")
		return m, nil
	case tea.KeyEnter:
		text := m.chatInput.String()
		m.chatInput.Clear()
		return m, m.sendChat(text)
	}
	m.chatInput, _ = m.chatInput.Update(msg)
	return m, nil
}

func (m Model) renderChat() string {
	var b strings.Builder

	if m.guest {
		b.WriteString(m.styles.Muted.Render("You are a guest — you can read but not speak."))
		b.WriteString("\n\n")
	}

	// Show the tail that fits, leaving room for the input line and chrome.
	visible := m.height - 10
	if visible < 3 {
		visible = 3
	}
	lines := m.chatLines
	if len(lines) > visible {
		lines = lines[len(lines)-visible:]
	}

	if len(lines) == 0 {
		b.WriteString(m.styles.Muted.Render("Nobody has said anything yet."))
		b.WriteString("\n")
	}
	for _, l := range lines {
		ts := l.At.In(m.location()).Format("15:04")
		if l.System {
			b.WriteString(m.styles.Muted.Render(fmt.Sprintf("  %s  * %s", ts, sanitizeLine(l.Text))))
		} else {
			b.WriteString(m.styles.Body.Render(fmt.Sprintf("  %s  %s: %s",
				ts, sanitizeLine(l.Nick), sanitizeLine(l.Text))))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	if m.guest {
		b.WriteString(m.styles.Muted.Render("(register to join in)"))
	} else {
		b.WriteString(m.styles.Accent.Render(m.chatInput.Render()))
	}

	return m.frame("Node Chat", b.String(), "type and press enter · esc leave")
}
