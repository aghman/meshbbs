package webd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
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
	msg, err := tryRead(ctx, conn, 5*time.Second)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

func tryRead(ctx context.Context, conn *websocket.Conn, within time.Duration) (screenMsg, error) {
	readCtx, cancel := context.WithTimeout(ctx, within)
	defer cancel()

	var msg screenMsg
	err := wsjson.Read(readCtx, conn, &msg)
	return msg, err
}

// readUntil consumes screens until one satisfies the condition. The model loads
// asynchronously, so several screens arrive for one action.
//
// The per-read timeout is whatever is left of the overall one, so a wait that
// ends in silence is reported as the condition it was waiting for rather than
// as a bare read timeout — which is the whole difference between "the post
// never came back" and "something went wrong on the socket".
func readUntil(t *testing.T, ctx context.Context, conn *websocket.Conn,
	why string, cond func(screenMsg) bool) screenMsg {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		msg, err := tryRead(ctx, conn, remaining)
		if err != nil {
			break
		}
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

// TestWSPostAppearsAfterSending — an author must see their own post.
//
// Sending returns a tea.Sequence: write the post, THEN reload the area. The
// assertion here is on the RELOADED SCREEN rather than on the send being
// accepted, because that is the only thing that distinguishes ordered from
// concurrent. Run the two commands concurrently and the reload can read the
// area before the write has committed, so the frame that lands on the author
// says "No posts yet" over the post they just made — and no later frame ever
// corrects it, since nothing reloads again.
func TestWSPostAppearsAfterSending(t *testing.T) {
	_, ts, sessionID := liveServer(t)
	ctx := context.Background()
	conn := dialWS(t, ctx, ts, sessionID)

	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "m"})
	readUntil(t, ctx, conn, "the area list with rows", func(m screenMsg) bool {
		return m.Screen.Kind == "arealist" && tableHasRows(m)
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "enter"})
	readUntil(t, ctx, conn, "an area", func(m screenMsg) bool {
		return m.Screen.Kind == "arearead"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "p"})
	readUntil(t, ctx, conn, "the composer", func(m screenMsg) bool {
		return m.Screen.Kind == "postcompose"
	})

	// Frames are applied in the order they arrive, so the two field writes are
	// in place by the time the send is handled.
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "subject", Value: "Solar setup"})
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "body", Value: "40W into a 12V AGM."})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "ctrl+d"})

	readUntil(t, ctx, conn, "the new post on the reloaded area", func(m screenMsg) bool {
		return m.Screen.Kind == "arearead" &&
			strings.Contains(blockJSON(m.Screen.Blocks), "Solar setup")
	})
}

// TestWSMailAppearsAfterSending is the same ordering claim on the mail path:
// send, THEN reload the inbox. A concurrent reload reads an inbox that does not
// yet hold the message and the mailbox comes back saying "No messages."
func TestWSMailAppearsAfterSending(t *testing.T) {
	f, ts, sessionID := liveServer(t)
	ctx := context.Background()

	// Mail needs a DM key: it is what the passphrase unlocks and what the
	// message is sealed to. austin mails austin, so one key covers both ends.
	const passphrase = "correct horse battery"
	if err := f.srv.svc.EnsureDMKey(f.ctx, "austin", passphrase); err != nil {
		t.Fatal(err)
	}

	conn := dialWS(t, ctx, ts, sessionID)
	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "e"})
	readUntil(t, ctx, conn, "the unlock prompt", func(m screenMsg) bool {
		return m.Screen.Kind == "unlock"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "passphrase", Value: passphrase})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "enter"})
	readUntil(t, ctx, conn, "the mailbox", func(m screenMsg) bool {
		return m.Screen.Kind == "maillist"
	})

	_ = wsjson.Write(ctx, conn, clientMsg{Key: "c"})
	readUntil(t, ctx, conn, "the mail composer", func(m screenMsg) bool {
		return m.Screen.Kind == "mailcompose"
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "to", Value: "austin"})
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "subject", Value: "Antenna"})
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "body", Value: "J-pole time."})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "ctrl+d"})

	// The subject is sealed with the body, so the row cannot be matched on its
	// text — the inbox having a row at all is what says the send landed before
	// the reload read it.
	readUntil(t, ctx, conn, "the sent message in the reloaded mailbox", func(m screenMsg) bool {
		return m.Screen.Kind == "maillist" && tableHasRows(m)
	})
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
	// Wait for the ROWS, not just the screen. The area load is a command, so
	// the first frame after the keypress is the right screen with an empty
	// table — and pressing enter on an empty list is a no-op, which would leave
	// the read below waiting for a frame that is never sent.
	msg := readUntil(t, ctx, conn, "the file areas with rows", func(m screenMsg) bool {
		if m.Screen.Kind != "fileareas" {
			return false
		}
		return tableHasRows(m)
	})
	if msg.Screen.Title != "File Areas" {
		t.Errorf("title = %q", msg.Screen.Title)
	}

	if err := wsjson.Write(ctx, conn, clientMsg{Key: "enter"}); err != nil {
		t.Fatal(err)
	}
	msg = readUntil(t, ctx, conn, "the file list with rows", func(m screenMsg) bool {
		return m.Screen.Kind == "filearea" && tableHasRows(m)
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

// Describing a file works from the browser too.
//
// The permission check for "d" lives in the model rather than in the view that
// draws the hint, and this is what holds that line: the browser sends the same
// key names an SSH user types (webui.md §5), so a check that only hid a hint
// would be no check at all. Here it is the allowed case — austin describing
// austin's upload — proving the browser reaches the same screen and that its
// whole-value field edit lands.
func TestWSDescribeFile(t *testing.T) {
	f, ts, sessionID := liveServer(t)
	ctx := context.Background()

	if _, err := f.store.CreateFileArea(f.ctx, "utils", "Utilities", false); err != nil {
		t.Fatal(err)
	}
	var hash blobstore.Hash
	hash[0] = 0x42
	if _, err := f.store.PutFile(f.ctx, "utils", store.File{
		Name: "ARCHIVE.ZIP", Hash: hash, Size: 4096, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	conn := dialWS(t, ctx, ts, sessionID)
	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})

	_ = wsjson.Write(ctx, conn, clientMsg{Key: "f"})
	readUntil(t, ctx, conn, "the file areas with rows", func(m screenMsg) bool {
		return m.Screen.Kind == "fileareas" && tableHasRows(m)
	})
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "enter"})
	readUntil(t, ctx, conn, "the file list with rows", func(m screenMsg) bool {
		return m.Screen.Kind == "filearea" && tableHasRows(m)
	})

	_ = wsjson.Write(ctx, conn, clientMsg{Key: "d"})
	readUntil(t, ctx, conn, "the describe screen", func(m screenMsg) bool {
		return m.Screen.Kind == "filedescribe"
	})

	// The browser sets whole field values rather than streaming keystrokes
	// (webui.md §5.1), so this is the path a real client takes.
	_ = wsjson.Write(ctx, conn, clientMsg{Field: "description", Value: "Set from a browser"})
	readUntil(t, ctx, conn, "the field to echo back", func(m screenMsg) bool {
		return strings.Contains(blockJSON(m.Screen.Blocks), "Set from a browser")
	})

	// The saved description has to reach the screen the user lands on. It did
	// not, at first: the write and the reload were a tea.Sequence, and this
	// Driver dispatches sequenced commands concurrently, so the reload read the
	// catalog before the write committed and the user's own edit came back
	// missing. This assertion is what caught that.
	_ = wsjson.Write(ctx, conn, clientMsg{Key: "enter"})
	readUntil(t, ctx, conn, "the saved description on screen", func(m screenMsg) bool {
		return m.Screen.Kind != "filedescribe" &&
			strings.Contains(blockJSON(m.Screen.Blocks), "Set from a browser")
	})

	got, err := f.store.GetFile(f.ctx, "utils", "ARCHIVE.ZIP")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "Set from a browser" {
		t.Errorf("stored description = %q, want the text the browser sent", got.Description)
	}
}

