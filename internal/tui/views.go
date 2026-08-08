package tui

import (
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/store"
)

// This file builds Screen descriptions (webui.md §2). It used to build ANSI
// strings; everything to do with columns, wrapping and terminal height now
// lives in render_ansi.go, because none of it is true of a browser.
//
// The discipline to hold here: emit whole values and let the renderer decide
// what fits. A truncate() call in this file is a bug — it bakes an 80-column
// decision into every front end that will ever exist.

// minInt is the smaller of two ints.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// frameWidth is the usable width, with a sane floor so a client reporting a
// nonsense size does not produce a one-column screen.
func (m Model) frameWidth() int {
	if m.width < 20 {
		return 20
	}
	return m.width
}

// statusLine is the transient message, if there is one.
func (m Model) statusLine() Status {
	return Status{Text: m.status, IsErr: m.statusErr}
}

// hints is shorthand for a help line built from key/label pairs.
func hints(pairs ...string) []KeyHint {
	out := make([]KeyHint, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, KeyHint{Key: pairs[i], Label: pairs[i+1]})
	}
	return out
}

func (m Model) buildMenu() Screen {
	who := m.nick
	if m.guest {
		who += " (guest, read-only)"
	}
	ident := Line{{Text: "You are " + who, Level: LevelHeading}}
	if m.nodeNum > 0 {
		ident = append(ident, Span{Text: fmt.Sprintf("  on node %d", m.nodeNum), Level: LevelMuted})
	}

	items := []Choice{
		{"M", "Message areas — read and post"},
		{"F", "File areas — browse what is here"},
		{"E", "Electronic mail — your private messages"},
		{"C", "Chat with everyone online"},
		{"W", "Who else is online"},
		{"N", "This node's identity"},
	}
	if m.cfg.WebEnabled && !m.guest {
		items = append(items, Choice{"P", "Passkey for the web — sign in from a browser"})
	}
	if m.sysop {
		items = append(items, Choice{"S", "Sysop panel"})
	}
	items = append(items, Choice{"Q", "Quit"})

	blocks := []Block{
		TextBlock{Lines: []Line{ident}},
		ChoicesBlock{Items: items},
	}
	if m.guest {
		blocks = append(blocks, Say(LevelMuted,
			"You are browsing as a guest. Run `ssh new@this-bbs` to register."))
	}

	return Screen{
		Kind: "menu", Title: "MeshBBS", Blocks: blocks, Status: m.statusLine(),
		Help: []KeyHint{{Label: "Press a highlighted letter. Ctrl+C to disconnect."}},
	}
}

func (m Model) buildAreaList() Screen {
	rows := make([]Row, 0, len(m.areas))
	for _, a := range m.areas {
		rows = append(rows, Row{Cells: []string{a.Name, sanitizeLine(a.Description), a.Scope()}})
	}

	return Screen{
		Kind: "arealist", Title: "Message Areas", Status: m.statusLine(),
		Blocks: []Block{
			TableBlock{
				Columns:  []Column{{Width: 16}, {Width: 26}, {}},
				Gap:      2,
				Rows:     rows,
				Selected: m.areaIdx,
				Empty:    "No message areas yet.",
			},
			// [N7]'s UI burden: say what "federated" costs, where someone is
			// deciding which area to post in.
			Prose(LevelMuted, `"Local to this BBS" means posts stay here. "Federated" means they `+
				"travel the mesh and spend shared airtime, which needs the post_federated capability."),
		},
		Help: hints("up/down", "move", "enter", "open", "q", "back"),
	}
}

