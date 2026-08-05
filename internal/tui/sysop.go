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

func (m Model) buildSysop() Screen {
	s := m.sysop_

	blocks := []Block{TabsBlock{Names: sysopTabs, Selected: s.tab}}
	switch s.tab {
	case 0:
		blocks = append(blocks, m.sysopUsersBlocks()...)
	case 1:
		blocks = append(blocks, m.sysopAreasBlocks()...)
	case 2:
		blocks = append(blocks, m.sysopAliasesBlocks()...)
	default:
		blocks = append(blocks, m.sysopStatusBlocks()...)
	}

	if s.confirm != "" {
		verb, target, _ := strings.Cut(s.confirm, ":")
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
		blocks = append(blocks, ConfirmBlock{Question: q, Key: "y"})
	}

	help := hints("tab", "switch", "up/down", "move", "q", "back")
	switch s.tab {
	case 0:
		help = hints("tab", "switch", "up/down", "move", "g", "toggle federated posting", "q", "back")
	case 1:
		help = hints("tab", "switch", "up/down", "move", "f", "toggle federation", "q", "back")
	}

	return Screen{Kind: "sysop", Title: "Sysop", Blocks: blocks, Status: m.statusLine(), Help: help}
}

func (m Model) sysopUsersBlocks() []Block {
	s := m.sysop_
	if len(s.users) == 0 {
		return []Block{TableBlock{Selected: -1, Empty: "No accounts."}}
	}

	rows := make([]Row, 0, len(s.users))
	for _, u := range s.users {
		rows = append(rows, Row{Cells: []string{
			u.Nick, u.State, yesNo(u.IsSysop),
			yesNo(contains(s.caps[u.Nick], store.CapPostFederated)),
			yesNo(u.DirectoryListed),
		}})
	}

	return []Block{
		TableBlock{
			Header:   []string{"NICK", "STATE", "SYSOP", "FEDERATED", "MAIL KEY"},
			Columns:  []Column{{Width: 14}, {Width: 8}, {Width: 6}, {Width: 9}, {}},
			Rows:     rows,
			Selected: s.userIdx,
		},
		Prose(LevelMuted, "Federated posting is withheld from new accounts on purpose: anyone may "+
			"register and use this BBS, but spending the network's shared airtime is "+
			"a grant you make deliberately."),
	}
}

func (m Model) sysopAreasBlocks() []Block {
	s := m.sysop_
	rows := make([]Row, 0, len(s.areas))
	for _, a := range s.areas {
		rows = append(rows, Row{Cells: []string{a.Name, sanitizeLine(a.Description), a.Scope()}})
	}
	return []Block{TableBlock{
		Columns:  []Column{{Width: 16}, {Width: 20}, {}},
		Rows:     rows,
		Selected: s.areaIdx,
		Empty:    "No areas.",
	}}
}

func (m Model) sysopAliasesBlocks() []Block {
	s := m.sysop_
	rows := make([]Row, 0, len(s.aliases))
	for _, a := range s.aliases {
		rows = append(rows, Row{Cells: []string{a.Alias, a.NodeID.String()}})
	}
	return []Block{
		Prose(LevelMuted, "Aliases are this BBS's private names for other nodes. They are resolved "+
			"when a message is composed and never travel on the wire, so another "+
			"BBS may use the same name for something else."),
		TableBlock{
			Columns:  []Column{{Width: 16}, {}},
			Rows:     rows,
			Selected: s.aliasIdx,
			Empty:    "No aliases. Add one with: meshbbs peer alias <name> <node-id>",
		},
	}
}

func (m Model) sysopStatusBlocks() []Block {
	id := m.cfg.Service.NodeID()

	peers := make([]Row, 0, len(m.peers))
	for _, p := range m.peers {
		who := sanitizeLine(p.Nick)
		if p.Guest {
			who += " (guest)"
		}
		peers = append(peers, Row{Cells: []string{
			fmt.Sprintf("node %d", p.Node), who, sanitizeLine(p.Where),
		}})
	}

	return []Block{
		TextBlock{Lines: []Line{
			{{Text: "Node", Level: LevelHeading}},
			{{Text: "  " + id.String(), Level: LevelBody}},
			{{Text: "  " + id.Words(), Level: LevelMuted}},
		}},
		TableBlock{
			Title:    "Sessions",
			Columns:  []Column{{Width: 8}, {Width: 18}, {}},
			Rows:     peers,
			Selected: -1,
			Empty:    "  nobody connected",
		},
		TextBlock{Lines: []Line{
			{{Text: "Counts", Level: LevelHeading}},
			{{Text: fmt.Sprintf("  %d accounts · %d areas · %d aliases",
				len(m.sysop_.users), len(m.sysop_.areas), len(m.sysop_.aliases)), Level: LevelBody}},
		}},
		// Federation is Phase 2; saying so beats a blank panel or a fake gauge.
		Prose(LevelMuted, "Mesh federation is not built yet (Phase 3). When it is, this panel gains "+
			"the airtime budget, the observed flood multiplier, and peer high-water marks."),
		Say(LevelMuted, "  local time "+m.clockNow().In(m.location()).Format(time.RFC1123)),
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
