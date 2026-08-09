package webd

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/aghman/meshbbs/internal/term"
	"github.com/aghman/meshbbs/internal/tui"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// The session bridge (webui.md §6).
//
// One WebSocket per signed-in browser, carrying whole Screen values out and
// key names in. There is no diffing and no patch protocol: screens are small,
// Bubble Tea already re-renders everything on every update, and a full snapshot
// has no reconciliation bug class to get wrong.

// clientMsg is everything a browser may send.
//
// Deliberately tiny. The browser drives the BBS by pressing keys — clicking the
// [M] row sends "m" — so the model never learns it is talking to a browser and
// there is no web-only path through Update for behaviour to diverge along.
type clientMsg struct {
	// Key is a key name: "m", "enter", "up", "ctrl+d".
	Key string `json:"key,omitempty"`
	// Field and Value set a whole input, the one place the browser cannot send
	// keystrokes (webui.md §5.1).
	Field string `json:"field,omitempty"`
	Value string `json:"value,omitempty"`
	// Select moves a list cursor to an index, so clicking the fourth row does
	// not mean sending three "down" presses.
	Select *int `json:"select,omitempty"`
}

// serverMsg is a rendered screen plus who is looking at it.
type serverMsg struct {
	Screen tui.Screen `json:"screen"`
	Nick   string     `json:"nick"`
	// Farewell is why the session is ending, on the last frame before the
	// socket closes. Empty on every other frame.
	Farewell string `json:"farewell,omitempty"`
}

const (
	// writeTimeout bounds a single frame write, so one wedged client cannot
	// hold a session goroutine forever.
	writeTimeout = 10 * time.Second
	// maxClientFrame caps an inbound frame. The largest legitimate message is a
	// post body, which the model itself limits to 4000 characters.
	maxClientFrame = 64 * 1024
)

// handleWS upgrades a signed-in request and runs a session on it.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	sess := s.currentSession(r)
	if sess == nil {
		httpError(w, http.StatusUnauthorized, "not signed in")
		return
	}

	user, err := s.store.GetUser(r.Context(), sess.Nick)
	if err != nil {
		httpError(w, http.StatusUnauthorized, "that account no longer exists")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The upgrade handshake is not covered by the Origin check in the
		// middleware — browsers do not apply the same-origin policy to
		// WebSocket, and the library's own check is what stands in for it.
		OriginPatterns: []string{originHost(s.opts.Origin)},
	})
	if err != nil {
		s.log.Warn("websocket upgrade failed", "err", err, "remote", r.RemoteAddr)
		return
	}
	conn.SetReadLimit(maxClientFrame)
	defer conn.CloseNow()

	// Detach from the request context: an HTTP request context is cancelled
	// when the handler returns, and this session outlives the handshake.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	driver := tui.NewDriver(tui.Config{
		Service:  s.svc,
		Store:    s.store,
		Presence: s.opts.Presence,
		Chat:     s.opts.Chat,
		Clock:    s.opts.Clock,
		Location: s.opts.Location,
		Themes:   s.opts.Themes,
		// The browser renders Screen values, so this only decides which glyph
		// set the model reaches for. A browser is unambiguously UTF-8, which is
		// the one thing the SSH path has to guess at (§5.4).
		Encoding:     term.EncodingUTF8,
		SessionLimit: s.opts.SessionLimit,
		Doors:        &doorLauncher{sshHost: s.opts.SSHHost, sshPort: s.opts.SSHPort},
		ThemeName:    s.opts.ThemeName,
		Width:        webWidth,
		Height:       webHeight,
		SessionID:    sess.ID,
		Remote:       r.RemoteAddr,
		WebEnabled:   true,
		WebURL:       s.opts.Origin,
		Intent:       tui.IntentAuthenticated,
		Nick:         user.Nick,
		User:         user,
		Logger:       s.log,
		Ctx:          ctx,
	})
	// Closing the driver runs the model's own exit path, which clears the mail
	// passphrase and leaves Presence — the same teardown an SSH disconnect gets.
	defer driver.Close()

	s.log.Info("web session opened", "nick", user.Nick, "remote", r.RemoteAddr)
	defer s.log.Info("web session closed", "nick", user.Nick)

	go s.readLoop(ctx, cancel, conn, driver, sess)
	s.writeLoop(ctx, conn, driver, user.Nick)
}

// webWidth and webHeight are the notional size reported to the model.
//
// A browser has no rows or columns — it scrolls and wraps — and the web
// renderer ignores geometry entirely (webui.md §4). These exist because the
// model still holds a size, and reporting a cramped terminal would make the
// browser inherit limits it does not have: a chat backlog windowed to a
// terminal's height, a body wrapped at column 78.
const (
	webWidth  = 120
	webHeight = 200
)

// readLoop applies client messages until the connection ends.
func (s *Server) readLoop(ctx context.Context, cancel context.CancelFunc,
	conn *websocket.Conn, driver *tui.Driver, sess *Session) {
	defer cancel()

	for {
		var msg clientMsg
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			// A closed tab is the normal ending, not an error worth logging at
			// anything above debug.
			s.log.Debug("web session read ended", "err", err)
			return
		}

		switch {
		case msg.Field != "":
			driver.SetField(msg.Field, msg.Value)
		case msg.Select != nil:
			driver.Select(*msg.Select)
		case msg.Key != "":
			driver.Key(msg.Key)
			// An unlocked session holds the mail passphrase in memory, which
			// tightens its idle bound. Reading it back off the model rather
			// than inferring it from the keypress keeps the two in step.
			s.sessions.SetUnlocked(sess.ID, driver.Unlocked())
		}
	}
}

// writeLoop pushes a fresh screen whenever the model changes.
func (s *Server) writeLoop(ctx context.Context, conn *websocket.Conn,
	driver *tui.Driver, nick string) {
	for {
		if err := s.push(ctx, conn, driver, nick); err != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-driver.Done():
			// The user quit. Send the final screen, then close politely so the
			// browser can say so rather than reporting a dropped connection.
			_ = s.push(ctx, conn, driver, nick)
			// The close reason as well as the frame: a browser that has already
			// torn the page down still surfaces this one.
			_ = conn.Close(websocket.StatusNormalClosure, closeReason(driver.Farewell()))
			return
		case <-driver.Dirty():
		}
	}
}

func (s *Server) push(ctx context.Context, conn *websocket.Conn,
	driver *tui.Driver, nick string) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	err := wsjson.Write(writeCtx, conn, serverMsg{
		Screen: driver.Screen(), Nick: nick, Farewell: driver.Farewell(),
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		s.log.Debug("web session write failed", "err", err)
	}
	return err
}

// originHost strips the scheme from the configured origin, which is the form
// the WebSocket origin check wants.
func originHost(origin string) string {
	for _, prefix := range []string{"https://", "http://"} {
		if len(origin) > len(prefix) && origin[:len(prefix)] == prefix {
			return origin[len(prefix):]
		}
	}
	return origin
}

// closeReason fits a farewell into a WebSocket close frame, which allows 123
// bytes and drops the whole close if given more.
func closeReason(farewell string) string {
	if farewell == "" {
		return "goodbye"
	}
	const max = 120
	if len(farewell) > max {
		return farewell[:max]
	}
	return farewell
}
