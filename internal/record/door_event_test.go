package record

import (
	"bytes"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/rng"
)

func testNode(t *testing.T, seed uint64) identity.NodeID {
	t.Helper()
	key, err := identity.GenerateNodeKey(rng.TestSecret(seed))
	if err != nil {
		t.Fatal(err)
	}
	return key.ID()
}

// oneKill is the commonest shape: an actor, a cross-board target, a small
// door-defined payload.
func oneKill(t *testing.T) DoorEventBody {
	t.Helper()
	return DoorEventBody{
		Game: "lord",
		Events: []DoorEvent{{
			Kind:       3,
			Actor:      "alice",
			Target:     "bob",
			TargetNode: testNode(t, 11),
			Payload:    []byte{0xde, 0xad, 0xbe, 0xef},
		}},
	}
}

func TestDoorEventBodyRoundTrips(t *testing.T) {
	cases := map[string]DoorEventBody{
		"one kill": oneKill(t),
		"no target, no payload": {
			Game:   "tw2002",
			Events: []DoorEvent{{Kind: 1, Actor: "carol"}},
		},
		"target but no payload": {
			Game:   "lord",
			Events: []DoorEvent{{Kind: 9, Actor: "dex", Target: "eli", TargetNode: testNode(t, 12)}},
		},
		"payload but no target": {
			Game:   "lord",
			Events: []DoorEvent{{Kind: 0, Actor: "fen", Payload: []byte{1, 2, 3}}},
		},
		"a full batch": {
			Game: "lord",
			Events: func() []DoorEvent {
				var out []DoorEvent
				for i := 0; i < MaxDoorEventsPerRecord; i++ {
					out = append(out, DoorEvent{
						Kind: uint8(i), Actor: "gus", Payload: []byte{byte(i)},
					})
				}
				return out
			}(),
		},
		"the largest legal entry": {
			Game: strings.Repeat("g", MaxDoorGameLen),
			Events: []DoorEvent{{
				Kind:       255,
				Actor:      strings.Repeat("a", MaxNickLen),
				Target:     strings.Repeat("t", MaxNickLen),
				TargetNode: testNode(t, 13),
				Payload:    bytes.Repeat([]byte{0xFF}, MaxDoorEventPayloadLen),
			}},
		},
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			enc, err := MarshalDoorEventBody(body)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := len(enc), DoorEventBodyLen(body); got != want {
				t.Errorf("encoded %d bytes, DoorEventBodyLen said %d", got, want)
			}

			got, err := UnmarshalDoorEventBody(enc)
			if err != nil {
				t.Fatal(err)
			}
			if got.Game != body.Game || len(got.Events) != len(body.Events) {
				t.Fatalf("round trip changed the shape: %+v", got)
			}
			for i, want := range body.Events {
				g := got.Events[i]
				if g.Kind != want.Kind || g.Actor != want.Actor ||
					g.Target != want.Target || g.TargetNode != want.TargetNode ||
					!bytes.Equal(g.Payload, want.Payload) {
					t.Errorf("event %d round-tripped as %+v, want %+v", i, g, want)
				}
			}

			// Determinism: §6.2.1 rule 2 is about maps, and the surest way to
			// keep this format clear of that is for the same body to always
			// produce the same bytes.
			for i := 0; i < 4; i++ {
				again, err := MarshalDoorEventBody(body)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(again, enc) {
					t.Fatal("marshalling the same body twice produced different bytes")
				}
			}
		})
	}
}

