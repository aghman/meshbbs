package sshd

import (
	"errors"
	"io"
	"sync"
)

// connMux lends a connection's byte streams to one consumer at a time.
//
// A door game is a subprocess that owns the terminal for as long as it runs
// (§9.1), which means something has to take the connection away from the TUI
// and give it back afterwards. This is that something. It exists before any
// door does because getting the handoff wrong is invisible in ordinary use and
// corrupts input under load, so it is worth proving on its own.
//
// # Why this is not tea.Exec
//
// Bubble Tea already offers a way to hand the terminal to a subprocess:
// tea.Exec, which calls ReleaseTerminal, runs the child, then RestoreTerminal.
// Over SSH it does not work, and the reason is specific enough to be worth
// recording so that nobody deletes this file and reaches for the built-in.
//
// ReleaseTerminal cancels the program's input reader. That reader comes from
// cancelreader.NewReader, which can only genuinely interrupt a blocked Read on
// something that holds a file descriptor. A charmbracelet/ssh Session has no
// Fd, so it gets cancelreader's FALLBACK implementation, whose Cancel() returns
// false and cannot interrupt a read already in progress. ReleaseTerminal
// therefore falls through its 500 ms wait with the read goroutine still parked
// inside sess.Read — and that goroutine goes on to consume and discard the next
// keystroke the user types. Worse, RestoreTerminal starts a SECOND read loop
// while the first is still parked, so from then on two goroutines split the
// user's keystrokes between them at random.
//
// The fix is to never cancel a read. One goroutine owns the connection's Read
// for the whole session and hands each chunk to whichever consumer is active,
// so a handoff is a pointer swap rather than an interruption: nothing blocks,
// nothing is cancelled, and no byte is dropped or delivered twice.
//
// # Who gets a byte that arrives during a handoff
//
// Whoever reads next. Bytes are queued by the pump and delivered on demand
// rather than routed at arrival, which is what a real terminal does — type
// ahead while a program is starting and the program gets what you typed. The
// alternative, deciding a byte's owner when it arrives, would make the answer
// depend on network timing.
//
// # Suppressed writes
//
// While a borrower holds the connection, the TUI's writes are DISCARDED and
// reported as successful. Both halves of that are deliberate.
//
// Discarded, because a suspended program keeps rendering: its asynchronous
// commands — the chat watcher above all — go on firing, and those frames must
// not land in the middle of a door's screen. Buffering them instead would only
// replay a queue of stale snapshots at resume, and a Screen is a whole snapshot
// with nothing to accumulate; the driver's dirty channel makes the same trade
// for the same reason.
//
// Reported as successful, because a write error is how Bubble Tea learns the
// terminal has gone away. Returning one here would tear down the session the
// user is going to come back to.
//
// The consequence is that the suspended renderer ends the borrow believing the
// screen already shows its last frame, when a door has since drawn over it. It
// has to be told to repaint, which is what onResume is for.
type connMux struct {
	src io.Reader
	dst io.Writer

	// onResume runs after a borrow ends, with the TUI active again. It is where
	// the suspended renderer is told to repaint; see the note on suppressed
	// writes above. Set it before the first borrow, or leave it nil.
	onResume func()

	// wmu serializes writes to dst, and is held ACROSS the ownership check as
	// well as the write itself. That is the whole of what makes a screen safe,
	// and it is worth spelling out because it is also why a handoff does not
	// need to take this lock:
	//
	//   - a write that has passed its check already holds wmu, so it completes
	//     as a whole frame and lands before anything the new owner writes, which
	//     must queue behind it;
	//   - a write that has not yet taken wmu will see the new owner when it does
	//     and be suppressed.
	//
	// Either way no frame is spliced and no stale frame lands after the new
	// owner has started drawing. Taking wmu in activate and restore as well
	// looks prudent and changes nothing observable, which is a good reason not
	// to: a lock whose removal no test can detect reads as a hazard that does
	// not exist, and the next person will preserve it without knowing why.
	wmu sync.Mutex

	mu     sync.Mutex
	ready  *sync.Cond
	active *port
	tui    *port
	// pend is the one chunk the pump has read and nobody has consumed yet.
	// Holding exactly one is deliberate backpressure: a door that stops reading
	// should stop the flow, not let us accumulate the user's keystrokes.
	pend   []byte
	err    error
	closed bool
}

