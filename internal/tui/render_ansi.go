package tui

import (
	"fmt"
	"strings"

	"github.com/aghman/meshbbs/internal/theme"
	"github.com/charmbracelet/lipgloss"
)

// ansiRenderer draws a Screen for a character terminal (webui.md §2).
//
// This is the half of the old render* methods that deals in columns and rows.
// Everything here is geometry: truncating a name to fit a column, windowing a
// chat backlog to the rows available, wrapping prose at the frame edge. None of
// it belongs in the model, because none of it is true of a browser.
type ansiRenderer struct {
	styles theme.Styles
	width  int
	height int
}

func (r ansiRenderer) style(l Level) lipgloss.Style {
	switch l {
	case LevelMuted:
		return r.styles.Muted
	case LevelHeading:
		return r.styles.Heading
	case LevelAccent:
		return r.styles.Accent
	case LevelError:
		return r.styles.Error
	case LevelSuccess:
		return r.styles.Success
	default:
		return r.styles.Body
	}
}

// render draws the whole frame: title bar, body, status line, help.
func (r ansiRenderer) render(s Screen) string {
	var b strings.Builder

	b.WriteString(r.styles.Title.Render(s.Title))
	b.WriteString("\n")
	b.WriteString(r.styles.Muted.Render(strings.Repeat("-", minInt(r.width, 78))))
	b.WriteString("\n\n")
	b.WriteString(r.body(s.Blocks))
	b.WriteString("\n")

	if s.Status.Text != "" {
		b.WriteString("\n")
		// WRAP the status, never truncate it. These messages carry the remedy
		// for whatever just failed — "ask the sysop for the post_federated
		// capability" is the whole point of the [N7] refusal — and clipping
		// them at the terminal edge throws away the actionable half.
		style := r.styles.Success
		prefix := "* "
		if s.Status.IsErr {
			style, prefix = r.styles.Error, "! "
		}
		b.WriteString(style.Width(r.width).Render(prefix + sanitize(s.Status.Text)))
		b.WriteString("\n")
	}
	if help := r.help(s.Help); help != "" {
		b.WriteString("\n")
		b.WriteString(r.styles.StatusBar.Render(help))
	}

	// Clamp to the terminal width. §5.4 sets 80x24 as the fallback floor, but
	// people connect from phones and narrow splits, and a line that overflows
	// wraps into a garbled second line rather than being merely cut off.
	// MaxWidth is ANSI-aware, so this truncates visible columns without
	// severing a colour escape.
	return lipgloss.NewStyle().MaxWidth(r.width).Render(b.String())
}

// help joins key hints the way a terminal wants them.
func (r ansiRenderer) help(hints []KeyHint) string {
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		switch {
		case h.Key == "":
			parts = append(parts, h.Label)
		case h.Label == "":
			parts = append(parts, h.Key)
		default:
			parts = append(parts, h.Key+" "+h.Label)
		}
	}
	return strings.Join(parts, " · ")
}

// body renders the blocks, separated by a blank line.
func (r ansiRenderer) body(blocks []Block) string {
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		parts = append(parts, r.block(blk))
	}
	return strings.Join(parts, "\n\n")
}

func (r ansiRenderer) block(b Block) string {
	switch v := b.(type) {
	case TextBlock:
		return r.text(v)
	case ChoicesBlock:
		return r.choices(v)
	case TableBlock:
		return r.table(v)
	case ArticleBlock:
		return r.article(v)
	case FormBlock:
		return r.form(v)
	case ChatLogBlock:
		return r.chatLog(v)
	case TabsBlock:
		return r.tabs(v)
	case ConfirmBlock:
		return r.confirm(v)
	default:
		return ""
	}
}

func (r ansiRenderer) text(t TextBlock) string {
	out := make([]string, 0, len(t.Lines))
	for _, line := range t.Lines {
		// Wrapping is only meaningful for a line of uniform emphasis; a mixed
		// line wrapped span-by-span would break at every style change rather
		// than at the frame edge. No screen needs both at once.
		if t.Wrap && len(line) == 1 {
			out = append(out, r.style(line[0].Level).Width(r.width).Render(line[0].Text))
			continue
		}
		var sb strings.Builder
		for _, sp := range line {
			sb.WriteString(r.style(sp.Level).Render(sp.Text))
		}
		out = append(out, sb.String())
	}
	return strings.Join(out, "\n")
}

