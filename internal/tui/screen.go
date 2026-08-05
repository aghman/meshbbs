package tui

// Screen is a renderer-independent description of what a session is looking at
// (webui.md §2).
//
// # Why this type exists
//
// The model used to build ANSI strings directly, which meant the SSH session
// was the only thing that could ever display it. A second front end would have
// had to either parse those strings back into meaning — hopeless — or grow its
// own navigation model, which is two UIs that drift the first time somebody
// adds a screen to one and forgets the other.
//
// So the model now emits THIS, and renderers consume it. There is one Screen()
// method and one menu graph; a screen cannot exist over SSH and be missing from
// the web, because there is nowhere for it to be missing from.
//
// # The rule that keeps it useful
//
// A Screen carries SEMANTICS. Renderers own GEOMETRY (webui.md §4). Truncation,
// column padding, viewport windowing and line wrapping all live in the
// renderer, because the correct answer differs: an 80-column grid wants a name
// cut to 16 characters, and a browser wants the whole name and a CSS ellipsis.
// The model emits the whole name and a hint about the column.
//
// The moment a Width or a Height field appears here, this has started becoming
// a terminal emulator again and the web renderer has lost the only advantage it
// had.
type Screen struct {
	// Kind names the screen, so a renderer can special-case layout without
	// inferring it from the blocks. It matches the screen constants.
	Kind string `json:"kind"`
	// Title is the frame's title bar.
	Title string `json:"title"`
	// Blocks are the body, rendered in order and separated by a blank line.
	Blocks []Block `json:"blocks"`
	// Status is the transient message line, empty when there is nothing to say.
	Status Status `json:"status"`
	// Help are the key bindings this screen offers.
	Help []KeyHint `json:"help"`
}

// Status is the transient result of the last action.
type Status struct {
	Text  string `json:"text"`
	IsErr bool   `json:"isErr"`
}

// KeyHint is one binding in the help line.
//
// It is a PAIR rather than the pre-joined string the model used to build, and
// that is most of why the web UI can be operated by touch: the terminal joins
// these into "up/down move · enter open · q back", and the browser renders the
// same list as buttons that are still labelled with their key — so someone who
// learns `p post` in a browser knows it over SSH.
//
// A hint with no Key is prose rather than a binding; the menu's "Press a
// highlighted letter" is the only one.
type KeyHint struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Level is the emphasis a run of text carries. It maps onto theme.Styles, so
// the vocabulary is the one the theme already defines rather than a second one
// invented here.
type Level int

const (
	LevelBody Level = iota
	LevelMuted
	LevelHeading
	LevelAccent
	LevelError
	LevelSuccess
)

// Span is a run of text at one emphasis.
type Span struct {
	Text  string `json:"text"`
	Level Level  `json:"level"`
}

// Line is a single visual line, which may mix emphasis — the menu's
// "You are austin" in heading followed by "on node 1" in muted is one line of
// two spans. This is inline emphasis in the sense HTML means it, not a cell
// grid: it says which words are stressed, never where they sit.
type Line []Span

// Block is one element of a screen body.
//
// The vocabulary is small and CLOSED on purpose. Eight kinds cover all fifteen
// screens, and adding a ninth should feel like a decision — a screen that needs
// its own bespoke block is usually a screen that wants rethinking.
type Block interface{ isBlock() }

func (TextBlock) isBlock()    {}
func (ChoicesBlock) isBlock() {}
func (TableBlock) isBlock()   {}
func (ArticleBlock) isBlock() {}
func (FormBlock) isBlock()    {}
func (ChatLogBlock) isBlock() {}
func (TabsBlock) isBlock()    {}
func (ConfirmBlock) isBlock() {}

// TextBlock is a paragraph: one or more lines at some emphasis.
type TextBlock struct {
	Lines []Line `json:"lines"`
	// Wrap marks prose whose line breaks are the renderer's business. When it
	// is false the breaks are meaningful and must be preserved — the `ssh -o
	// PreferredAuthentications=password …` example on the key-unknown screen is
	// the case that matters, since re-wrapping it would produce a command that
	// does not work.
	Wrap bool `json:"wrap"`
}

// Say builds a one-line TextBlock.
func Say(level Level, text string) TextBlock {
	return TextBlock{Lines: []Line{{{Text: text, Level: level}}}}
}