// tableHasRows reports whether a screen's table block has landed its contents.
//
// Every screen backed by a store query arrives twice: once when the keypress
// changes the screen, and again when the load command returns. A test that
// acts on the first frame is racing the second, and loses on a slow enough
// machine — which is what the race detector effectively makes every machine.
func tableHasRows(m screenMsg) bool {
	for _, b := range m.Screen.Blocks {
		if b["kind"] == "table" {
			rows, _ := b["rows"].([]any)
			return len(rows) > 0
		}
	}
	return false
}

// A peer's file reaches the browser with its holder intact.
//
// The browser is where "held by" matters most: an SSH user reading a listing
// can be told once in prose, but a web client renders the table itself and has
// to receive the holder as DATA rather than as a sentence. The model emits one
// Screen, so this is the same row the terminal draws.
func TestWSCatalogCarriesTheHolder(t *testing.T) {
	f, ts, sessionID := liveServer(t)
	ctx := context.Background()

	area, err := f.store.CreateFileArea(f.ctx, "meshwide", "Shared", true)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := identity.GenerateNodeKey(rng.TestSecret(9))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutNode(f.ctx, store.Node{
		ID: peer.ID(), PublicKey: peer.Public, DisplayName: "Pacific NW",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetAlias(f.ctx, "pnw", peer.ID()); err != nil {
		t.Fatal(err)
	}

	var full blobstore.Hash
	full[0] = 0x77
	wire, err := record.TruncateFileHash(full[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_000, area.Tag, record.FileBody{
		Name: "REMOTE.ZIP", Size: 900_000, Hash: wire, Description: "Over there",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutRecord(f.ctx, rec); err != nil {
		t.Fatal(err)
	}

	conn := dialWS(t, ctx, ts, sessionID)
	readUntil(t, ctx, conn, "the menu", func(m screenMsg) bool {
		return m.Screen.Kind == "menu"
	})
	if err := wsjson.Write(ctx, conn, clientMsg{Key: "f"}); err != nil {
		t.Fatal(err)
	}
	readUntil(t, ctx, conn, "the file areas with rows", func(m screenMsg) bool {
		return m.Screen.Kind == "fileareas" && tableHasRows(m)
	})
	if err := wsjson.Write(ctx, conn, clientMsg{Key: "enter"}); err != nil {
		t.Fatal(err)
	}
	msg := readUntil(t, ctx, conn, "the file list with rows", func(m screenMsg) bool {
		return m.Screen.Kind == "filearea" && tableHasRows(m)
	})

	var found bool
	for _, b := range msg.Screen.Blocks {
		if b["kind"] != "table" {
			continue
		}
		rows, _ := b["rows"].([]any)
		for _, r := range rows {
			cells, _ := r.(map[string]any)["cells"].([]any)
			// The holder is the last cell, and it is the sysop's petname —
			// [D9]'s human-facing surface, not a raw node ID.
			if len(cells) == 4 && cells[0] == "REMOTE.ZIP" && cells[3] == "pnw" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("the peer's row did not arrive with its holder: %+v", msg.Screen.Blocks)
	}
}
