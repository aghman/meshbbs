package tui

import (
	"encoding/json"
	"testing"
	"time"
)

// The Driver is what a browser session runs on. These tests are about the pump
// and the wire shape; the screens themselves are covered by the golden frames.

// waitFor polls until the condition holds or the test gives up.
//
// The driver runs commands in goroutines, exactly as Bubble Tea does, so
// results arrive asynchronously. Polling is the honest way to observe that;
// a fixed sleep would be either slow or flaky depending on the machine.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

func driverFor(t *testing.T, f *fixture, nick string) *Driver {
	t.Helper()
	u, err := f.store.GetUser(f.ctx, nick)
	if err != nil {
		t.Fatal(err)
	}
	cfg := f.config(IntentAuthenticated, nick)
	cfg.User = u
	d := NewDriver(cfg)
	t.Cleanup(d.Close)
	return d
}

func TestDriverRunsCommands(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	d := driverFor(t, f, "austin")

	// Init loads the areas asynchronously; the menu should show the account
	// once join() has come back.
	waitFor(t, "the menu to name the account", func() bool {
		return d.Screen().Kind == "menu"
	})

	d.Key("m")
	waitFor(t, "areas to load", func() bool {
		sc := d.Screen()
		if sc.Kind != "arealist" {
			return false
		}
		for _, b := range sc.Blocks {
			if tb, ok := b.(TableBlock); ok && len(tb.Rows) > 0 {
				return true
			}
		}
		return false
	})
}

// TestDriverNavigatesLikeATerminal — the browser sends key names and the model
// responds exactly as it would over SSH, because it is the same model.
func TestDriverNavigatesLikeATerminal(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	d := driverFor(t, f, "austin")

	waitFor(t, "the menu", func() bool { return d.Screen().Kind == "menu" })

	// Wait for the areas themselves, not just the screen: the list loads
	// asynchronously and its key handlers are no-ops until it has rows, so
	// acting on the bare screen kind races the load.
	d.Key("m")
	waitFor(t, "areas to load", func() bool { return rowCount(d.Screen()) > 1 })

	d.Key("down")
	if got := selectedRow(d.Screen()); got != 1 {
		t.Errorf("after down, selected = %d, want 1", got)
	}

	d.Key("q")
	waitFor(t, "the menu again", func() bool { return d.Screen().Kind == "menu" })
}

// TestDriverSelectJumpsStraightToARow — clicking the fourth row should not mean
// synthesising three down presses.
func TestDriverSelectJumpsStraightToARow(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	d := driverFor(t, f, "austin")

	waitFor(t, "the menu", func() bool { return d.Screen().Kind == "menu" })
	d.Key("m")
	waitFor(t, "areas", func() bool { return rowCount(d.Screen()) > 2 })

	d.Select(2)
	if got := selectedRow(d.Screen()); got != 2 {
		t.Errorf("selected = %d, want 2", got)
	}

	// Out of range clamps rather than refusing: the browser's idea of a list
	// length can lag the model's by one update.
	d.Select(99)
	if got, n := selectedRow(d.Screen()), rowCount(d.Screen()); got != n-1 {
		t.Errorf("selected = %d for a %d-row list, want the last row", got, n)
	}
	d.Select(-5)
	if got := selectedRow(d.Screen()); got != 0 {
		t.Errorf("selected = %d for a negative index, want 0", got)
	}
}

// TestDriverSetFieldRespectsLimits is the bound the terminal path gets from
// typing one character at a time. The web sets whole values, so a browser that
// pasted a megabyte would walk straight past it if the limit were only enforced
// in Update.
func TestDriverSetFieldRespectsLimits(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	d := driverFor(t, f, "austin")

	waitFor(t, "the menu", func() bool { return d.Screen().Kind == "menu" })
	d.Key("m")
	waitFor(t, "areas to load", func() bool { return rowCount(d.Screen()) > 0 })
	d.Key("enter")
	waitFor(t, "an area", func() bool { return d.Screen().Kind == "arearead" })
	d.Key("p")
	waitFor(t, "the composer", func() bool { return d.Screen().Kind == "postcompose" })

	huge := make([]byte, 10_000)
	for i := range huge {
		huge[i] = 'x'
	}
	d.SetField("subject", string(huge))

	for _, b := range d.Screen().Blocks {
		fb, ok := b.(FormBlock)
		if !ok {
			continue
		}
		for _, fld := range fb.Fields {
			if fld.Name == "subject" && len([]rune(fld.Value)) > 72 {
				t.Errorf("subject accepted %d runes, past its 72 limit", len([]rune(fld.Value)))
			}
		}
	}
}

