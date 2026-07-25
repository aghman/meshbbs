package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// frame wraps a screen with a title bar and status line.
func (m Model) frame(title string, body string, help string) string {
	var b strings.Builder

	b.WriteString(m.styles.Title.Render(title))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render(strings.Repeat("-", minInt(m.frameWidth(), 78))))
	b.WriteString("\n\n")
	b.WriteString(body)
	b.WriteString("\n")

	if m.status != "" {
		b.WriteString("\n")
		// WRAP the status, never truncate it. These messages carry the remedy
		// for whatever just failed — "ask the sysop for the post_federated
		// capability" is the whole point of the [N7] refusal — and clipping
		// them at the terminal edge throws away the actionable half.
		style := m.styles.Success
		prefix := "* "
		if m.statusErr {
			style, prefix = m.styles.Error, "! "
		}
		b.WriteString(style.Width(m.frameWidth()).Render(prefix + sanitize(m.status)))
		b.WriteString("\n")
	}
	if help != "" {
		b.WriteString("\n")
		b.WriteString(m.styles.StatusBar.Render(help))
	}

	// Clamp to the terminal width. §5.4 sets 80x24 as the fallback floor, but
	// people connect from phones and narrow splits, and a line that overflows
	// wraps into a garbled second line rather than being merely cut off.
	// MaxWidth is ANSI-aware, so this truncates visible columns without
	// severing a colour escape.
	return lipgloss.NewStyle().MaxWidth(m.frameWidth()).Render(b.String())
}

// frameWidth is the usable width, with a sane floor so a client reporting a
// nonsense size does not produce a one-column screen.
func (m Model) frameWidth() int {
	if m.width < 20 {
		return 20
	}
	return m.width
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (m Model) renderMenu() string {
	var b strings.Builder

	who := m.nick
	if m.guest {
		who += " (guest, read-only)"
	}
	b.WriteString(m.styles.Heading.Render("You are " + who))
	if m.nodeNum > 0 {
		b.WriteString(m.styles.Muted.Render(fmt.Sprintf("  on node %d", m.nodeNum)))
	}
	b.WriteString("\n\n")

	items := [][2]string{
		{"M", "Message areas — read and post"},
		{"E", "Electronic mail — your private messages"},
		{"C", "Chat with everyone online"},
		{"W", "Who else is online"},
		{"N", "This node's identity"},
	}
	if m.sysop {
		items = append(items, [2]string{"S", "Sysop panel"})
	}
	items = append(items, [2]string{"Q", "Quit"})
	for _, it := range items {
		b.WriteString("  ")
		b.WriteString(m.styles.Accent.Render("[" + it[0] + "]"))
		b.WriteString(" ")
		b.WriteString(m.styles.Body.Render(it[1]))
		b.WriteString("\n")
	}

	if m.guest {
		b.WriteString("\n")
		b.WriteString(m.styles.Muted.Render(
			"You are browsing as a guest. Run `ssh new@" + "this-bbs` to register."))
		b.WriteString("\n")
	}

	return m.frame("MeshBBS", b.String(), "Press a highlighted letter. Ctrl+C to disconnect.")
}

func (m Model) renderAreaList() string {
	var b strings.Builder
	if len(m.areas) == 0 {
		b.WriteString(m.styles.Muted.Render("No message areas yet."))
	}
	for i, a := range m.areas {
		line := fmt.Sprintf("%-16s  %-26s  %s",
			truncate(a.Name, 16), truncate(sanitizeLine(a.Description), 26), a.Scope())
		if i == m.areaIdx {
			b.WriteString(m.styles.Selected.Render("> " + line))
		} else {
			b.WriteString(m.styles.Body.Render("  " + line))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render(
		"\"Local to this BBS\" means posts stay here. \"Federated\" means they travel\n" +
			"the mesh and spend shared airtime, which needs the post_federated capability."))

	return m.frame("Message Areas", b.String(), "up/down move · enter open · q back")
}

func (m Model) renderAreaRead() string {
	area := ""
	scope := ""
	if len(m.areas) > 0 {
		area = m.areas[m.areaIdx].Name
		scope = m.areas[m.areaIdx].Scope()
	}

	var b strings.Builder
	// [N7]'s UI burden: state the area's reach so a user who posts and sees
	// nothing federate is never left guessing.
	b.WriteString(m.styles.Muted.Render("Scope: " + scope))
	b.WriteString("\n\n")

	if len(m.posts) == 0 {
		b.WriteString(m.styles.Muted.Render("No posts yet. Press P to write the first one."))
	} else {
		p := m.posts[m.postIdx]
		b.WriteString(m.styles.Heading.Render(sanitizeLine(p.Subject)))
		b.WriteString("\n")
		b.WriteString(m.styles.Muted.Render(fmt.Sprintf("from %s · %s · message %d of %d",
			sanitizeLine(p.Author),
			m.at(int64(p.TS), "2006-01-02 15:04"),
			m.postIdx+1, len(m.posts))))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Body.Render(sanitize(p.Body)))
	}

	return m.frame("Area: "+area, b.String(), "up/down previous·next · p post · q back")
}

func (m Model) renderCompose() string {
	c := m.compose
	var b strings.Builder

	b.WriteString(m.styles.Muted.Render("Posting to " + c.area))
	b.WriteString("\n\n")
	b.WriteString(m.fieldLine(c.subject.Render(), c.field == 1))
	b.WriteString("\n\n")
	b.WriteString(m.fieldLine(c.body.Render(), c.field == 2))

	return m.frame("New Post", b.String(),
		"tab next field · ctrl+d post · esc cancel")
}

func (m Model) renderMailCompose() string {
	c := m.compose
	var b strings.Builder

	b.WriteString(m.fieldLine(c.to.Render(), c.field == 0))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render(
		"  a nick, or nick@node — node may be an alias your sysop set up"))
	b.WriteString("\n\n")
	b.WriteString(m.fieldLine(c.subject.Render(), c.field == 1))
	b.WriteString("\n\n")
	b.WriteString(m.fieldLine(c.body.Render(), c.field == 2))

	return m.frame("New Message", b.String(),
		"tab next field · ctrl+d send · esc cancel")
}