func (m Model) buildAreaRead() Screen {
	area, scope := "", ""
	if len(m.areas) > 0 {
		area = m.areas[m.areaIdx].Name
		scope = m.areas[m.areaIdx].Scope()
	}

	// [N7]'s UI burden: state the area's reach so a user who posts and sees
	// nothing federate is never left guessing.
	blocks := []Block{Say(LevelMuted, "Scope: "+scope)}
	if len(m.posts) == 0 {
		blocks = append(blocks, Say(LevelMuted, "No posts yet. Press P to write the first one."))
	} else {
		p := m.posts[m.postIdx]
		blocks = append(blocks, ArticleBlock{
			Heading: sanitizeLine(p.Subject),
			Meta: fmt.Sprintf("from %s · %s · message %d of %d",
				sanitizeLine(p.Author), m.at(int64(p.TS), "2006-01-02 15:04"),
				m.postIdx+1, len(m.posts)),
			Body: sanitize(p.Body),
		})
	}

	return Screen{
		Kind: "arearead", Title: "Area: " + area, Blocks: blocks, Status: m.statusLine(),
		Help: hints("up/down", "previous·next", "p", "post", "q", "back"),
	}
}

func (m Model) buildCompose() Screen {
	c := m.compose
	return Screen{
		Kind: "postcompose", Title: "New Post", Status: m.statusLine(),
		Blocks: []Block{
			Say(LevelMuted, "Posting to "+c.area),
			FormBlock{Fields: []Field{
				{Name: "subject", Label: c.subject.prompt, Value: c.subject.String(), Active: c.field == 1},
				{Name: "body", Label: "", Value: c.body.String(), Active: c.field == 2, Multiline: true},
			}},
		},
		Help: hints("tab", "next field", "ctrl+d", "post", "esc", "cancel"),
	}
}

func (m Model) buildMailCompose() Screen {
	c := m.compose
	return Screen{
		Kind: "mailcompose", Title: "New Message", Status: m.statusLine(),
		Blocks: []Block{
			FormBlock{Fields: []Field{
				{
					Name: "to", Label: c.to.prompt, Value: c.to.String(), Active: c.field == 0,
					Hint: "  a nick, or nick@node — node may be an alias your sysop set up",
				},
				{Name: "subject", Label: c.subject.prompt, Value: c.subject.String(), Active: c.field == 1},
				{Name: "body", Label: "", Value: c.body.String(), Active: c.field == 2, Multiline: true},
			}},
		},
		Help: hints("tab", "next field", "ctrl+d", "send", "esc", "cancel"),
	}
}

func (m Model) buildMailList() Screen {
	rows := make([]Row, 0, len(m.mail))
	for _, d := range m.mail {
		flag := " "
		if d.Unread() {
			flag = "*"
		}
		// The subject is encrypted with the body, so it is genuinely unknown
		// until the message is opened. Saying so is more honest than showing
		// a blank column.
		rows = append(rows, Row{Cells: []string{
			flag,
			sanitizeLine(d.Sender),
			"(encrypted — press enter to read)",
			m.at(d.SentAt, "01-02 15:04"),
		}})
	}

	return Screen{
		Kind: "maillist", Title: "Mail", Status: m.statusLine(),
		Blocks: []Block{TableBlock{
			Columns:  []Column{{Width: 1}, {Width: 14}, {Width: 34}, {}},
			Rows:     rows,
			Selected: m.mailIdx,
			Empty:    "No messages.",
		}},
		Help: hints("up/down", "move", "enter", "read", "c", "compose", "q", "back"),
	}
}

func (m Model) buildMailRead() Screen {
	if len(m.mail) == 0 {
		return Screen{Kind: "mailread", Title: "Mail", Status: m.statusLine(),
			Help: hints("q", "back")}
	}
	d := m.mail[m.mailIdx]
	return Screen{
		Kind: "mailread", Title: "Message", Status: m.statusLine(),
		Blocks: []Block{ArticleBlock{
			Heading: sanitizeLine(m.mailSubject),
			Meta:    "from " + sanitizeLine(d.Sender) + " · " + m.at(d.SentAt, "2006-01-02 15:04"),
			Body:    sanitize(m.mailBody),
		}},
		Help: hints("r", "reply", "any other key", "back"),
	}
}

