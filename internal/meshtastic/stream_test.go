package meshtastic

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/meshtastic/meshpb"
	"google.golang.org/protobuf/proto"
)

// radioSide is the node's end of a fake connection.
type radioSide struct {
	t    *testing.T
	conn net.Conn
}

// send writes one FromRadio, framed as the firmware would.
func (r *radioSide) send(m *meshpb.FromRadio) {
	r.t.Helper()
	body, err := proto.Marshal(m)
	if err != nil {
		r.t.Error(err)
		return
	}
	buf, err := AppendFrame(nil, body)
	if err != nil {
		r.t.Error(err)
		return
	}
	if _, err := r.conn.Write(buf); err != nil {
		return // the client hung up; the test that cares will notice
	}
}

// sendRaw writes bytes that are not a frame — device log output, noise.
func (r *radioSide) sendRaw(b []byte) {
	if _, err := r.conn.Write(b); err != nil {
		return
	}
}

// startFakeRadio runs a node that answers ToRadio messages with handle, and
// returns the client side of the connection.
//
// It is a real stream with real framing in both directions, not a mock: the
// framing is the part of this package most likely to be wrong, so a test double
// that skipped it would exercise the wrong half.
func startFakeRadio(t *testing.T, opts Options, handle func(r *radioSide, m *meshpb.ToRadio)) *Conn {
	t.Helper()
	client, server := net.Pipe()
	go serveRadio(t, server, handle)

	c := NewConn(client, opts)
	t.Cleanup(func() { c.Close() })
	return c
}

// serveRadio is the node's side of a connection: read ToRadio, call handle.
func serveRadio(t *testing.T, server net.Conn, handle func(r *radioSide, m *meshpb.ToRadio)) {
	defer server.Close()
	rs := &radioSide{t: t, conn: server}
	fr := NewFrameReader(server, nil)
	for {
		body, err := fr.ReadFrame()
		if err != nil {
			return
		}
		var m meshpb.ToRadio
		if err := proto.Unmarshal(body, &m); err != nil {
			t.Errorf("radio received an unparseable ToRadio: %v", err)
			return
		}
		if handle != nil {
			handle(rs, &m)
		}
	}
}

func TestConnSendAndRecv(t *testing.T) {
	c := startFakeRadio(t, Options{Name: "fake"}, func(r *radioSide, m *meshpb.ToRadio) {
		if m.GetHeartbeat() == nil {
			t.Errorf("radio got %T, want a heartbeat", m.GetPayloadVariant())
		}
		r.send(&meshpb.FromRadio{
			PayloadVariant: &meshpb.FromRadio_MyInfo{MyInfo: &meshpb.MyNodeInfo{MyNodeNum: 0xDEADBEEF}},
		})
	})

	if err := c.Heartbeat(); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	msg, err := c.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if got := msg.GetMyInfo().GetMyNodeNum(); got != 0xDEADBEEF {
		t.Errorf("my_node_num = %#x", got)
	}
}

// Device output interleaved with frames must not disturb message delivery, and
// must reach the log sink.
func TestConnRecvSurvivesInterleavedDeviceLog(t *testing.T) {
	var logs []string
	c := startFakeRadio(t, Options{OnDeviceLog: func(s string) { logs = append(logs, s) }},
		func(r *radioSide, m *meshpb.ToRadio) {
			r.sendRaw([]byte("INFO | Radio ready\r\n"))
			r.send(&meshpb.FromRadio{
				PayloadVariant: &meshpb.FromRadio_ConfigCompleteId{ConfigCompleteId: 7},
			})
		})

	if err := c.Heartbeat(); err != nil {
		t.Fatal(err)
	}
	msg, err := c.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if msg.GetConfigCompleteId() != 7 {
		t.Errorf("config_complete_id = %d, want 7", msg.GetConfigCompleteId())
	}
	if !equalStrings(logs, []string{"INFO | Radio ready"}) {
		t.Errorf("device log = %q", logs)
	}
}

// Wake must be bytes the device's parser cannot mistake for a header start.
func TestWakeWritesOnlyStart2(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(nopCloser{&buf}, Options{})
	if err := c.Wake(); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if len(got) != 32 {
		t.Fatalf("wrote %d bytes, want 32", len(got))
	}
	if bytes.IndexByte(got, start1) >= 0 {
		t.Error("wake sequence contains start1, so a node mid-frame could read it as a header")
	}
	for i, b := range got {
		if b != start2 {
			t.Fatalf("byte %d = %#x, want start2", i, b)
		}
	}
}

func TestSendRejectsOversizeMessage(t *testing.T) {
	var buf bytes.Buffer
	c := NewConn(nopCloser{&buf}, Options{})
	// A ToRadio big enough to exceed one frame. The firmware would reject it;
	// better to fail here than to write a frame nothing can parse.
	big := &meshpb.ToRadio{PayloadVariant: &meshpb.ToRadio_Packet{Packet: &meshpb.MeshPacket{
		PayloadVariant: &meshpb.MeshPacket_Encrypted{Encrypted: make([]byte, MaxFrame+1)},
	}}}
	if err := c.Send(big); err == nil {
		t.Fatal("Send accepted a message larger than MaxFrame")
	}
	if buf.Len() != 0 {
		t.Errorf("wrote %d bytes of a rejected message", buf.Len())
	}
}

func TestCloseIsIdempotentAndUnblocksRecv(t *testing.T) {
	c := startFakeRadio(t, Options{}, nil)

	errc := make(chan error, 1)
	go func() {
		_, err := c.Recv()
		errc <- err
	}()

	// Give Recv a moment to block on the read before closing under it.
	time.Sleep(20 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}

	select {
	case err := <-errc:
		if err == nil {
			t.Error("Recv returned nil error after Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock a pending Recv")
	}

	if err := c.Send(&meshpb.ToRadio{}); err == nil {
		t.Error("Send succeeded after Close")
	}
}

type nopCloser struct{ io.Writer }

func (nopCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (nopCloser) Close() error             { return nil }