func (m Model) fieldLine(s string, active bool) string {
	if active {
		return m.styles.Accent.Render(s)
	}
	return m.styles.Body.Render(s)
}

func (m Model) renderMailList() string {
	var b strings.Builder
	if len(m.mail) == 0 {
		b.WriteString(m.styles.Muted.Render("No messages."))
	}
	for i, d := range m.mail {
		flag := " "
		if d.Unread() {
			flag = "*"
		}
		// The subject is encrypted with the body, so it is genuinely unknown
		// until the message is opened. Saying so is more honest than showing
		// a blank column.
		subject := "(encrypted — press enter to read)"
		line := fmt.Sprintf("%s %-14s %-34s %s",
			flag, truncate(sanitizeLine(d.Sender), 14),
			truncate(subject, 34),
			m.at(d.SentAt, "01-02 15:04"))
		if i == m.mailIdx {
			b.WriteString(m.styles.Selected.Render("> " + line))
		} else {
			b.WriteString(m.styles.Body.Render("  " + line))
		}
		b.WriteString("\n")
	}
	return m.frame("Mail", b.String(), "up/down move · enter read · c compose · q back")
}

func (m Model) renderMailRead() string {
	if len(m.mail) == 0 {
		return m.frame("Mail", "", "q back")
	}
	d := m.mail[m.mailIdx]
	var b strings.Builder
	b.WriteString(m.styles.Heading.Render(sanitizeLine(m.mailSubject)))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render("from " + sanitizeLine(d.Sender) + " · " +
		m.at(d.SentAt, "2006-01-02 15:04")))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Body.Render(sanitize(m.mailBody)))
	return m.frame("Message", b.String(), "r reply · any other key back")
}

func (m Model) renderUnlock() string {
	var b strings.Builder
	b.WriteString(m.styles.Body.Render(
		"Your messages are encrypted with a key that only your passphrase opens."))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render(
		"The sysop stores that key encrypted and cannot read your mail without it."))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Accent.Render(m.unlockPW.Render()))
	return m.frame("Unlock Mail", b.String(), "enter unlock · esc back")
}

func (m Model) renderKeySetup() string {
	var b strings.Builder
	b.WriteString(m.styles.Body.Render("You do not have a message key yet."))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render(
		"Your private messages are encrypted with a key only your passphrase opens.\n" +
			"The sysop stores it encrypted and cannot read your mail."))
	b.WriteString("\n\n")
	if m.setupIdx == 0 {
		b.WriteString(m.styles.Accent.Render(m.setupPW.Render()))
	} else {
		b.WriteString(m.styles.Muted.Render("Passphrase: " + strings.Repeat("*", len(m.setupPW.String()))))
		b.WriteString("\n")
		b.WriteString(m.styles.Accent.Render(m.setupPW2.Render()))
	}
	b.WriteString("\n\n")
	b.WriteString(m.styles.Error.Render(
		"If you forget this passphrase your messages become permanently unreadable."))
	return m.frame("Create Your Message Key", b.String(), "enter continue · esc back")
}