func (m Model) buildUnlock() Screen {
	return Screen{
		Kind: "unlock", Title: "Unlock Mail", Status: m.statusLine(),
		Blocks: []Block{
			TextBlock{Lines: []Line{
				{{Text: "Your messages are encrypted with a key that only your passphrase opens.", Level: LevelBody}},
				{{Text: "The sysop stores that key encrypted and cannot read your mail without it.", Level: LevelMuted}},
			}},
			FormBlock{Fields: []Field{
				{Name: "passphrase", Label: m.unlockPW.prompt, Value: m.unlockPW.String(),
					Masked: true, Active: true},
			}},
		},
		Help: hints("enter", "unlock", "esc", "back"),
	}
}

func (m Model) buildKeySetup() Screen {
	fields := []Field{
		{Name: "passphrase", Label: m.setupPW.prompt, Value: m.setupPW.String(),
			Masked: true, Active: m.setupIdx == 0, Done: m.setupIdx != 0},
	}
	if m.setupIdx != 0 {
		fields = append(fields, Field{
			Name: "confirm", Label: m.setupPW2.prompt, Value: m.setupPW2.String(),
			Masked: true, Active: true,
		})
	}

	return Screen{
		Kind: "keysetup", Title: "Create Your Message Key", Status: m.statusLine(),
		Blocks: []Block{
			TextBlock{Lines: []Line{
				{{Text: "You do not have a message key yet.", Level: LevelBody}},
				{{Text: "Your private messages are encrypted with a key only your passphrase opens.", Level: LevelMuted}},
				{{Text: "The sysop stores it encrypted and cannot read your mail.", Level: LevelMuted}},
			}},
			FormBlock{Fields: fields},
			Say(LevelError, "If you forget this passphrase your messages become permanently unreadable."),
		},
		Help: hints("enter", "continue", "esc", "back"),
	}
}

func (m Model) buildWho() Screen {
	rows := make([]Row, 0, len(m.peers))
	for _, p := range m.peers {
		who := sanitizeLine(p.Nick)
		if p.Guest {
			who += " (guest)"
		}
		rows = append(rows, Row{Cells: []string{
			fmt.Sprintf("Node %d", p.Node), who, sanitizeLine(p.Where),
		}})
	}

	return Screen{
		Kind: "who", Title: "Who's Online", Status: m.statusLine(),
		Blocks: []Block{TableBlock{
			Columns:  []Column{{Width: 8}, {Width: 20}, {}},
			Rows:     rows,
			Selected: -1,
			Empty:    "Nobody else is online.",
		}},
		Help: hints("any key", "back"),
	}
}

func (m Model) buildNodeInfo() Screen {
	id := m.cfg.Service.NodeID()
	return Screen{
		Kind: "nodeinfo", Title: "Node Identity", Status: m.statusLine(),
		Blocks: []Block{
			TextBlock{Lines: []Line{
				{{Text: "This BBS's identity is derived from its own key.", Level: LevelBody}},
				{{Text: "There is no registry — the ID is the key.", Level: LevelMuted}},
			}},
			TextBlock{Lines: []Line{
				{{Text: "  base32  ", Level: LevelHeading}, {Text: id.String(), Level: LevelAccent}},
				{{Text: "  words   ", Level: LevelHeading}, {Text: id.Words(), Level: LevelAccent}},
			}},
			Prose(LevelMuted, "Read the words aloud when confirming this node over the radio; "+
				"type the base32 form when adding it to a config."),
		},
		Help: hints("any key", "back"),
	}
}

