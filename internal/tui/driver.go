package tui

import (
	"context"
	"reflect"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// Driver runs a Model without Bubble Tea (webui.md §2).
//
// # Why this is not tea.Program
//
// tea.Program owns a terminal: it renders ANSI to a writer, reads escape
// sequences from a reader, and manages an alt screen. The browser needs none of
// that — it wants Screen values and sends key names. What it DOES need is the
// rest of what tea.Program provides, which is the message pump: apply a message
// to the model, run whatever commands come back, feed their results in as more
// messages.
//
// So this is that pump and nothing else. The model is unchanged and unaware,
// which is the property the whole design rests on: an SSH session and a browser
// session run the same state machine, so they cannot drift.
//
// # Concurrency
//
// Commands run in goroutines, exactly as Bubble Tea runs them, because some of
// them block indefinitely by design — the chat watcher parks until somebody
// speaks. (The test harness in harness_test.go drives Update synchronously
// instead, which is why it needs timeouts and a cascade budget; that is a
// deliberately different trade for determinism, not duplication to merge.)
//
// A tea.Sequence is the exception, and honouring it is not optional: its whole
// contract is that each command finishes before the next begins. Callers reach
// for it precisely where a reload must not race the write it is meant to show.
type Driver struct {
	mu     sync.Mutex
	model  Model
	closed bool

	// dirty coalesces redraws. It has capacity one and is written
	// non-blockingly: a consumer that falls behind should render the LATEST
	// screen, not work through a queue of stale ones. Screens are whole
	// snapshots, so intermediate ones carry nothing.
	dirty chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewDriver builds a driver and runs the model's Init.
func NewDriver(cfg Config) *Driver {
	parent := cfg.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	cfg.Ctx = ctx

	d := &Driver{
		model:  New(cfg),
		dirty:  make(chan struct{}, 1),
		ctx:    ctx,
		cancel: cancel,
	}
	d.markDirty()
	d.dispatch(d.model.Init())
	return d
}

// Screen returns what the session is currently looking at.
func (d *Driver) Screen() Screen {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model.Screen()
}

// Dirty fires when the screen may have changed.
func (d *Driver) Dirty() <-chan struct{} { return d.dirty }

// Done fires when the session has ended, whether by the user quitting or the
// connection going away.
func (d *Driver) Done() <-chan struct{} { return d.ctx.Done() }

// Send applies a message and runs whatever it produces.
func (d *Driver) Send(msg tea.Msg) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	next, cmd := d.model.Update(msg)
	d.model = next.(Model)
	quitting := d.model.quitting
	d.mu.Unlock()

	d.markDirty()
	if quitting {
		// The model asked to leave — it has already cleared the passphrase and
		// left Presence. Stop before running whatever tea.Quit returned.
		d.Close()
		return
	}
	d.dispatch(cmd)
}

// Key sends a keypress by name, as the browser reports it.
func (d *Driver) Key(name string) {
	if msg, ok := keyMsg(name); ok {
		d.Send(msg)
	}
}

// SetField sets a whole input value.
//
// This is the ONE place the browser diverges from sending keystrokes
// (webui.md §5.1): a WebSocket frame per character is unpleasant on a phone and
// outright broken with autocorrect, predictive text and IME composition, all of
// which revise multi-character runs rather than emitting a keystroke stream.
func (d *Driver) SetField(name, value string) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.model = d.model.setField(name, value)
	d.mu.Unlock()
	d.markDirty()
}

// Select moves a list cursor straight to an index.
//
// Clicking the fourth row should not mean synthesising three "down" presses:
// that works, but it makes the wire chatty and puts the cursor somewhere
// arbitrary if the list changed underneath. The cursor is still just model
// state, so this sets it the way an arrow key would.
func (d *Driver) Select(index int) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.model = d.model.selectIndex(index)
	d.mu.Unlock()
	d.markDirty()
}

// Farewell is why the session ended, when it was not the user's own choice.
//
// The ANSI front end gets this from View(), which the web never calls: it
// renders Screen values, and a goodbye is deliberately not a Screen. Without
// this the browser would close a timed-out session with no explanation, and a
// caller who thinks the connection dropped reconnects — which on a board with
// a time limit is the behaviour the limit exists to prevent.
func (d *Driver) Farewell() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model.farewell
}

// Unlocked reports whether this session is holding a mail passphrase, which is
// what selects the shorter idle bound on the web (webui.md §9).
func (d *Driver) Unlocked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.model.unlocked
}