func (r ansiRenderer) choices(c ChoicesBlock) string {
	out := make([]string, 0, len(c.Items))
	for _, it := range c.Items {
		out = append(out, "  "+r.styles.Accent.Render("["+it.Key+"]")+" "+r.styles.Body.Render(it.Label))
	}
	return strings.Join(out, "\n")
}

// table lays rows out as fixed-width columns.
//
// The model hands over whole strings and a width hint; cutting them to fit is
// this function's job. That split is what lets the browser show a name the
// terminal has to elide.
func (r ansiRenderer) table(t TableBlock) string {
	title := ""
	if t.Title != "" {
		title = r.styles.Heading.Render(t.Title) + "\n"
	}
	if len(t.Rows) == 0 && t.Empty != "" {
		return title + r.styles.Muted.Render(t.Empty)
	}

	gap := t.Gap
	if gap <= 0 {
		gap = 1
	}
	sep := strings.Repeat(" ", gap)

	cells := func(vals []string) string {
		parts := make([]string, 0, len(vals))
		for i, v := range vals {
			w := 0
			if i < len(t.Columns) {
				w = t.Columns[i].Width
			}
			if w > 0 {
				parts = append(parts, fmt.Sprintf("%-*s", w, truncate(v, w)))
				continue
			}
			parts = append(parts, v)
		}
		return strings.Join(parts, sep)
	}

	var out []string
	if len(t.Header) > 0 {
		out = append(out, r.styles.Muted.Render("  "+cells(t.Header)))
	}
	for i, row := range t.Rows {
		line := cells(row.Cells)
		if i == t.Selected {
			out = append(out, r.styles.Selected.Render("> "+line))
			continue
		}
		out = append(out, r.styles.Body.Render("  "+line))
	}
	return title + strings.Join(out, "\n")
}

func (r ansiRenderer) article(a ArticleBlock) string {
	var b strings.Builder
	b.WriteString(r.styles.Heading.Render(a.Heading))
	b.WriteString("\n")
	b.WriteString(r.styles.Muted.Render(a.Meta))
	b.WriteString("\n\n")
	b.WriteString(r.styles.Body.Render(a.Body))
	return b.String()
}

func (r ansiRenderer) form(f FormBlock) string {
	var b strings.Builder
	for i, fld := range f.Fields {
		if i > 0 {
			// A completed field sits DIRECTLY above the one being typed — the
			// passphrase above its confirmation reads as one thing. Independent
			// fields get a blank line between them.
			if f.Fields[i-1].Done {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		}

		shown := fld.Value
		if fld.Masked {
			shown = strings.Repeat("*", len([]rune(fld.Value)))
		}
		if fld.Done {
			b.WriteString(r.styles.Muted.Render(fld.Label + shown))
			continue
		}

		style := r.styles.Body
		if fld.Active {
			style = r.styles.Accent
		}
		b.WriteString(style.Render(fld.Label + shown + "_"))
		if fld.Hint != "" {
			b.WriteString("\n")
			b.WriteString(r.styles.Muted.Render(fld.Hint))
		}
	}
	return b.String()
}

// chatLog windows the backlog to the rows available.
//
// The model hands over every retained line — it has no idea how tall this
// terminal is, and should not. Leaving room for the input line and the frame is
// what the -10 accounts for.
func (r ansiRenderer) chatLog(c ChatLogBlock) string {
	if len(c.Lines) == 0 {
		return r.styles.Muted.Render(c.Empty)
	}

	visible := r.height - 10
	if visible < 3 {
		visible = 3
	}
	lines := c.Lines
	if len(lines) > visible {
		lines = lines[len(lines)-visible:]
	}

	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if l.System {
			out = append(out, r.styles.Muted.Render(fmt.Sprintf("  %s  * %s", l.Time, l.Text)))
			continue
		}
		out = append(out, r.styles.Body.Render(fmt.Sprintf("  %s  %s: %s", l.Time, l.Nick, l.Text)))
	}
	return strings.Join(out, "\n")
}

func (r ansiRenderer) tabs(t TabsBlock) string {
	var b strings.Builder
	for i, name := range t.Names {
		if i == t.Selected {
			b.WriteString(r.styles.Selected.Render(" " + name + " "))
			continue
		}
		b.WriteString(r.styles.Muted.Render(" " + name + " "))
	}
	return b.String()
}

func (r ansiRenderer) confirm(c ConfirmBlock) string {
	return r.styles.Error.Width(r.width).Render(c.Question + "  [y/N]")
}