func (m Model) buildSignup() Screen {
	s := m.signup
	var blocks []Block

	switch s.step {
	case stepNick:
		blocks = append(blocks,
			TextBlock{Lines: []Line{
				{{Text: "Pick a nick. It is unique to this BBS only —", Level: LevelBody}},
				{{Text: "someone else may use the same nick on another node.", Level: LevelMuted}},
			}},
			FormBlock{Fields: []Field{
				{Name: "nick", Label: s.nick.prompt, Value: s.nick.String(), Active: true},
			}})

	case stepPassword:
		intro := TextBlock{}
		if s.hasKey {
			intro.Lines = []Line{
				{{Text: "Your SSH key will be enrolled automatically.", Level: LevelSuccess}},
				{{Text: "A password is optional — press enter to skip it.", Level: LevelMuted}},
			}
		} else {
			intro.Lines = []Line{
				{{Text: "Choose a password (at least 8 characters).", Level: LevelBody}},
			}
		}
		blocks = append(blocks, intro, FormBlock{Fields: []Field{
			{Name: "password", Label: s.pass.prompt, Value: s.pass.String(), Masked: true, Active: true},
		}})

	case stepPasswordConfirm:
		blocks = append(blocks,
			Say(LevelBody, "Type it again."),
			FormBlock{Fields: []Field{
				{Name: "password2", Label: s.pass2.prompt, Value: s.pass2.String(), Masked: true, Active: true},
			}})

	case stepPassphraseChoice:
		blocks = append(blocks,
			TextBlock{Lines: []Line{
				{{Text: "Your private messages are encrypted with a key of your own.", Level: LevelBody}},
				{{Text: "It is protected by a passphrase. Use your password for that too?", Level: LevelMuted}},
			}},
			Say(LevelAccent, "  [Y] yes, use my password    [N] no, set a separate passphrase"))

	case stepPassphrase:
		blocks = append(blocks,
			Say(LevelBody, "Choose a passphrase for your message key."),
			FormBlock{Fields: []Field{
				{Name: "passphrase", Label: s.phrase.prompt, Value: s.phrase.String(), Masked: true, Active: true},
			}})

	case stepPassphraseConfirm:
		blocks = append(blocks,
			Say(LevelBody, "Type it again."),
			FormBlock{Fields: []Field{
				{Name: "passphrase2", Label: s.phrase2.prompt, Value: s.phrase2.String(), Masked: true, Active: true},
			}})

	case stepAcknowledge:
		// §6.7 requires this at signup, in plain language — not buried in a
		// man page. Losing the passphrase is genuinely unrecoverable.
		blocks = append(blocks,
			Say(LevelError, "Before you finish, one thing that cannot be undone:"),
			Prose(LevelBody, "If you forget your passphrase, your private messages become "+
				"permanently unreadable. Not by the sysop, not by anyone. There "+
				"is no reset, and inventing one would defeat the point."),
			Prose(LevelMuted, "The sysop can reset your password, but doing so destroys your "+
				"existing mail — they will be warned, and so are you."),
			Say(LevelAccent, "  [Y] I understand, create my account    [N] cancel"))
	}

	if s.err != "" {
		blocks = append(blocks, Say(LevelError, "! "+s.err))
	}

	return Screen{
		Kind: "signup", Title: "New User Registration", Blocks: blocks, Status: m.statusLine(),
		Help: []KeyHint{{Key: "esc", Label: "to disconnect"}},
	}
}

// buildWebEnrol shows a live passkey-enrolment code ([D18]).
//
// The screen has one job beyond displaying the code: making its authority
// legible. A user who is told "here is a code, type it into a website" has been
// handed something that looks exactly like a password, and will treat it like
// one — so the screen says what it can and cannot do, in those words.
func (m Model) buildWebEnrol() Screen {
	where := m.cfg.WebURL
	if where == "" {
		where = "this BBS's web address"
	}

	return Screen{
		Kind: "webenrol", Title: "Passkey Enrolment", Status: m.statusLine(),
		Blocks: []Block{
			Lines(LevelBody,
				"Open "+where+" in a browser and enter this code to add a passkey",
				"to your account. After that the passkey signs you in on its own."),
			TextBlock{Lines: []Line{
				{{Text: "  code   ", Level: LevelHeading}, {Text: m.webCode, Level: LevelAccent}},
				{{Text: "  until  ", Level: LevelHeading},
					{Text: m.at(m.webCodeExpires, "15:04:05"), Level: LevelAccent}},
			}},
			// Saying this plainly is the point. The code looks like a password
			// and is not one, and a user who believes otherwise will guard it
			// like a password — or worse, reuse the habit somewhere it matters.
			Prose(LevelMuted, "This code can only add a passkey. It cannot log anyone in, it works "+
				"once, and asking for another cancels this one."),
			Say(LevelMuted, "It expires in ten minutes. Press P again for a fresh one."),
		},
		Help: hints("any key", "back"),
	}
}