// Prose builds a wrapped TextBlock whose breaks belong to the renderer.
func Prose(level Level, text string) TextBlock {
	return TextBlock{Lines: []Line{{{Text: text, Level: level}}}, Wrap: true}
}

// Lines builds a TextBlock of several single-emphasis lines. An empty string
// is a blank line, which is how a multi-paragraph run of guidance keeps its
// shape without being split into separate blocks.
func Lines(level Level, texts ...string) TextBlock {
	b := TextBlock{}
	for _, t := range texts {
		if t == "" {
			b.Lines = append(b.Lines, Line{})
			continue
		}
		b.Lines = append(b.Lines, Line{{Text: t, Level: level}})
	}
	return b
}

// Choice is one selectable entry with a hotkey.
type Choice struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// ChoicesBlock is a hotkey menu. The web renders these as clickable rows that
// send the hotkey, so the model never learns it was a click.
type ChoicesBlock struct {
	Items []Choice `json:"items"`
}

// Column describes a table column to the renderer.
//
// Width is a HINT, not a truncation the model performs: the ANSI renderer cuts
// and pads to it, and the web ignores it in favour of real layout. This is the
// §4 rule in its most concrete form — the old code called truncate(name, 16)
// in the model, which meant every front end inherited an 80-column decision.
type Column struct {
	Width int `json:"width"`
}

// Row is one table row.
type Row struct {
	Cells []string `json:"cells"`
}

// TableBlock is a list of rows, optionally selectable and optionally headed.
type TableBlock struct {
	// Title names the section this table belongs to, rendered immediately
	// above it. The sysop status panel is a stack of titled sections.
	Title   string   `json:"title,omitempty"`
	Header  []string `json:"header,omitempty"`
	Columns []Column `json:"columns"`
	Rows    []Row    `json:"rows"`
	// Selected is the cursor position, or -1 when the table is not navigable.
	Selected int `json:"selected"`
	// Gap is the number of spaces between columns in a character grid. It is
	// the one piece of geometry the model still states, because it is the
	// difference between two visually distinct table styles the BBS already
	// uses rather than a consequence of terminal width.
	Gap int `json:"gap"`
	// Empty is shown instead of the rows when there are none.
	Empty string `json:"empty,omitempty"`
}

// ArticleBlock is a single authored item: a forum post or a private message.
type ArticleBlock struct {
	Heading string `json:"heading"`
	Meta    string `json:"meta"`
	Body    string `json:"body"`
}

// Field is one editable input.
type Field struct {
	// Name identifies the field to the web, which sets whole values rather
	// than streaming keystrokes (webui.md §5.1).
	Name   string `json:"name"`
	Label  string `json:"label"`
	Value  string `json:"value"`
	Masked bool   `json:"masked"`
	Active bool   `json:"active"`
	// Multiline selects a text area over a single-line input.
	Multiline bool `json:"multiline"`
	// Done marks a field already completed and shown only for reference — the
	// first passphrase while the confirmation is being typed. It carries no
	// cursor, and the web renders it read-only.
	Done bool `json:"done"`
	// Hint is guidance rendered directly beneath the field.
	Hint string `json:"hint,omitempty"`
}

// FormBlock is a set of fields, one of which has focus.
type FormBlock struct {
	Fields []Field `json:"fields"`
}

// ChatEntry is one line of node chat.
type ChatEntry struct {
	Time   string `json:"time"`
	Nick   string `json:"nick"`
	Text   string `json:"text"`
	System bool   `json:"system"`
}

// ChatLogBlock is the node chat backlog.
//
// It carries EVERY retained line. Windowing it to the terminal height is the
// ANSI renderer's job; the browser scrolls instead. The model used to slice
// this to `m.height - 10`, which is the clearest example of geometry that had
// no business being in the model — a browser has no rows.
type ChatLogBlock struct {
	Lines []ChatEntry `json:"lines"`
	Empty string      `json:"empty,omitempty"`
}

// TabsBlock is the sysop panel's tab bar.
type TabsBlock struct {
	Names    []string `json:"names"`
	Selected int      `json:"selected"`
}

// ConfirmBlock is a destructive-action prompt.
type ConfirmBlock struct {
	Question string `json:"question"`
	// Key is the keypress that confirms; anything else declines.
	Key string `json:"key"`
}
