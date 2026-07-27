package meshtastic

import (
	"fmt"
	"io"
	"sync"

	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"google.golang.org/protobuf/proto"
)

// Conn is a framed protobuf stream to a local Meshtastic node.
//
// Serial and TCP differ only in how the stream is opened: the framing, the
// messages and the session are identical, which is why DialSerial and DialTCP
// both return this type.
//
// One goroutine may Send while another Recv's. Two concurrent Recv's are not
// supported and would interleave halves of a frame.
type Conn struct {
	rwc   io.ReadWriteCloser
	fr    *FrameReader
	name  string
	onLog func(string)

	mu     sync.Mutex
	closed bool
	// wbuf is reused across sends. Frames are small and this is on the hot path
	// for every packet the BBS transmits.
	wbuf []byte
}

// Options configures a Conn.
type Options struct {
	// Name identifies the connection in logs and the sysop status screen,
	// e.g. "serial:/dev/ttyUSB0" or "tcp:mesh.local:4403".
	Name string
	// OnDeviceLog receives the node's own debug output, which on serial is
	// interleaved with frames. Optional.
	OnDeviceLog func(string)
}

// NewConn wraps an already-open stream. The Conn takes ownership: Close closes
// the underlying stream.
func NewConn(rwc io.ReadWriteCloser, opts Options) *Conn {
	name := opts.Name
	if name == "" {
		name = "meshtastic"
	}
	return &Conn{
		rwc:   rwc,
		fr:    NewFrameReader(rwc, opts.OnDeviceLog),
		name:  name,
		onLog: opts.OnDeviceLog,
	}
}

// Name identifies the connection.
func (c *Conn) Name() string { return c.name }

// deviceLog routes firmware log output to the sink.
//
// Two paths reach it: raw text between frames on serial, and LogRecord
// messages, which is how the same output arrives over TCP. A sysop debugging a
// radio should not have to care which transport they happen to be on.
func (c *Conn) deviceLog(s string) {
	if c.onLog != nil && s != "" {
		c.onLog(s)
	}
}

// Send marshals and writes one ToRadio message.
func (c *Conn) Send(m *meshpb.ToRadio) error {
	// Deterministic marshalling costs nothing here and keeps §6.2.1's encoding
	// discipline uniform across the codebase: the moment one encoder is allowed
	// to be map-order dependent, the habit spreads to one whose output is
	// signed.
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal ToRadio: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	c.wbuf = c.wbuf[:0]
	c.wbuf, err = AppendFrame(c.wbuf, body)
	if err != nil {
		return err
	}
	if _, err := c.rwc.Write(c.wbuf); err != nil {
		return fmt.Errorf("write to %s: %w", c.name, err)
	}
	return nil
}

// Recv reads the next FromRadio message. It blocks until one arrives, the
// stream ends, or the Conn is closed.
func (c *Conn) Recv() (*meshpb.FromRadio, error) {
	body, err := c.fr.ReadFrame()
	if err != nil {
		return nil, err
	}
	var m meshpb.FromRadio
	if err := proto.Unmarshal(body, &m); err != nil {
		// A frame that is not a FromRadio is a real error, not something to
		// skip past: the framing said this was a complete message and it was
		// not, so the stream is either corrupt or from firmware we do not
		// understand. Reporting it beats silently dropping radio traffic.
		return nil, fmt.Errorf("unmarshal FromRadio from %s (%d bytes): %w", c.name, len(body), err)
	}
	return &m, nil
}

// Wake nudges a sleeping or desynchronised node.
//
// It writes 32 start2 bytes and deliberately NOT start1: the point is to feed
// the device's own frame parser bytes that cannot begin a header, so a node
// left mid-frame by a previous client discards its partial state instead of
// mistaking our first real frame for the tail of an old one. This mirrors what
// the reference client does on connect, and costs one write.
func (c *Conn) Wake() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	var wake [32]byte
	for i := range wake {
		wake[i] = start2
	}
	_, err := c.rwc.Write(wake[:])
	return err
}

// Heartbeat tells the node we are still here.
//
// The firmware drops an idle client after a few minutes — a serial-specific
// behaviour that also applies to TCP — and a BBS that transmits once every
// fifteen to thirty minutes (§7.3) is idle by that standard almost all the
// time. Scheduling this belongs to the link that owns the connection, not here.
func (c *Conn) Heartbeat() error {
	return c.Send(&meshpb.ToRadio{
		PayloadVariant: &meshpb.ToRadio_Heartbeat{Heartbeat: &meshpb.Heartbeat{}},
	})
}

// Skipped reports bytes discarded outside frames; see FrameReader.Skipped.
func (c *Conn) Skipped() uint64 { return c.fr.Skipped() }

// Frames reports frames successfully read; see FrameReader.Frames.
func (c *Conn) Frames() uint64 { return c.fr.Frames() }

// Close shuts the stream down. It is idempotent, and unblocks a concurrent
// Recv — which is the only way to interrupt one, since neither a serial port
// nor a blocked protobuf read has anything finer to cancel.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	return c.rwc.Close()
}
