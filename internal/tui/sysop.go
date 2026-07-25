package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// sysopState holds the sysop panel's cursor and pending input.
type sysopState struct {
	tab      int // 0 users, 1 areas, 2 aliases, 3 status
	userIdx  int
	areaIdx  int
	aliasIdx int
	users    []store.User
	caps     map[string][]string
	areas    []store.Area
	aliases  []store.Alias
	// confirm holds a pending destructive action awaiting a y/n.
	confirm string
}

var sysopTabs = []string{"Users", "Areas", "Aliases", "Status"}

// handleSysopKey drives the sysop panel.
func (m Model) handleSysopKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.sysop_

	// A pending confirmation swallows everything until answered. Destructive
	// sysop actions are audit-logged and irreversible enough to deserve it.
	if s.confirm != "" {
		switch strings.ToLower(msg.String()) {
		case "y":
			action := s.confirm
			s.confirm = ""
			m.sysop_ = s
			return m, m.runSysopAction(action)
		default:
			s.confirm = ""
			m.sysop_ = s
			return m, okf("Cancelled.")
		}
	}

	switch strings.ToLower(msg.String()) {
	case "tab", "right":
		s.tab = (s.tab + 1) % len(sysopTabs)
		m.sysop_ = s
		return m, m.loadSysopData()
	case "left":
		s.tab = (s.tab - 1 + len(sysopTabs)) % len(sysopTabs)
		m.sysop_ = s
		return m, m.loadSysopData()
	case "up", "k":
		switch s.tab {
		case 0:
			if s.userIdx > 0 {
				s.userIdx--
			}
		case 1:
			if s.areaIdx > 0 {
				s.areaIdx--
			}
		case 2:
			if s.aliasIdx > 0 {
				s.aliasIdx--
			}
		}
		m.sysop_ = s
		return m, nil
	case "down", "j":
		switch s.tab {
		case 0:
			if s.userIdx < len(s.users)-1 {
				s.userIdx++
			}
		case 1:
			if s.areaIdx < len(s.areas)-1 {
				s.areaIdx++
			}
		case 2:
			if s.aliasIdx < len(s.aliases)-1 {
				s.aliasIdx++
			}
		}
		m.sysop_ = s
		return m, nil

	case "g":
		// Grant or revoke federated posting — the [N7] lever, and the single
		// most consequential thing a sysop does here, since it decides who may
		// spend the network's shared airtime.
		if s.tab == 0 && s.userIdx < len(s.users) {
			nick := s.users[s.userIdx].Nick
			if contains(s.caps[nick], store.CapPostFederated) {
				s.confirm = "revoke:" + nick
			} else {
				s.confirm = "grant:" + nick
			}
			m.sysop_ = s
		}
		return m, nil

	case "f":
		// Toggle an area between local and federated. Federating an area
		// starts spending mesh airtime, so it confirms.
		if s.tab == 1 && s.areaIdx < len(s.areas) {
			area := s.areas[s.areaIdx]
			if area.Federated {
				s.confirm = "unfederate:" + area.Name
			} else {
				s.confirm = "federate:" + area.Name
			}
			m.sysop_ = s
		}
		return m, nil

	case "q", "esc":
		m.screen = screenMenu
		m.setWhere("menu")
		return m, nil
	}
	return m, nil
}

// runSysopAction performs a confirmed action.
func (m Model) runSysopAction(action string) tea.Cmd {
	verb, target, _ := strings.Cut(action, ":")
	return func() tea.Msg {
		switch verb {
		case "grant":
			if err := m.cfg.Store.GrantCapability(m.ctx, target, store.CapPostFederated, m.nick); err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
			return sysopActionMsg{text: target + " can now post to federated areas."}
		case "revoke":
			if err := m.cfg.Store.RevokeCapability(m.ctx, target, store.CapPostFederated, m.nick); err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
			return sysopActionMsg{text: target + " can no longer post to federated areas."}
		case "federate":
			if err := m.cfg.Store.SetAreaFederated(m.ctx, target, true, m.nick); err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
			return sysopActionMsg{text: target + " now replicates over the mesh and spends airtime."}
		case "unfederate":
			if err := m.cfg.Store.SetAreaFederated(m.ctx, target, false, m.nick); err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
			return sysopActionMsg{text: target + " is now local to this BBS."}
		}
		return statusMsg{text: "unknown action", isErr: true}
	}
}

type sysopActionMsg struct{ text string }
type sysopDataMsg struct {
	users   []store.User
	caps    map[string][]string
	areas   []store.Area
	aliases []store.Alias
}

// loadSysopData refreshes the panel.
func (m Model) loadSysopData() tea.Cmd {
	return func() tea.Msg {
		users, err := m.cfg.Store.ListUsers(m.ctx)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		caps := map[string][]string{}
		for _, u := range users {
			c, err := m.cfg.Store.Capabilities(m.ctx, u.Nick)
			if err != nil {
				return statusMsg{text: err.Error(), isErr: true}
			}
			caps[u.Nick] = c
		}
		areas, err := m.cfg.Store.ListAreas(m.ctx)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		aliases, err := m.cfg.Store.ListAliases(m.ctx)
		if err != nil {
			return statusMsg{text: err.Error(), isErr: true}
		}
		return sysopDataMsg{users: users, caps: caps, areas: areas, aliases: aliases}
	}
}