func (m Model) buildKeyUnknown() Screen {
	return Screen{
		Kind: "keyunknown", Title: "Key Not Recognised", Status: m.statusLine(),
		Blocks: []Block{
			Say(LevelError, "That account exists, but this key is not enrolled on it."),
			Say(LevelBody, sanitize(m.cfg.AuthNote)),
			// These breaks are load-bearing: re-wrapping the ssh invocation
			// would print a command that does not work.
			Lines(LevelMuted, strings.Split(
				"If this is your account, reconnect with a password:\n"+
					"    ssh -o PreferredAuthentications=password "+sanitizeLine(m.cfg.Nick)+"@this-bbs\n\n"+
					"You can enrol this key once you are logged in.\n\n"+
					"If you meant to register a NEW account, pick a different name:\n"+
					"    ssh new@this-bbs", "\n")...),
		},
		Help: hints("any key", "to disconnect"),
	}
}

// humanSize renders a byte count for a listing.
//
// Rounded and unit-suffixed rather than exact: a file listing is read at a
// glance, and "4.2 MB" answers "will this fit on my machine" better than
// 4404019 does. The detail screen shows the exact figure, because that is where
// someone checking a transfer completed will look.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// fetchCommand is the SFTP invocation that gets a file.
//
// The BBS deliberately has no in-band transfer (§5.1): no ZMODEM, no XMODEM,
// nothing to emulate. That makes the browser's job to say where a file is and
// how to fetch it, and a browser that cannot answer the second half is only
// half a browser — so this is shown rather than left as something the user is
// expected to already know.
func (m Model) fetchCommand(area, name string) string {
	who := m.nick
	if who == "" || m.guest {
		who = "you"
	}
	port := ""
	if m.cfg.SSHPort != 0 && m.cfg.SSHPort != 22 {
		port = fmt.Sprintf("-P %d ", m.cfg.SSHPort)
	}
	return fmt.Sprintf("sftp %s%s@this-bbs  then:  get /%s/%s",
		port, sanitizeLine(who), sanitizeLine(area), sanitizeLine(name))
}

func (m Model) buildFileAreaList() Screen {
	rows := make([]Row, 0, len(m.fileAreas))
	for _, a := range m.fileAreas {
		rows = append(rows, Row{Cells: []string{a.Name, sanitizeLine(a.Description), a.Scope()}})
	}

	// A table's Empty line is rendered without wrapping, so it has to be short
	// enough to fit any terminal. The guidance goes in prose beneath, which the
	// renderer does wrap — and which changes with the situation, because
	// explaining what federation costs is noise on a BBS that has no file areas
	// to federate.
	guidance := Prose(LevelMuted, `"Federated" means this area's file list travels the mesh, `+
		"so other BBSes can see what is here. The files themselves never do, at any size.")
	if len(m.fileAreas) == 0 {
		guidance = Prose(LevelMuted,
			"There are none yet. The sysop makes one with `meshbbs area create <name> --files`, "+
				"and users with the upload_files capability fill it over SFTP.")
	}

	return Screen{
		Kind: "fileareas", Title: "File Areas", Status: m.statusLine(),
		Blocks: []Block{
			TableBlock{
				Columns:  []Column{{Width: 16}, {Width: 26}, {}},
				Gap:      2,
				Rows:     rows,
				Selected: m.fileAreaIdx,
				Empty:    "No file areas yet.",
			},
			// The same [N7] burden the message areas carry, and the same answer:
			// say what federating costs where someone can see it. The wording
			// differs because what travels differs — a federated file area puts
			// the CATALOG on the mesh and never the files (§7.5).
			guidance,
		},
		Help: hints("up/down", "move", "enter", "open", "q", "back"),
	}
}