// errMuxBusy reports a second borrow while one is in progress. It is an error
// rather than a queue because the only thing that can ask twice is a bug or a
// double keypress, and neither wants to be served eventually.
var errMuxBusy = errors.New("the connection is already lent to another program")

// port is one consumer's view of the connection. Identity is the pointer.
type port struct{ m *connMux }

// newConnMux starts pumping src and returns the mux.
//
// The pump goroutine lives as long as the connection: it exits once src returns
// an error, which is what a closed session does. Close makes blocked consumers
// return, but it cannot interrupt a read already in flight — that is the whole
// premise of this type — so the caller ends the pump by ending the connection.
func newConnMux(src io.Reader, dst io.Writer) *connMux {
	m := &connMux{src: src, dst: dst}
	m.ready = sync.NewCond(&m.mu)
	m.tui = &port{m: m}
	m.active = m.tui
	go m.pump()
	return m
}

// TUI returns the port the terminal UI reads and writes through. It is active
// whenever nothing has been borrowed.
func (m *connMux) TUI() io.ReadWriter { return m.tui }

// Borrow gives fn exclusive use of the connection until it returns.
//
// While fn runs, the TUI's reads block and its writes are discarded. When fn
// returns — whether it succeeded or not — the TUI becomes active again and
// onResume fires.
func (m *connMux) Borrow(fn func(io.ReadWriter) error) error {
	p := &port{m: m}
	if err := m.activate(p); err != nil {
		return err
	}
	defer m.restore()
	return fn(p)
}

// Close makes every blocked consumer return. See newConnMux on the pump.
func (m *connMux) Close() {
	m.mu.Lock()
	m.closed = true
	m.ready.Broadcast()
	m.mu.Unlock()
}

func (m *connMux) activate(p *port) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return io.ErrClosedPipe
	}
	if m.active != m.tui {
		return errMuxBusy
	}
	m.active = p
	m.ready.Broadcast()
	return nil
}

func (m *connMux) restore() {
	m.mu.Lock()
	m.active = m.tui
	m.ready.Broadcast()
	m.mu.Unlock()

	// Outside the lock: onResume sends to the Bubble Tea program, which renders,
	// and rendering comes back through Write.
	if m.onResume != nil {
		m.onResume()
	}
}

// pump owns src.Read for the life of the connection.
func (m *connMux) pump() {
	buf := make([]byte, 4096)
	for {
		m.mu.Lock()
		for len(m.pend) > 0 && !m.closed {
			m.ready.Wait()
		}
		if m.closed {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()

		n, err := m.src.Read(buf)

		m.mu.Lock()
		if n > 0 {
			m.pend = append([]byte(nil), buf[:n]...)
		}
		if err != nil {
			m.err = err
		}
		stop := m.err != nil || m.closed
		m.ready.Broadcast()
		m.mu.Unlock()

		if stop {
			return
		}
	}
}

// Read blocks until this port is active and has bytes, or the stream ends.
func (p *port) Read(b []byte) (int, error) {
	m := p.m
	if len(b) == 0 {
		return 0, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		mine := m.active == p
		if mine && len(m.pend) > 0 {
			n := copy(b, m.pend)
			m.pend = m.pend[n:]
			if len(m.pend) == 0 {
				m.pend = nil
				// The pump is waiting for exactly this.
				m.ready.Broadcast()
			}
			return n, nil
		}
		// A dead connection ends every consumer, not just the active one:
		// a suspended TUI whose session has gone must not park forever waiting
		// for a turn that is never coming. The active port drains first, so
		// nothing already received is thrown away.
		if m.err != nil && (!mine || len(m.pend) == 0) {
			return 0, m.err
		}
		if m.closed {
			return 0, io.EOF
		}
		m.ready.Wait()
	}
}

// Write sends to the connection, or discards when this port is not active.
func (p *port) Write(b []byte) (int, error) {
	m := p.m

	m.wmu.Lock()
	defer m.wmu.Unlock()

	m.mu.Lock()
	active := m.active == p && !m.closed
	m.mu.Unlock()
	if !active {
		// Suppressed, not failed — see the note on suppressed writes.
		return len(b), nil
	}
	return m.dst.Write(b)
}