func (m Model) renderWho() string {
	var b strings.Builder
	if len(m.peers) == 0 {
		b.WriteString(m.styles.Muted.Render("Nobody else is online."))
	}
	for _, p := range m.peers {
		who := sanitizeLine(p.Nick)
		if p.Guest {
			who += " (guest)"
		}
		b.WriteString(m.styles.Body.Render(fmt.Sprintf("  Node %-3d %-20s %s",
			p.Node, truncate(who, 20), sanitizeLine(p.Where))))
		b.WriteString("\n")
	}
	return m.frame("Who's Online", b.String(), "any key back")
}

func (m Model) renderNodeInfo() string {
	id := m.cfg.Service.NodeID()
	var b strings.Builder

	b.WriteString(m.styles.Body.Render("This BBS's identity is derived from its own key."))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render("There is no registry — the ID is the key."))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Heading.Render("  base32  "))
	b.WriteString(m.styles.Accent.Render(id.String()))
	b.WriteString("\n")
	b.WriteString(m.styles.Heading.Render("  words   "))
	b.WriteString(m.styles.Accent.Render(id.Words()))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Muted.Render(
		"Read the words aloud when confirming this node over the radio;\n" +
			"type the base32 form when adding it to a config."))

	return m.frame("Node Identity", b.String(), "any key back")
}

func (m Model) renderSignup() string {
	s := m.signup
	var b strings.Builder

	switch s.step {
	case stepNick:
		b.WriteString(m.styles.Body.Render("Pick a nick. It is unique to this BBS only —"))
		b.WriteString("\n")
		b.WriteString(m.styles.Muted.Render("someone else may use the same nick on another node."))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render(s.nick.Render()))

	case stepPassword:
		if s.hasKey {
			b.WriteString(m.styles.Success.Render("Your SSH key will be enrolled automatically."))
			b.WriteString("\n")
			b.WriteString(m.styles.Muted.Render("A password is optional — press enter to skip it."))
		} else {
			b.WriteString(m.styles.Body.Render("Choose a password (at least 8 characters)."))
		}
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render(s.pass.Render()))

	case stepPasswordConfirm:
		b.WriteString(m.styles.Body.Render("Type it again."))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render(s.pass2.Render()))

	case stepPassphraseChoice:
		b.WriteString(m.styles.Body.Render(
			"Your private messages are encrypted with a key of your own."))
		b.WriteString("\n")
		b.WriteString(m.styles.Muted.Render(
			"It is protected by a passphrase. Use your password for that too?"))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render("  [Y] yes, use my password    [N] no, set a separate passphrase"))

	case stepPassphrase:
		b.WriteString(m.styles.Body.Render("Choose a passphrase for your message key."))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render(s.phrase.Render()))

	case stepPassphraseConfirm:
		b.WriteString(m.styles.Body.Render("Type it again."))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render(s.phrase2.Render()))

	case stepAcknowledge:
		// §6.7 requires this at signup, in plain language — not buried in a
		// man page. Losing the passphrase is genuinely unrecoverable.
		b.WriteString(m.styles.Error.Render("Before you finish, one thing that cannot be undone:"))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Body.Render(
			"If you forget your passphrase, your private messages become\n" +
				"permanently unreadable. Not by the sysop, not by anyone. There\n" +
				"is no reset, and inventing one would defeat the point."))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Muted.Render(
			"The sysop can reset your password, but doing so destroys your\n" +
				"existing mail — they will be warned, and so are you."))
		b.WriteString("\n\n")
		b.WriteString(m.styles.Accent.Render("  [Y] I understand, create my account    [N] cancel"))
	}

	if s.err != "" {
		b.WriteString("\n\n")
		b.WriteString(m.styles.Error.Render("! " + s.err))
	}

	return m.frame("New User Registration", b.String(), "esc to disconnect")
}

func (m Model) renderKeyUnknown() string {
	var b strings.Builder
	b.WriteString(m.styles.Error.Render("That account exists, but this key is not enrolled on it."))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Body.Render(sanitize(m.cfg.AuthNote)))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Muted.Render(
		"If this is your account, reconnect with a password:\n" +
			"    ssh -o PreferredAuthentications=password " + sanitizeLine(m.cfg.Nick) + "@this-bbs\n\n" +
			"You can enrol this key once you are logged in.\n\n" +
			"If you meant to register a NEW account, pick a different name:\n" +
			"    ssh new@this-bbs"))
	return m.frame("Key Not Recognised", b.String(), "any key to disconnect")
}

func (m Model) renderGoodbye() string {
	return m.styles.Title.Render("\nDisconnecting. Thanks for calling.\n\n")
}
