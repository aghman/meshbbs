package door

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// The door API's request parser, fuzzed (§12.5).
//
// Every other target in this suite parses something that arrived over the mesh
// from a stranger. This one parses something that arrived from a THIRD-PARTY
// BINARY the sysop installed, running on the sysop's own machine with the
// server's privileges (§9.4). That is a different threat and, in one respect, a
// worse one: a door does not have to get past a channel PSK first, and it can
// send a million malformed requests a second from a process nobody is watching.
//
// The property under test is deliberately weak and absolute: whatever it sends,
// the server neither panics nor stops serving. It is not that the input is
// understood — most of it will not be — but that a door cannot take the BBS
// down by talking nonsense to it.
func FuzzAPIRequest(f *testing.F) {
	seeds := []string{
		`{"id":1,"op":"session.get"}`,
		`{"id":2,"op":"state.set","key":"k","value":"v"}`,
		`{"id":3,"op":"state.get","scope":"global","key":"k"}`,
		`{"id":4,"op":"announce","subject":"s","text":"t"}`,
		`{"id":5,"op":"user.post","area":"general","subject":"s","text":"t"}`,
		`{"id":6,"op":"user.dm","to":"bob","text":"t"}`,
		`{"op":"hello","token":"wrong"}`,
		``,
		`{`,
		`[]`,
		`null`,
		`{"id":"not-a-number","op":"session.get"}`,
		`{"op":"state.set","scope":"../../etc","key":"k"}`,
		`{"op":"session.get"}` + "\n" + `{"op":"state.keys"}`,
		"\x00\x00\x00",
		strings.Repeat(`{"op":"session.get"}`+"\n", 8),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		host := newFakeHost()
		mgr := New(realClock(), discardLogger())
		mgr.SetHost(host)

		// Level 4 so that no operation is turned away by the level check before
		// its own parsing runs — the point is to reach every handler.
		a := &apiServer{
			mgr:  mgr,
			host: host,
			spec: Spec{Name: "fuzzdoor", Grant: Grant{
				Level: 4, AnnounceArea: "games", AnnouncePerHour: 1000, StateQuota: 4096,
			}},
			sess:    Session{Nick: "alice", Node: 1, Width: 80, Height: 24},
			token:   "fuzz-token",
			closing: make(chan struct{}),
			conns:   map[net.Conn]struct{}{},
		}

		server, client := net.Pipe()
		served := make(chan struct{})
		go func() {
			defer close(served)
			a.serveConn(server)
		}()

		// Drain replies. net.Pipe is unbuffered, so a server writing with
		// nobody reading would deadlock and be reported as a hang rather than
		// as the parser bug this is looking for.
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			_, _ = bufio.NewReader(client).WriteTo(discardWriter{})
		}()

		deadline := time.Now().Add(5 * time.Second)
		_ = client.SetDeadline(deadline)

		// Say hello first, so the fuzzed bytes reach dispatch rather than
		// stopping at the handshake. The handshake itself is covered by the
		// unfuzzed tests, and by whatever the corpus sends before this line
		// lands in a later iteration.
		_, _ = client.Write([]byte(`{"id":0,"op":"hello","token":"fuzz-token"}` + "\n"))
		_, _ = client.Write(data)
		_, _ = client.Write([]byte("\n"))
		_ = client.Close()

		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Fatalf("the server did not finish with %q", data)
		}
		<-drained
	})
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