// TestDriverCloseClearsTheSession — a closed tab must release the mail
// passphrase and leave Presence, exactly as an SSH disconnect does. Skipping
// that would leave a ghost in who's-online and a secret in memory.
func TestDriverCloseClearsTheSession(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	d := driverFor(t, f, "austin")

	waitFor(t, "the session to join", func() bool { return f.presence.didJoin() })
	d.Close()

	if !f.presence.didLeave() {
		t.Error("closing the driver did not leave Presence")
	}
	select {
	case <-d.Done():
	default:
		t.Error("Done() did not fire after Close()")
	}
}

// TestScreenJSONTagsEveryBlock pins the wire contract. Go's interface encoding
// drops the concrete type, so without an explicit tag a client has to guess a
// block's kind from which fields are present — which breaks precisely where a
// table and a chat log overlap, and breaks as a rendering bug rather than a
// decode error.
func TestScreenJSONTagsEveryBlock(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "pw")
	d := driverFor(t, f, "austin")
	waitFor(t, "the menu", func() bool { return d.Screen().Kind == "menu" })

	raw, err := json.Marshal(d.Screen())
	if err != nil {
		t.Fatal(err)
	}

	var out struct {
		Kind   string           `json:"kind"`
		Blocks []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "menu" {
		t.Errorf("screen kind = %q", out.Kind)
	}
	if len(out.Blocks) == 0 {
		t.Fatal("no blocks encoded")
	}
	for i, b := range out.Blocks {
		kind, _ := b["kind"].(string)
		if kind == "" {
			t.Errorf("block %d has no kind tag: %v", i, b)
		}
	}
}

// TestEveryBlockKindEncodes walks one of each, so a block added without a wire
// encoding fails here rather than as a blank area in somebody's browser.
func TestEveryBlockKindEncodes(t *testing.T) {
	blocks := []Block{
		TextBlock{}, ChoicesBlock{}, TableBlock{}, ArticleBlock{},
		FormBlock{}, ChatLogBlock{}, TabsBlock{}, ConfirmBlock{},
	}
	seen := map[string]bool{}
	for _, b := range blocks {
		raw, err := json.Marshal(Screen{Blocks: []Block{b}})
		if err != nil {
			t.Fatalf("%T does not encode: %v", b, err)
		}
		var out struct {
			Blocks []map[string]any `json:"blocks"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		kind, _ := out.Blocks[0]["kind"].(string)
		if kind == "" {
			t.Errorf("%T encoded without a kind", b)
		}
		if seen[kind] {
			t.Errorf("%T reuses the kind %q", b, kind)
		}
		seen[kind] = true
	}
	if len(seen) != 8 {
		t.Errorf("encoded %d distinct kinds, want 8", len(seen))
	}
}

func TestKeyNameMapping(t *testing.T) {
	for _, name := range []string{"enter", "up", "esc", "tab", "ctrl+d", "m", "?"} {
		if _, ok := keyMsg(name); !ok {
			t.Errorf("keyMsg(%q) was refused", name)
		}
	}
	// Unknown names are refused rather than guessed at: a key the UI does not
	// bind has no meaning to send, and inventing one is how a stray frame ends
	// up navigating somebody's session.
	for _, name := range []string{"", "F13", "ctrl+alt+delete", "meta+x", "\x00"} {
		if _, ok := keyMsg(name); ok {
			t.Errorf("keyMsg(%q) was accepted", name)
		}
	}
}

func selectedRow(sc Screen) int {
	for _, b := range sc.Blocks {
		if tb, ok := b.(TableBlock); ok {
			return tb.Selected
		}
	}
	return -1
}

func rowCount(sc Screen) int {
	for _, b := range sc.Blocks {
		if tb, ok := b.(TableBlock); ok {
			return len(tb.Rows)
		}
	}
	return 0
}
