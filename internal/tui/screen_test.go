package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The golden frames prove the ANSI output did not change. These tests prove
// the thing goldens CANNOT see: that the model stopped making decisions that
// only a terminal is entitled to make (webui.md §4).
//
// Without them the refactor rots quietly. Someone adds a screen, calls
// truncate() in the builder because that is what the neighbouring code used to
// do, and the golden still passes — because the ANSI renderer would have
// truncated to the same width anyway. The damage only shows up in the browser,
// which by then is a separate project.

// TestScreenCarriesWholeValues is the §4 rule stated as an assertion: the model
// emits the whole string and a width hint, and the RENDERER cuts it.
func TestScreenCarriesWholeValues(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	s := f.login(t, "austin").typeRunes("m")

	table, ok := findTable(s.model.Screen())
	if !ok {
		t.Fatal("area list has no table block")
	}

	// The seeded areas include a description longer than its 26-column hint.
	var long string
	for _, row := range table.Rows {
		if len(row.Cells) > 1 && len([]rune(row.Cells[1])) > 26 {
			long = row.Cells[1]
		}
	}
	if long == "" {
		t.Fatal("no area description exceeds its column hint; this test can no longer detect truncation")
	}
	if strings.Contains(long, "…") {
		t.Errorf("model truncated a value before the renderer saw it: %q", long)
	}

	// And the terminal still elides it, because that is the renderer's job.
	if !strings.Contains(s.view(), "…") {
		t.Error("ANSI render did not elide an over-long cell")
	}
}

// TestChatLogIsNotWindowedByTheModel pins the other half: the backlog the model
// hands over does not depend on how tall the terminal is.
//
// This one used to be false. renderChat sliced to m.height-10, which meant a
// browser — which has no rows at all — would have inherited a terminal's idea
// of how much scrollback exists.
func TestChatLogIsNotWindowedByTheModel(t *testing.T) {
	f := newFixture(t)
	f.user(t, "alice", "pw")
	u, err := f.store.GetUser(f.ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, "alice")
	cfg.User = u
	cfg.Chat = NewChatRoom(200)

	s := newSession(t, cfg).typeRunes("c")

	const spoken = 40
	m := s.model
	for i := range spoken {
		m.chatLines = append(m.chatLines, ChatLine{
			At: time.Unix(1_700_000_000, 0), Nick: "alice",
			Text: fmt.Sprintf("line %d", i),
		})
	}

	var log ChatLogBlock
	found := false
	for _, b := range m.Screen().Blocks {
		if v, ok := b.(ChatLogBlock); ok {
			log, found = v, true
		}
	}
	if !found {
		t.Fatal("chat screen has no chat log block")
	}
	// Every retained line, not just the ones that fit. Joining the room adds a
	// system line of its own, so compare against what the model actually holds
	// rather than against the count typed here.
	if len(log.Lines) != len(m.chatLines) {
		t.Errorf("model windowed the backlog: carried %d lines, holds %d", len(log.Lines), len(m.chatLines))
	}
	if len(log.Lines) < spoken {
		t.Fatalf("only %d lines reached the screen; the test cannot detect windowing", len(log.Lines))
	}

	// The 24-row terminal still shows only what fits.
	shown := strings.Count(stripANSI(ansiRenderer{
		styles: m.styles, width: m.frameWidth(), height: m.height,
	}.chatLog(log)), "\n") + 1
	if shown >= spoken {
		t.Errorf("ANSI render showed %d lines on a %d-row terminal; it should window", shown, m.height)
	}
}

// TestEveryScreenIsDescribable walks the screen constants and checks each one
// produces something a renderer can work with. A screen that returns an empty
// Title has no frame in the terminal and no <title> in the browser.
func TestEveryScreenIsDescribable(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	base := f.login(t, "austin").model

	// The bound is the last real screen, not screenGoodbye — the goodbye is
	// deliberately not a Screen (no frame, nothing to navigate). Adding a
	// screen after this one without moving the bound silently skips it.
	for sc := screenSignup; sc <= screenWebEnrol; sc++ {
		m := base
		m.screen = sc
		scr := m.Screen()

		if scr.Title == "" {
			t.Errorf("screen %d produced no title", sc)
		}
		if scr.Kind == "" {
			t.Errorf("screen %d produced no kind", sc)
		}
		if len(scr.Help) == 0 {
			t.Errorf("screen %d (%s) offers no key hints; a touch client would have no way out",
				sc, scr.Kind)
		}
		for _, b := range scr.Blocks {
			tb, ok := b.(TableBlock)
			if !ok {
				continue
			}
			for i, row := range tb.Rows {
				if len(row.Cells) != len(tb.Columns) {
					t.Errorf("screen %s row %d has %d cells against %d columns",
						scr.Kind, i, len(row.Cells), len(tb.Columns))
				}
			}
		}
	}
}

func findTable(s Screen) (TableBlock, bool) {
	for _, b := range s.Blocks {
		if v, ok := b.(TableBlock); ok {
			return v, true
		}
	}
	return TableBlock{}, false
}
