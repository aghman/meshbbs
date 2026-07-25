package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// textInput is a minimal single-line editor.
//
// bubbles/textinput would do this, but a BBS has to render sensibly on a
// CP437 terminal at 80x24 and this keeps full control of what bytes go out.
type textInput struct {
	value  string
	mask   bool // password entry
	limit  int
	prompt string
}

func newInput(prompt string, limit int, mask bool) textInput {
	return textInput{prompt: prompt, limit: limit, mask: mask}
}

// Update applies a key press, returning the new state and whether the key was
// consumed.
func (t textInput) Update(msg tea.KeyMsg) (textInput, bool) {
	switch msg.Type {
	case tea.KeyRunes:
		if t.limit > 0 && len([]rune(t.value)) >= t.limit {
			return t, true
		}
		t.value += string(msg.Runes)
		return t, true
	case tea.KeySpace:
		if t.limit > 0 && len([]rune(t.value)) >= t.limit {
			return t, true
		}
		t.value += " "
		return t, true
	case tea.KeyBackspace:
		r := []rune(t.value)
		if len(r) > 0 {
			t.value = string(r[:len(r)-1])
		}
		return t, true
	case tea.KeyCtrlU:
		t.value = ""
		return t, true
	}
	return t, false
}

// Render draws the field, masking if it is a password.
func (t textInput) Render() string {
	shown := t.value
	if t.mask {
		shown = strings.Repeat("*", len([]rune(t.value)))
	}
	return t.prompt + shown + "_"
}

func (t textInput) String() string { return t.value }
func (t *textInput) Clear()        { t.value = "" }

// textArea is a minimal multi-line editor for message bodies.
type textArea struct {
	lines []string
	limit int
}

func newTextArea(limit int) textArea {
	return textArea{lines: []string{""}, limit: limit}
}

// Update applies a key press.
func (t textArea) Update(msg tea.KeyMsg) (textArea, bool) {
	last := len(t.lines) - 1
	switch msg.Type {
	case tea.KeyRunes:
		if t.Len() >= t.limit {
			return t, true
		}
		t.lines[last] += string(msg.Runes)
		return t, true
	case tea.KeySpace:
		if t.Len() >= t.limit {
			return t, true
		}
		t.lines[last] += " "
		return t, true
	case tea.KeyEnter:
		if t.Len() >= t.limit {
			return t, true
		}
		t.lines = append(t.lines, "")
		return t, true
	case tea.KeyBackspace:
		if len(t.lines[last]) > 0 {
			r := []rune(t.lines[last])
			t.lines[last] = string(r[:len(r)-1])
		} else if len(t.lines) > 1 {
			t.lines = t.lines[:last]
		}
		return t, true
	}
	return t, false
}

// Len returns the current character count.
func (t textArea) Len() int {
	n := 0
	for _, l := range t.lines {
		n += len(l) + 1
	}
	return n
}

func (t textArea) String() string { return strings.Join(t.lines, "\n") }

// Render draws the body with a cursor on the last line.
func (t textArea) Render() string {
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	out[len(out)-1] += "_"
	return strings.Join(out, "\n")
}

func (t *textArea) Clear() { t.lines = []string{""} }