func (m Model) renderSysop() string {
	s := m.sysop_
	var b strings.Builder

	// Tab bar.
	for i, name := range sysopTabs {
		if i == s.tab {
			b.WriteString(m.styles.Selected.Render(" " + name + " "))
		} else {
			b.WriteString(m.styles.Muted.Render(" " + name + " "))
		}
	}
	b.WriteString("\n\n")

	switch s.tab {
	case 0:
		b.WriteString(m.renderSysopUsers())
	case 1:
		b.WriteString(m.renderSysopAreas())
	case 2:
		b.WriteString(m.renderSysopAliases())
	default:
		b.WriteString(m.renderSysopStatus())
	}

	if s.confirm != "" {
		verb, target, _ := strings.Cut(s.confirm, ":")
		b.WriteString("\n\n")
		var q string
		switch verb {
		case "grant":
			q = fmt.Sprintf("Let %s post to federated areas? This spends the mesh network's shared airtime.", target)
		case "revoke":
			q = fmt.Sprintf("Stop %s posting to federated areas?", target)
		case "federate":
			q = fmt.Sprintf("Federate %s? Posts there will replicate over the mesh and consume airtime.", target)
		case "unfederate":
			q = fmt.Sprintf("Make %s local only? Existing posts stay where they already reached.", target)
		}
		b.WriteString(m.styles.Error.Width(m.frameWidth()).Render(q + "  [y/N]"))
	}

	help := "tab switch · up/down move · q back"
	switch s.tab {
	case 0:
		help = "tab switch · up/down move · g toggle federated posting · q back"
	case 1:
		help = "tab switch · up/down move · f toggle federation · q back"
	}
	return m.frame("Sysop", b.String(), help)
}

func (m Model) renderSysopUsers() string {
	s := m.sysop_
	var b strings.Builder
	if len(s.users) == 0 {
		return m.styles.Muted.Render("No accounts.")
	}
	b.WriteString(m.styles.Muted.Render(
		fmt.Sprintf("  %-14s %-8s %-6s %-9s %s", "NICK", "STATE", "SYSOP", "FEDERATED", "MAIL KEY")))
	b.WriteString("\n")
	for i, u := range s.users {
		line := fmt.Sprintf("%-14s %-8s %-6s %-9s %s",
			truncate(u.Nick, 14), u.State, yesNo(u.IsSysop),
			yesNo(contains(s.caps[u.Nick], store.CapPostFederated)),
			yesNo(u.DirectoryListed))
		if i == s.userIdx {
			b.WriteString(m.styles.Selected.Render("> " + line))
		} else {
			b.WriteString(m.styles.Body.Render("  " + line))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Width(m.frameWidth()).Render(
		"Federated posting is withheld from new accounts on purpose: anyone may " +
			"register and use this BBS, but spending the network's shared airtime is " +
			"a grant you make deliberately."))
	return b.String()
}

func (m Model) renderSysopAreas() string {
	s := m.sysop_
	var b strings.Builder
	if len(s.areas) == 0 {
		return m.styles.Muted.Render("No areas.")
	}
	for i, a := range s.areas {
		line := fmt.Sprintf("%-16s %-20s %s",
			truncate(a.Name, 16), truncate(sanitizeLine(a.Description), 20), a.Scope())
		if i == s.areaIdx {
			b.WriteString(m.styles.Selected.Render("> " + line))
		} else {
			b.WriteString(m.styles.Body.Render("  " + line))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderSysopAliases() string {
	s := m.sysop_
	var b strings.Builder
	b.WriteString(m.styles.Muted.Width(m.frameWidth()).Render(
		"Aliases are this BBS's private names for other nodes. They are resolved " +
			"when a message is composed and never travel on the wire, so another " +
			"BBS may use the same name for something else."))
	b.WriteString("\n\n")
	if len(s.aliases) == 0 {
		b.WriteString(m.styles.Muted.Render("No aliases. Add one with: meshbbs peer alias <name> <node-id>"))
		return b.String()
	}
	for i, a := range s.aliases {
		line := fmt.Sprintf("%-16s %s", truncate(a.Alias, 16), a.NodeID.String())
		if i == s.aliasIdx {
			b.WriteString(m.styles.Selected.Render("> " + line))
		} else {
			b.WriteString(m.styles.Body.Render("  " + line))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m Model) renderSysopStatus() string {
	var b strings.Builder
	id := m.cfg.Service.NodeID()

	b.WriteString(m.styles.Heading.Render("Node"))
	b.WriteString("\n")
	b.WriteString(m.styles.Body.Render("  " + id.String()))
	b.WriteString("\n")
	b.WriteString(m.styles.Muted.Render("  " + id.Words()))
	b.WriteString("\n\n")

	b.WriteString(m.styles.Heading.Render("Sessions"))
	b.WriteString("\n")
	if len(m.peers) == 0 {
		b.WriteString(m.styles.Muted.Render("  nobody connected"))
		b.WriteString("\n")
	}
	for _, p := range m.peers {
		who := sanitizeLine(p.Nick)
		if p.Guest {
			who += " (guest)"
		}
		b.WriteString(m.styles.Body.Render(fmt.Sprintf("  node %-3d %-18s %s",
			p.Node, truncate(who, 18), sanitizeLine(p.Where))))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(m.styles.Heading.Render("Counts"))
	b.WriteString("\n")
	b.WriteString(m.styles.Body.Render(fmt.Sprintf("  %d accounts · %d areas · %d aliases",
		len(m.sysop_.users), len(m.sysop_.areas), len(m.sysop_.aliases))))
	b.WriteString("\n\n")

	// Federation is Phase 2; saying so beats a blank panel or a fake gauge.
	b.WriteString(m.styles.Muted.Width(m.frameWidth()).Render(
		"Mesh federation is not built yet (Phase 3). When it is, this panel gains " +
			"the airtime budget, the observed flood multiplier, and peer high-water marks."))
	b.WriteString("\n\n")
	b.WriteString(m.styles.Muted.Render("  local time " + m.clockNow().Format(time.RFC1123)))
	return b.String()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