// Resize tells the model how much room it has.
//
// A browser has no rows or columns, so this reports a generous notional size:
// the web renderer ignores geometry entirely (webui.md §4), and the only thing
// still reading these is code that has not been taught that yet. Reporting a
// cramped terminal would make a browser inherit limits it does not have.
func (d *Driver) Resize(width, height int) {
	d.Send(tea.WindowSizeMsg{Width: width, Height: height})
}

// Close ends the session and releases the model's secrets.
func (d *Driver) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	// Tear the session down through the model's own exit path, so the
	// passphrase is cleared and Presence is left exactly as it would be on an
	// SSH disconnect. Skipping this would leak a ghost into who's-online and
	// keep a mail passphrase in memory after the tab closed.
	if !d.model.quitting {
		next, _ := d.model.leave()
		d.model = next.(Model)
	}
	d.mu.Unlock()

	d.cancel()
	d.markDirty()
}

// Wait blocks until in-flight commands have finished.
func (d *Driver) Wait() { d.wg.Wait() }

func (d *Driver) markDirty() {
	select {
	case d.dirty <- struct{}{}:
	default:
	}
}

// dispatch runs a command off the pump, feeding its result back in.
func (d *Driver) dispatch(cmd tea.Cmd) {
	if cmd == nil || !d.live() {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.exec(cmd)
	}()
}

// exec runs one command on the CALLING goroutine and feeds its result back in.
//
// Running here rather than spawning is the whole point: it is what lets a
// sequence hold the goroutine while it works through its members in order.
func (d *Driver) exec(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	if msg == nil {
		return
	}

	// tea.Batch returns an exported BatchMsg, but tea.Sequence returns an
	// UNEXPORTED sequenceMsg, and both are really just []tea.Cmd. Handling only
	// the exported one silently drops every sequenced command — and treating
	// them alike silently defeats Sequence, whose entire contract is ordering.
	if cmds, ok := asCommandSlice(msg); ok {
		if isBatch(msg) {
			d.execBatch(cmds)
			return
		}
		// A sequence: each command runs to completion, and its result is fed
		// back through Update, before the next one starts. That is what makes
		// "post, then reload the area" show the author their own post instead
		// of racing the write it depends on.
		for _, c := range cmds {
			if !d.live() {
				return
			}
			d.exec(c)
		}
		return
	}

	select {
	case <-d.ctx.Done():
	default:
		d.Send(msg)
	}
}

// execBatch runs commands concurrently and waits for them.
//
// Concurrency is Batch's contract — some of its members block indefinitely by
// design, and the chat poller must not hold up its sibling refresh. The wait
// matters only for a batch nested inside a sequence, where the step after it is
// entitled to see everything the batch did.
func (d *Driver) execBatch(cmds []tea.Cmd) {
	var wg sync.WaitGroup
	for _, c := range cmds {
		if c == nil || !d.live() {
			continue
		}
		wg.Add(1)
		d.wg.Add(1)
		go func() {
			defer wg.Done()
			defer d.wg.Done()
			d.exec(c)
		}()
	}
	wg.Wait()
}

// live reports whether the session is still running.
func (d *Driver) live() bool {
	select {
	case <-d.ctx.Done():
		return false
	default:
		return true
	}
}

// isBatch distinguishes a concurrent batch from an ordered sequence.
//
// tea.BatchMsg is exported so it can be named; sequenceMsg is not, and the only
// other []tea.Cmd message Bubble Tea produces IS that sequence. Erring towards
// "sequence" is the safe way round: running a batch in order would only cost
// concurrency, while running a sequence concurrently costs correctness.
func isBatch(msg tea.Msg) bool {
	_, ok := msg.(tea.BatchMsg)
	return ok
}

// asCommandSlice reports whether msg is a slice of tea.Cmd, whatever its
// concrete named type. Reflection covers tea.Batch and tea.Sequence without
// depending on an internal name.
func asCommandSlice(msg tea.Msg) ([]tea.Cmd, bool) {
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		return nil, false
	}
	if !v.Type().Elem().AssignableTo(reflect.TypeOf(tea.Cmd(nil))) {
		return nil, false
	}
	out := make([]tea.Cmd, 0, v.Len())
	for i := range v.Len() {
		c, ok := v.Index(i).Interface().(tea.Cmd)
		if !ok {
			return nil, false
		}
		out = append(out, c)
	}
	return out, true
}