func TestDoorEventBodyRejectsBadInput(t *testing.T) {
	node := testNode(t, 11)
	long := strings.Repeat("x", MaxNickLen+1)

	cases := map[string]DoorEventBody{
		"no game":       {Events: []DoorEvent{{Actor: "a"}}},
		"game too long": {Game: strings.Repeat("g", MaxDoorGameLen+1), Events: []DoorEvent{{Actor: "a"}}},
		"game with a control character": {
			Game: "lo\x1brd", Events: []DoorEvent{{Actor: "a"}},
		},
		"no events": {Game: "lord"},
		"too many events": {Game: "lord", Events: func() []DoorEvent {
			var out []DoorEvent
			for i := 0; i <= MaxDoorEventsPerRecord; i++ {
				out = append(out, DoorEvent{Actor: "a"})
			}
			return out
		}()},
		"no actor":       {Game: "lord", Events: []DoorEvent{{Kind: 1}}},
		"actor too long": {Game: "lord", Events: []DoorEvent{{Actor: long}}},
		"actor with a control character": {
			Game: "lord", Events: []DoorEvent{{Actor: "ali\x07ce"}},
		},
		"target too long": {
			Game: "lord", Events: []DoorEvent{{Actor: "a", Target: long, TargetNode: node}},
		},
		"target on no node": {
			Game: "lord", Events: []DoorEvent{{Actor: "a", Target: "bob"}},
		},
		"target node with no target": {
			Game: "lord", Events: []DoorEvent{{Actor: "a", TargetNode: node}},
		},
		"payload too long": {
			Game: "lord",
			Events: []DoorEvent{{
				Actor:   "a",
				Payload: bytes.Repeat([]byte{0}, MaxDoorEventPayloadLen+1),
			}},
		},
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := MarshalDoorEventBody(body); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A hostile body must not be able to crash, over-allocate, or smuggle a second
// wire form past the parser.
func TestUnmarshalRejectsHostileDoorEventBodies(t *testing.T) {
	good, err := MarshalDoorEventBody(oneKill(t))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("truncated at every boundary", func(t *testing.T) {
		for i := 0; i < len(good); i++ {
			if _, err := UnmarshalDoorEventBody(good[:i]); err == nil {
				t.Errorf("accepted a body truncated to %d of %d bytes", i, len(good))
			}
		}
	})

	t.Run("trailing bytes", func(t *testing.T) {
		if _, err := UnmarshalDoorEventBody(append(append([]byte(nil), good...), 0)); err == nil {
			t.Error("accepted a body with a trailing byte")
		}
	})

	t.Run("a count larger than the entries present", func(t *testing.T) {
		// The count byte follows the game length and the game name.
		b := append([]byte(nil), good...)
		b[1+len("lord")] = MaxDoorEventsPerRecord
		if _, err := UnmarshalDoorEventBody(b); err == nil {
			t.Error("accepted a body claiming eight events and carrying one")
		}
	})

	t.Run("a count beyond the limit is refused before allocating", func(t *testing.T) {
		b := append([]byte(nil), good...)
		b[1+len("lord")] = 255
		if _, err := UnmarshalDoorEventBody(b); err == nil {
			t.Error("accepted a body claiming 255 events")
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, err := UnmarshalDoorEventBody(nil); err == nil {
			t.Error("accepted an empty body")
		}
	})
}

// §12.7: the byte budget is an assertion, not a hope. A field added later shows
// up here as a red build rather than as a mesh that has quietly got slower.
func TestDoorEventFitsTheBudget(t *testing.T) {
	typical := DoorEventBody{
		Game:   "lord",
		Events: []DoorEvent{{Kind: 3, Actor: "alice", Payload: []byte{1, 2, 3, 4, 5, 6, 7, 8}}},
	}
	if got := DoorEventBodyLen(typical); got > 32 {
		t.Errorf("a typical single event is %d bytes, expected no more than 32", got)
	}

	worst := DoorEventBody{Game: strings.Repeat("g", MaxDoorGameLen)}
	for i := 0; i < MaxDoorEventsPerRecord; i++ {
		worst.Events = append(worst.Events, DoorEvent{
			Kind:       255,
			Actor:      strings.Repeat("a", MaxNickLen),
			Target:     strings.Repeat("t", MaxNickLen),
			TargetNode: testNode(t, 14),
			Payload:    bytes.Repeat([]byte{0xFF}, MaxDoorEventPayloadLen),
		})
	}
	n := DoorEventBodyLen(worst)
	if n > MaxDoorEventBodyLen {
		t.Errorf("the worst legal body is %d bytes, over the %d ceiling", n, MaxDoorEventBodyLen)
	}
	if n >= MaxBodyLen {
		t.Errorf("the worst legal body is %d bytes, which reaches the generic %d limit", n, MaxBodyLen)
	}
	// The whole design rests on this ratio: batching inside the record beats
	// batching records, because the signature dominates everything else.
	batched := DoorEventBody{Game: "lord"}
	for i := 0; i < 6; i++ {
		batched.Events = append(batched.Events, typical.Events[0])
	}
	separate := 6 * DoorEventSize(typical)
	together := DoorEventSize(batched)
	if ratio := float64(separate) / float64(together); ratio < 3.0 {
		t.Errorf("batching six events saves only %.1fx (%d bytes vs %d); "+
			"§9.5's case for batching inside the record rests on about 3.3x",
			ratio, separate, together)
	}
}

// §7.5 has no exception for door events. The bound on a payload is what makes
// that true by arithmetic, so it is asserted rather than assumed.
func TestDoorEventCannotCarryFileContent(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(22))
	if err != nil {
		t.Fatal(err)
	}
	area := AreaTagFor("league")

	t.Run("raw content does not parse as a batch", func(t *testing.T) {
		for _, size := range []int{64, 512, 4 << 10, MaxBodyLen} {
			if _, err := UnmarshalDoorEventBody(bytes.Repeat([]byte{0xFF}, size)); err == nil {
				t.Errorf("%d bytes of content parsed as a door event batch", size)
			}
		}
	})

	t.Run("a record cannot be minted around content", func(t *testing.T) {
		for _, size := range []int{512, 4 << 10, MaxBodyLen} {
			_, err := New(key, Record{
				Origin: key.ID(), Seq: 1, TS: 1, Type: TypeDoorEvent, Area: area,
				Body: bytes.Repeat([]byte{0xFF}, size),
			})
			if err == nil {
				t.Errorf("minted a DOOR_EVENT carrying %d bytes of content", size)
			}
		}
	})

	t.Run("content appended to a valid batch is refused", func(t *testing.T) {
		good, err := MarshalDoorEventBody(oneKill(t))
		if err != nil {
			t.Fatal(err)
		}
		stuffed := append(append([]byte(nil), good...), bytes.Repeat([]byte{0xAA}, 4096)...)
		if _, err := UnmarshalDoorEventBody(stuffed); err == nil {
			t.Error("accepted a batch with 4 KiB appended")
		}
	})
}

func TestDoorEventRecordRoundTrips(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(23))
	if err != nil {
		t.Fatal(err)
	}
	body := oneKill(t)

	r, err := NewDoorEventRecord(key, 7, 1765310400, AreaTagFor("league"), body)
	if err != nil {
		t.Fatal(err)
	}
	if r.Type != TypeDoorEvent {
		t.Fatalf("type is %s", r.Type)
	}

	got, err := VerifyDoorEventRecord(r, key.Public)
	if err != nil {
		t.Fatal(err)
	}
	if got.Game != body.Game || len(got.Events) != 1 || got.Events[0].Actor != "alice" {
		t.Fatalf("verified body is %+v", got)
	}

	// And it must survive the envelope codec, which is where a peer's copy
	// arrives.
	back, err := Unmarshal(r.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if back.ID() != r.ID() {
		t.Errorf("record ID changed across the wire: %s vs %s", back.ID(), r.ID())
	}
}

// A door event signed by the wrong key must not verify, and a record of another
// type must not be read as one.
func TestDoorEventRecordRefusesTheWrongThing(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(24))
	if err != nil {
		t.Fatal(err)
	}
	other, err := identity.GenerateNodeKey(rng.TestSecret(25))
	if err != nil {
		t.Fatal(err)
	}

	r, err := NewDoorEventRecord(key, 1, 1, AreaTagFor("league"), oneKill(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDoorEventRecord(r, other.Public); err == nil {
		t.Error("verified against the wrong key")
	}

	post, err := New(key, Record{
		Origin: key.ID(), Seq: 2, TS: 1, Type: TypePost,
		Area: AreaTagFor("league"), Body: []byte("hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyDoorEventRecord(post, key.Public); err == nil {
		t.Error("read a POST as a DOOR_EVENT")
	}
}

// FuzzUnmarshalDoorEventBody is the target that found FILE's second wire form
// within seconds of existing. The same three properties are asserted here.
func FuzzUnmarshalDoorEventBody(f *testing.F) {
	good, err := MarshalDoorEventBody(oneKill(&testing.T{}))
	if err == nil {
		f.Add(good)
	}
	minimal, err := MarshalDoorEventBody(DoorEventBody{
		Game: "l", Events: []DoorEvent{{Actor: "a"}},
	})
	if err == nil {
		f.Add(minimal)
	}
	f.Add([]byte{0})
	f.Add([]byte{})
	f.Add([]byte{1, 'l', 1, 0, 1, 'a', 0, 0})

	f.Fuzz(func(t *testing.T, b []byte) {
		body, err := UnmarshalDoorEventBody(b)
		if err != nil {
			return
		}

		// 1. Anything that parses must be the canonical encoding of itself.
		reencoded, err := MarshalDoorEventBody(body)
		if err != nil {
			t.Fatalf("a parsed body would not re-encode: %v", err)
		}
		if !bytes.Equal(reencoded, b) {
			t.Fatalf("second wire form: %x parsed to something that encodes as %x", b, reencoded)
		}

		// 2. No control character reaches a field that gets drawn on a
		//    terminal. Game, actor and target are all rendered.
		for _, s := range []string{body.Game} {
			if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Fatalf("control character survived into the game name %q", s)
			}
		}
		for _, e := range body.Events {
			for _, s := range []string{e.Actor, e.Target} {
				if strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
					t.Fatalf("control character survived into a rendered name %q", s)
				}
			}
			// 3. The payload is deliberately NOT checked for control
			//    characters — it is opaque and a door may put anything in it.
			//    Which is exactly why a renderer must never print one raw, and
			//    why this absence is written down rather than left to be read
			//    as an oversight.
			if len(e.Payload) > MaxDoorEventPayloadLen {
				t.Fatalf("payload of %d bytes survived the bound", len(e.Payload))
			}
		}
		if len(body.Events) == 0 || len(body.Events) > MaxDoorEventsPerRecord {
			t.Fatalf("body parsed with %d events", len(body.Events))
		}
	})
}
