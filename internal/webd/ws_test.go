package webd

import (
	"context"
	"encoding/json"
	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The bridge, end to end over a real WebSocket against a real server.

// screenMsg is the wire shape as a client sees it — decoded loosely on purpose,
// so these tests fail if the JSON contract changes rather than silently
// adapting to it.
type screenMsg struct {
	Nick   string `json:"nick"`
	Screen struct {
		Kind   string           `json:"kind"`
		Title  string           `json:"title"`
		Blocks []map[string]any `json:"blocks"`
		Help   []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"help"`
		Status struct {
			Text  string `json:"text"`
			IsErr bool   `json:"isErr"`
		} `json:"status"`
	} `json:"screen"`
}

// liveServer runs the front end on a real listener and returns a signed-in
// client. Sessions are minted directly: the WebAuthn ceremony needs an
// authenticator, and what is under test here is the bridge, not the login.
func liveServer(t *testing.T) (*fixture, *httptest.Server, string) {
	t.Helper()
	f := newFixture(t)

	ts := httptest.NewServer(f.srv.http.Handler)
	t.Cleanup(ts.Close)

	sess, err := f.srv.sessions.Create("austin")
	if err != nil {
		t.Fatal(err)
	}
	return f, ts, sess.ID
}

func dialWS(t *testing.T, ctx context.Context, ts *httptest.Server, sessionID string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	header := http.Header{}
	if sessionID != "" {
		header.Set("Cookie", sessionCookie+"="+sessionID)
	}
	// The server checks the WebSocket Origin itself, since browsers do not
	// apply the same-origin policy to this handshake.
	header.Set("Origin", testOrigin)

	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func read(t *testing.T, ctx context.Context, conn *websocket.Conn) screenMsg {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var msg screenMsg
	if err := wsjson.Read(readCtx, conn, &msg); err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

// readUntil consumes screens until one satisfies the condition. The model loads
// asynchronously, so several screens arrive for one action.
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn,
	why string, cond func(screenMsg) bool) screenMsg {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := read(t, ctx, conn)
		if cond(msg) {
			return msg
		}
	}
	t.Fatalf("timed out waiting for %s", why)
	return screenMsg{}
}

func TestWSRequiresASession(t *testing.T) {
	_, ts, _ := liveServer(t)
	ctx := context.Background()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	header := http.Header{}
	header.Set("Origin", testOrigin)

	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		t.Fatal("an unauthenticated socket was accepted")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestWSServesTheMenu is the whole point of the bridge: a signed-in browser
// receives the same menu an SSH user sees, as structured blocks.
func TestWSServesTheMenu(t *testing.T) {
	_, ts, sessionID := liveServer(t)
	ctx := context.Background()
	conn := dialWS(t, ctx, ts, sessionID)

	msg := readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})

	if msg.Nick != "austin" {
		t.Errorf("nick = %q, want austin", msg.Nick)
	}
	if msg.Screen.Title != "MeshBBS" {
		t.Errorf("title = %q", msg.Screen.Title)
	}

	// Every block is tagged, and the menu offers choices with hotkeys — which
	// is what makes the browser clickable without a second navigation model.
	var choices map[string]any
	for _, b := range msg.Screen.Blocks {
		if b["kind"] == "" || b["kind"] == nil {
			t.Errorf("untagged block: %v", b)
		}
		if b["kind"] == "choices" {
			choices = b
		}
	}
	if choices == nil {
		t.Fatal("the menu carried no choices block")
	}
	items, _ := choices["items"].([]any)
	if len(items) < 5 {
		t.Errorf("menu has %d items, want at least 5", len(items))
	}

	if len(msg.Screen.Help) == 0 {
		t.Error("the menu offers no key hints; a touch client would have no way out")
	}
}

// TestWSNavigatesByKey — clicking a menu row sends its hotkey, so the server
// never learns it was a click.
func TestWSNavigatesByKey(t *testing.T) {
	_, ts, sessionID := liveServer(t)
	ctx := context.Background()
	conn := dialWS(t, ctx, ts, sessionID)

	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})

	if err := wsjson.Write(ctx, conn, clientMsg{Key: "m"}); err != nil {
		t.Fatal(err)
	}
	msg := readUntil(t, ctx, conn, "the area list with rows", func(m screenMsg) bool {
		if m.Screen.Kind != "arealist" {
			return false
		}
		for _, b := range m.Screen.Blocks {
			if b["kind"] == "table" {
				rows, _ := b["rows"].([]any)
				return len(rows) > 0
			}
		}
		return false
	})

	if msg.Screen.Title != "Message Areas" {
		t.Errorf("title = %q", msg.Screen.Title)
	}
}

// TestWSSendsWholeValuesForFields covers the one place the browser cannot send
// keystrokes (webui.md §5.1) — autocorrect and IME composition revise runs of
// characters rather than emitting a keystroke stream.
func TestWSSendsWholeValuesForFields(t *testing.T) {
	_, ts, sessionID := liveServer(t)
	ctx := context.Background()
	conn := dialWS(t, ctx, ts, sessionID)

	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "m"})
	readUntil(t, ctx, conn, "areas", func(m screenMsg) bool {
		for _, b := range m.Screen.Blocks {
			if b["kind"] == "table" {
				rows, _ := b["rows"].([]any)
				return len(rows) > 0
			}
		}
		return false
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "enter"})
	readUntil(t, ctx, conn, "an area", func(m screenMsg) bool {
		return m.Screen.Kind == "arearead"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "p"})
	readUntil(t, ctx, conn, "the composer", func(m screenMsg) bool {
		return m.Screen.Kind == "postcompose"
	})

	_ = wsjson.Write(ctx, conn, clientMsg{Field: "subject", Value: "hello from a browser"})
	msg := readUntil(t, ctx, conn, "the subject to come back", func(m screenMsg) bool {
		return strings.Contains(blockJSON(m.Screen.Blocks), "hello from a browser")
	})
	if msg.Screen.Kind != "postcompose" {
		t.Errorf("kind = %q", msg.Screen.Kind)
	}
}

// TestWSQuitClosesCleanly — the model's own exit path runs, so the passphrase
// is cleared and Presence is left exactly as on an SSH disconnect.
func TestWSQuitClosesCleanly(t *testing.T) {
	f, ts, sessionID := liveServer(t)
	ctx := context.Background()
	conn := dialWS(t, ctx, ts, sessionID)

	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "q"})

	// The socket should close rather than hang: a browser reporting a dropped
	// connection when the user chose Quit is a worse goodbye than saying so.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var msg screenMsg
		readCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := wsjson.Read(readCtx, conn, &msg)
		cancel()
		if err != nil {
			// Closed, which is the expected ending.
			if f.srv.Sessions() < 0 {
				t.Error("session accounting went negative")
			}
			return
		}
	}
	t.Error("the socket stayed open after the user quit")
}

func blockJSON(blocks []map[string]any) string {
	b, _ := json.Marshal(blocks)
	return string(b)
}

// The file browser reaches the browser too.
//
// This is webui.md's central claim under test for a NEW screen rather than an
// existing one: the model emits one Screen, so a screen added for SSH cannot be
// quietly missing from the web. Nothing in the JS or the server knows what a
// file area is — the browser renders it because it is a table, the same way it
// renders the message areas.
func TestWSFileBrowser(t *testing.T) {
	f, ts, sessionID := liveServer(t)
	ctx := context.Background()

	if _, err := f.store.CreateFileArea(f.ctx, "utils", "Utilities", false); err != nil {
		t.Fatal(err)
	}
	var hash blobstore.Hash
	hash[0] = 0x42
	if _, err := f.store.PutFile(f.ctx, "utils", store.File{
		Name: "ARCHIVE.ZIP", Hash: hash, Size: 4096,
		Description: "A compressed archive", Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	conn := dialWS(t, ctx, ts, sessionID)
	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})

	if err := wsjson.Write(ctx, conn, clientMsg{Key: "f"}); err != nil {
		t.Fatal(err)
	}
	msg := readUntil(t, ctx, conn, "the file areas", func(m screenMsg) bool {
		return m.Screen.Kind == "fileareas"
	})
	if msg.Screen.Title != "File Areas" {
		t.Errorf("title = %q", msg.Screen.Title)
	}

	if err := wsjson.Write(ctx, conn, clientMsg{Key: "enter"}); err != nil {
		t.Fatal(err)
	}
	// Wait for the rows, not just the screen: the file load is a command, so
	// the first frame after the keypress is the right screen with nothing in
	// it yet.
	msg = readUntil(t, ctx, conn, "the file list with rows", func(m screenMsg) bool {
		if m.Screen.Kind != "filearea" {
			return false
		}
		for _, b := range m.Screen.Blocks {
			if b["kind"] == "table" {
				rows, _ := b["rows"].([]any)
				return len(rows) > 0
			}
		}
		return false
	})

	// The rows carry whole values, not values the model cut to 80 columns: the
	// browser is entitled to lay them out itself (webui.md §4).
	var found bool
	for _, b := range msg.Screen.Blocks {
		if b["kind"] != "table" {
			continue
		}
		rows, _ := b["rows"].([]any)
		for _, r := range rows {
			cells, _ := r.(map[string]any)["cells"].([]any)
			if len(cells) == 4 && cells[0] == "ARCHIVE.ZIP" && cells[2] == "A compressed archive" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the file row did not arrive intact: %+v", msg.Screen.Blocks)
	}
}