// holderLabel names the BBS holding a file, as short as is still unambiguous.
//
// [D9] makes the sysop's petname the human-facing surface, so it wins when
// there is one; a node's own display name is the next best; and the short ID is
// the fallback that always exists. "here" for our own files, because "held by
// the BBS you are typing into" is noise in every row.
func holderLabel(e store.CatalogEntry) string {
	if e.Local {
		return "here"
	}
	if e.Holder != "" {
		return sanitizeLine(e.Holder)
	}
	if !e.Origin.IsZero() {
		return e.Origin.Short()
	}
	return "elsewhere"
}

func (m Model) buildFileArea() Screen {
	var elsewhere int
	rows := make([]Row, 0, len(m.files))
	for _, f := range m.files {
		if !f.Held {
			elsewhere++
		}
		rows = append(rows, Row{Cells: []string{
			f.Name,
			humanSize(f.Size),
			sanitizeLine(f.Description),
			holderLabel(f),
		}})
	}

	blocks := []Block{
		TableBlock{
			// "Held by" rather than "From": a FILE record carries no uploader,
			// so what the network knows about a peer's file is which BBS has it
			// (§6.5). Naming the column for the weaker fact would invite reading
			// a node name as a person.
			Header:   []string{"Name", "Size", "Description", "Held by"},
			Columns:  []Column{{Width: 24}, {Width: 9}, {Width: 24}, {Width: 14}},
			Gap:      2,
			Rows:     rows,
			Selected: m.fileIdx,
			Empty:    "This area has no files yet.",
		},
	}
	if elsewhere > 0 {
		// Said once, here, rather than repeated per row: the constraint is a
		// property of the mesh, not of any particular file (§7.5).
		blocks = append(blocks, Prose(LevelMuted, fmt.Sprintf(
			"%s held by another BBS. File listings travel the mesh; the files "+
				"themselves never do, so those are not downloadable from here.",
			plural(elsewhere, "file is", "files are"))))
	}

	// Advertise "d" only on a row the user can actually edit. A hint for a key
	// that answers "you can only describe files you uploaded" is worse than no
	// hint: it reads as a bug rather than as a rule. A peer's file is never
	// editable by anyone here, sysop included — its description lives in their
	// record, signed by their node.
	help := hints("up/down", "move", "enter", "details", "q", "back")
	if m.fileIdx >= 0 && m.fileIdx < len(m.files) &&
		m.files[m.fileIdx].MayDescribe(m.nick, m.sysop) {
		help = hints("up/down", "move", "enter", "details", "d", "describe", "q", "back")
	}

	return Screen{
		Kind: "filearea", Title: "Files in " + sanitizeLine(m.fileArea), Status: m.statusLine(),
		Blocks: blocks,
		Help:   help,
	}
}

// plural renders a count with the right verb form.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (m Model) buildFileInfo() Screen {
	if m.fileIdx < 0 || m.fileIdx >= len(m.files) {
		return m.buildFileArea()
	}
	f := m.files[m.fileIdx]

	desc := sanitize(f.Description)
	if strings.TrimSpace(desc) == "" {
		desc = "(no description)"
	}

	meta := fmt.Sprintf("%s  ·  %d bytes", humanSize(f.Size), f.Size)
	if f.Uploader != "" {
		meta += "  ·  uploaded by " + sanitizeLine(f.Uploader)
	}
	if f.TS != 0 {
		meta += "  ·  " + m.at(f.TS, "2006-01-02 15:04")
	}

	blocks := []Block{
		ArticleBlock{
			Heading: sanitizeLine(f.Name),
			Meta:    meta,
			Body:    desc,
		},
	}
	blocks = append(blocks, m.fileAvailability(f)...)

	help := hints("q", "back")
	if f.MayDescribe(m.nick, m.sysop) {
		help = hints("d", "describe", "q", "back")
	}

	return Screen{
		Kind: "fileinfo", Title: sanitizeLine(f.Name), Blocks: blocks,
		Status: m.statusLine(),
		Help:   help,
	}
}

// buildFileDescribe is the description editor (§6.5).
//
// It is a screen rather than an inline edit on the detail view because the
// description is the one field here that is not derived from the bytes: name,
// size, hash and uploader are all facts about the upload, and this is the only
// thing a person writes. Giving it its own screen is also what lets the browser
// drive it, since the web front end works in whole field values (webui.md §5.1)
// and needs somewhere to put one.
func (m Model) buildFileDescribe() Screen {
	if m.fileIdx < 0 || m.fileIdx >= len(m.files) {
		return m.buildFileArea()
	}
	f := m.files[m.fileIdx]

	return Screen{
		Kind: "filedescribe", Title: "Describe " + sanitizeLine(f.Name),
		Status: m.statusLine(),
		Blocks: []Block{
			TextBlock{Lines: []Line{
				{{Text: "Say what this file is, for people browsing the catalog.", Level: LevelBody}},
			}},
			FormBlock{Fields: []Field{
				{Name: "description", Label: m.descInput.prompt, Value: m.descInput.String(),
					Active: true,
					Hint: fmt.Sprintf("Up to %d characters. Leave it empty to clear.",
						store.MaxFileDescLen)},
			}},
			// Worth saying where it goes. A description on a federated area is
			// not a private note — it travels to every BBS that syncs the
			// catalog, and unlike the files themselves it really does go on the
			// air (§6.5, §7.5).
			Prose(LevelMuted, "If this area federates, the description travels the mesh "+
				"with the rest of the catalog entry. The file itself never does."),
		},
		Help: hints("enter", "save", "esc", "cancel"),
	}
}

// fileAvailability says whether a file can be had, and how.
//
// # Why this does not promise a queue
//
// Design §6.5 and §7.5 both sketch this line as "queued for next exchange",
// with the request satisfied at the next sneakernet bundle. That queue is a
// Phase 5 deliverable and does not exist: nothing records a request and nothing
// would satisfy one. Printing it would be exactly the dishonesty those two
// sections are about — a promise the software has no intention of keeping is
// worse than the spinner they warn against, because it does not even look like
// it is still working.
//
// So this says what is true today: which BBS holds it, and that the mesh will
// never bring the bytes. The sysop's contact is the one actionable thing we
// have, so it is offered when the holding node published one.
func (m Model) fileAvailability(f store.CatalogEntry) []Block {
	if f.Held {
		return []Block{
			Lines(LevelMuted,
				"To download it:",
				"    "+m.fetchCommand(m.fileArea, f.Name)),
		}
	}

	who := holderLabel(f)
	line := "Held by " + who
	if !f.Origin.IsZero() && who != f.Origin.Short() {
		line += " (" + f.Origin.Short() + ")"
	}

	blocks := []Block{Say(LevelAccent, line)}
	blocks = append(blocks, Prose(LevelMuted,
		"This BBS does not have the file, only its listing. File contents never "+
			"travel the mesh, at any size, so there is nothing to download here."))
	if f.HolderContact != "" {
		blocks = append(blocks, Lines(LevelMuted,
			"That BBS's sysop:",
			"    "+sanitizeLine(f.HolderContact)))
	} else {
		blocks = append(blocks, Prose(LevelMuted,
			"Ask your sysop if you need it — they can reach the other BBS directly."))
	}
	return blocks
}
