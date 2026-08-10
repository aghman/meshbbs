package record

import (
	"bytes"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
)

// MaxDoorGameLen bounds the game name a DOOR_EVENT record is about.
//
// Short because it is a KEY, not prose: every sysop in a league must type it
// identically for their boards to be playing the same game, it is matched
// exactly, and it is never rendered as a sentence. It is also hoisted to the
// record rather than repeated per event (see DoorEventBody), so 16 bytes is
// paid once however many events ride along.
const MaxDoorGameLen = 16

// MaxDoorEventsPerRecord bounds how many events one record carries.
//
// # Why events batch inside the record rather than across records
//
// A record's framing is 89 bytes, of which 64 is the signature. A LORD kill is
// 23 bytes of news. One event per record therefore spends 79% of the wire on
// framing, and §9.5's "small and batched" cannot mean batching records
// into bundles — a bundle already hoists origin, area and base timestamp
// (§6.1.3), but every record inside it keeps its own signature. The only
// batching that pays is inside the body. Measured, not estimated — these are
// the numbers TestDoorEventFitsTheBudget computes from this codec:
//
//	six events, one per record:  6 x 112 = 672 bytes
//	the same six in one record:            197 bytes
//
// Eight is the cap because it keeps the worst possible body under a kilobyte
// while capturing nearly all of that 3.3x. A ninth event saves proportionally
// almost nothing and costs a bigger worst case on a link where the worst case
// is what the governor must budget for.
const MaxDoorEventsPerRecord = 8

// MaxDoorEventPayloadLen bounds the door-defined payload on one event.
//
// # Why there is an opaque field at all
//
// MeshBBS cannot model TradeWars' state or enumerate LORD's events, and trying
// would be a worse design than admitting it. So each event carries a small
// blob whose meaning belongs to the door.
//
// # Why it is safe to have one
//
// It is one bounded byte string, not a key/value map. A map would be the
// §6.2.1 rule-2 hazard — Go randomises iteration order — and would need a
// canonical-ordering rule layered on top of the codec. A length-prefixed blob
// is deterministic for free, and any ordering question inside it is the door's
// problem, not the wire's.
//
// And 48 bytes keeps §7.5 true by arithmetic rather than by inspection. A door
// could in principle drip file content through payloads; at 48 bytes an event,
// 8 events a record, and a league area's share of roughly one packet a day,
// moving a megabyte takes on the order of decades. That is the same argument
// checkNoFileContent makes for FILE — the bound is what makes the abuse
// uninteresting — and it is why the limit is not a tuning knob.
//
// A door that wants more than 48 bytes is doing state synchronisation, which is
// not what this is. The refusal says so, because "limit is 48" invites a
// workaround and "this is not the feature you want" does not.
const MaxDoorEventPayloadLen = 48

// MaxDoorEventBodyLen is a whole-body ceiling, belt to the per-field braces.
//
// The largest body the field limits permit is 882 bytes, comfortably under
// MaxBodyLen's 8 KiB — but "comfortably" is doing load-bearing work in that
// sentence, and a future field could quietly erode it. This is the explicit
// second check, asserted in both directions by TestDoorEventFitsTheBudget.
//
// Worth stating plainly: at 882 bytes the codec is not the binding constraint
// anyway. The governor is. One worst-case record is four full packets, and a
// node originates about eleven a day at R = 4.
const MaxDoorEventBodyLen = 1024

// DoorEvent is one thing that happened in a door game.
//
// # What it does NOT carry
//
// The actor's node. That is the record's Origin. Eight bytes restating the
// envelope, on a link where §7.5 counts every one.
//
// A round number, sequence or game clock. Causality is (origin, seq) and a
// record's TS is advisory (§6.2.1). A game that needs rounds encodes them in
// its payload, where the rule that clocks are advisory does not bind it.
type DoorEvent struct {
	// Kind is a door-defined event code.
	//
	// A uint8 rather than a string because MeshBBS cannot enumerate any door's
	// events and a name would be paid on every entry. 256 codes per game is
	// more than any door will use, and an unknown code is forward-compatible by
	// construction: a receiver renders "event 7" and passes the payload on.
	Kind uint8

	// Actor is the nick the event is about, on the origin's own board.
	//
	// Bounded by MaxNickLen deliberately rather than by a new limit: a door
	// event names a nick, nicks are already bounded at 24 for exactly this
	// reason, and a second nick bound would be two rules for one thing.
	Actor string

	// Target is the other party, if there is one. Empty costs one byte.
	Target string

	// TargetNode is the board Target is on. Set if and only if Target is.
	//
	// It cannot be inferred from Origin the way the actor's node can — the whole
	// point of a league is that "alice slew bob@pnw" crosses boards. It is a
	// CLAIM by the origin node rather than a verified fact, and §9.5 already
	// says so: integrity against your own league members needs commit-reveal or
	// cross-checked game state, not a signature layer.
	TargetNode identity.NodeID

	// Payload is door-defined bytes, possibly empty. See MaxDoorEventPayloadLen.
	Payload []byte
}

// DoorEventBody is the payload of a DOOR_EVENT record (§9.5).
//
// One record is about one game. The name is hoisted here rather than repeated
// per event for the same reason §6.1.3 hoists origin into a bundle header: a
// league area usually carries one game, and paying 17 bytes per record instead
// of per event is most of the win at no cost in expressiveness — two games mean
// two records, which is what the area would have needed anyway.
//
// # What it does NOT carry
//
// A league or area name. The league IS the area, and the area tag is already in
// the envelope and hoisted into the bundle header besides.
//
// An elision for "same actor as the previous entry". It would save about six
// bytes an entry and was rejected on purpose: it makes canonicality depend on a
// rule the encoder must never forget ("a repeat MUST be elided"), which is
// precisely the class of bug the re-encode check exists to catch rather than to
// rely on. Revisit before the Phase 6 freeze if measurement asks for it.
type DoorEventBody struct {
	Game   string
	Events []DoorEvent
}

// DoorEventBodyLen reports the encoded size of a body.
func DoorEventBodyLen(d DoorEventBody) int {
	n := 1 + len(d.Game) + 1
	for _, e := range d.Events {
		// Four fixed bytes an entry: kind, and a length for each of actor,
		// target and payload. A target's own bytes and its node ride along only
		// when there is one.
		n += 4 + len(e.Actor) + len(e.Payload)
		if e.Target != "" {
			n += len(e.Target) + identity.NodeIDLen
		}
	}
	return n
}

// validateDoorEvent applies the wire's rules to one event.
//
// Shared by marshal and unmarshal so a hostile body is rejected by the same
// rules that governed writing it, rather than by whatever the reader assumes.
func validateDoorEvent(e DoorEvent) error {
	if e.Actor == "" {
		return fmt.Errorf("door event has no actor")
	}
	if err := validateLabel("door event actor", e.Actor, MaxNickLen); err != nil {
		return err
	}
	if e.Target == "" {
		// A target node with no nick is unrepresentable on the wire rather than
		// merely refused — the eight bytes are only written when a target is —
		// so this catches a caller who set one and would otherwise have watched
		// it vanish silently.
		if !e.TargetNode.IsZero() {
			return fmt.Errorf("door event has a target node but no target nick")
		}
	} else {
		if err := validateLabel("door event target", e.Target, MaxNickLen); err != nil {
			return err
		}
		if e.TargetNode.IsZero() {
			// A zero node addresses nothing, so an entry carrying one could
			// never be resolved to a board by any receiver. Same reasoning as
			// FILE's all-zero hash.
			return fmt.Errorf("door event names target %q on no node", e.Target)
		}
	}
	if len(e.Payload) > MaxDoorEventPayloadLen {
		return fmt.Errorf("door event payload is %d bytes, limit is %d — a door that "+
			"needs more than this is doing state synchronisation, which is not what "+
			"DOOR_EVENT is for (§9.5)", len(e.Payload), MaxDoorEventPayloadLen)
	}
	// The payload is deliberately NOT checked for control characters. It is
	// opaque bytes and a door may legitimately put anything in it — which is
	// exactly why a renderer must never print one raw.
	return nil
}

// validateDoorEventBody applies the rules that govern a whole body.
func validateDoorEventBody(d DoorEventBody) error {
	if d.Game == "" {
		return fmt.Errorf("door event body names no game")
	}
	if err := validateLabel("door game", d.Game, MaxDoorGameLen); err != nil {
		return err
	}
	if len(d.Events) == 0 {
		// 89 bytes of framing that say nothing, and a free way to burn a peer's
		// sequence space.
		return fmt.Errorf("door event body for %q carries no events", d.Game)
	}
	if len(d.Events) > MaxDoorEventsPerRecord {
		return fmt.Errorf("door event body carries %d events, limit is %d",
			len(d.Events), MaxDoorEventsPerRecord)
	}
	for _, e := range d.Events {
		if err := validateDoorEvent(e); err != nil {
			return err
		}
	}
	if n := DoorEventBodyLen(d); n > MaxDoorEventBodyLen {
		return fmt.Errorf("door event body is %d bytes, limit is %d", n, MaxDoorEventBodyLen)
	}
	return nil
}

// MarshalDoorEventBody encodes a DoorEventBody.
//
// Layout: len(game) | game | count | entry*
// entry:  kind | len(actor) | actor | len(target) | target |
//
//	[target_node(8)] | len(payload) | payload
//
// There is no flags byte. An absent target and an absent payload are each a
// single zero length, which costs what a flag bit would have and cannot
// disagree with the field it describes.
//
// # There is not a single varint in here, and that is the point
//
// Every length in this format fits in one byte because every bound is at most
// 48. FILE's first fuzz run found an overlong varint — a size of 0 written as
// 80 00 — giving one logical entry two wire forms and, since a record ID hashes
// the bytes, two content addresses that anti-entropy would carry forever. A
// format with no varints cannot have that bug at all. That is a stronger
// property than defending against it, and it is worth giving up nothing to get,
// because nothing here would have been smaller as a varint anyway.
func MarshalDoorEventBody(d DoorEventBody) ([]byte, error) {
	if err := validateDoorEventBody(d); err != nil {
		return nil, err
	}

	buf := make([]byte, 0, DoorEventBodyLen(d))
	buf = append(buf, byte(len(d.Game)))
	buf = append(buf, d.Game...)
	buf = append(buf, byte(len(d.Events)))
	for _, e := range d.Events {
		buf = append(buf, e.Kind)
		buf = append(buf, byte(len(e.Actor)))
		buf = append(buf, e.Actor...)
		buf = append(buf, byte(len(e.Target)))
		if e.Target != "" {
			buf = append(buf, e.Target...)
			buf = append(buf, e.TargetNode[:]...)
		}
		buf = append(buf, byte(len(e.Payload)))
		buf = append(buf, e.Payload...)
	}
	return buf, nil
}

// UnmarshalDoorEventBody parses a DOOR_EVENT record body.
func UnmarshalDoorEventBody(b []byte) (DoorEventBody, error) {
	var d DoorEventBody
	rest := b

	take := func(n int, what string) ([]byte, error) {
		if len(rest) < n {
			return nil, fmt.Errorf("%w: DOOR_EVENT body ended inside %s", ErrTruncated, what)
		}
		out := rest[:n]
		rest = rest[n:]
		return out, nil
	}

	gameLen, err := take(1, "the game length")
	if err != nil {
		return d, err
	}
	game, err := take(int(gameLen[0]), "the game name")
	if err != nil {
		return DoorEventBody{}, err
	}
	d.Game = string(game)

	count, err := take(1, "the event count")
	if err != nil {
		return DoorEventBody{}, err
	}
	if int(count[0]) > MaxDoorEventsPerRecord {
		// Checked before allocating, so a one-byte claim cannot size a slice.
		return DoorEventBody{}, fmt.Errorf("DOOR_EVENT body claims %d events, limit is %d",
			count[0], MaxDoorEventsPerRecord)
	}
	for i := 0; i < int(count[0]); i++ {
		var e DoorEvent

		kind, err := take(1, "an event kind")
		if err != nil {
			return DoorEventBody{}, err
		}
		e.Kind = kind[0]

		actorLen, err := take(1, "an actor length")
		if err != nil {
			return DoorEventBody{}, err
		}
		actor, err := take(int(actorLen[0]), "an actor")
		if err != nil {
			return DoorEventBody{}, err
		}
		e.Actor = string(actor)

		targetLen, err := take(1, "a target length")
		if err != nil {
			return DoorEventBody{}, err
		}
		if targetLen[0] > 0 {
			target, err := take(int(targetLen[0]), "a target")
			if err != nil {
				return DoorEventBody{}, err
			}
			e.Target = string(target)
			node, err := take(identity.NodeIDLen, "a target node")
			if err != nil {
				return DoorEventBody{}, err
			}
			copy(e.TargetNode[:], node)
		}

		payloadLen, err := take(1, "a payload length")
		if err != nil {
			return DoorEventBody{}, err
		}
		payload, err := take(int(payloadLen[0]), "a payload")
		if err != nil {
			return DoorEventBody{}, err
		}
		if len(payload) > 0 {
			e.Payload = append([]byte(nil), payload...)
		}

		d.Events = append(d.Events, e)
	}

	// Trailing bytes are a second encoding of the same record, and the ID is
	// derived from the bytes rather than the fields — so accepting them would
	// let one logical batch have two content addresses.
	if len(rest) != 0 {
		return DoorEventBody{}, fmt.Errorf("DOOR_EVENT body has %d trailing bytes", len(rest))
	}

	if err := validateDoorEventBody(d); err != nil {
		return DoorEventBody{}, err
	}

	// Require the input to be the canonical encoding of what it parsed to — the
	// same structural defence decodeCanonical applies to the envelope, and the
	// same one UnmarshalFileBody applies here.
	//
	// With no varints in the format there is no known second wire form to catch,
	// which makes this look redundant. It is not: it is the check that keeps a
	// FUTURE field from quietly introducing one, and it costs one allocation on
	// a path that already allocated.
	reencoded, err := MarshalDoorEventBody(d)
	if err != nil {
		return DoorEventBody{}, fmt.Errorf("DOOR_EVENT body did not survive re-encoding: %w", err)
	}
	if !bytes.Equal(reencoded, b) {
		return DoorEventBody{}, fmt.Errorf(
			"non-canonical DOOR_EVENT body: the same batch has another wire form")
	}
	return d, nil
}

// NewDoorEventRecord builds a DOOR_EVENT record signed by the NODE key.
//
// # Why there is no user-signed variant, and never will be
//
// `[N11]` settled this and the reasoning is worth having where someone would
// look for the missing function. Crossing a trust boundary to drive game state
// on someone else's machine sounds like it warrants a user signature. It does
// not, because the threat in an inter-BBS league is a cheating SYSOP, not a
// cheating user, and a user signature provides zero protection against that:
// under tier-2 key custody (§8.2) the sysop's server holds the user's key
// material during a session and can mint a valid user signature at will.
//
// Node-signing is therefore both cheaper and more honest about who is being
// trusted. Leagues rest on sysop-to-sysop trust, exactly as they did in the
// FidoNet era. A league wanting integrity guarantees against its own members
// needs game-level design — commit-reveal, cross-checked state — not a
// signature layer.
func NewDoorEventRecord(key identity.NodeKey, seq uint64, ts uint32, area AreaTag, d DoorEventBody) (*Record, error) {
	body, err := MarshalDoorEventBody(d)
	if err != nil {
		return nil, err
	}
	return New(key, Record{
		Origin: key.ID(),
		Seq:    seq,
		TS:     ts,
		Type:   TypeDoorEvent,
		Area:   area,
		Body:   body,
	})
}

// VerifyDoorEventRecord validates a DOOR_EVENT against its origin node's key.
func VerifyDoorEventRecord(r *Record, originPub []byte) (DoorEventBody, error) {
	if r.Type != TypeDoorEvent {
		return DoorEventBody{}, fmt.Errorf("record %s is a %s, not a DOOR_EVENT", r.ID(), r.Type)
	}
	body, err := UnmarshalDoorEventBody(r.Body)
	if err != nil {
		return DoorEventBody{}, fmt.Errorf("DOOR_EVENT %s: %w", r.ID(), err)
	}
	if err := r.Verify(originPub); err != nil {
		return DoorEventBody{}, fmt.Errorf("DOOR_EVENT %s: %w", r.ID(), err)
	}
	return body, nil
}

// DoorEventSize reports the uncompressed on-wire cost of a batch.
//
// Exported for the same reason FileSize and ProfileSize are: §9.5's claim that
// batching pays is arithmetic about the airtime budget, and a reader should be
// able to check it rather than take it on trust. §12.7 asserts it.
func DoorEventSize(d DoorEventBody) int {
	return 64 + 8 + 8 + 4 + 1 + 4 + DoorEventBodyLen(d)
}

// checkDoorEventBody enforces the type-level bound wherever a record enters.
//
// Same door, same reasoning as checkNoFileContent: the generic path allows a
// body up to MaxBodyLen, which is 8 KiB, and without this a DOOR_EVENT could be
// minted or accepted carrying 8 KiB of anything at all — making the payload
// bound above a suggestion rather than a rule, and MaxDoorEventPayloadLen's
// arithmetic about file content false.
//
// A DOOR_EVENT body must therefore parse as a batch, checked where one is built
// and where one is decoded from a peer.
func checkDoorEventBody(r *Record) error {
	if r.Type != TypeDoorEvent {
		return nil
	}
	if _, err := UnmarshalDoorEventBody(r.Body); err != nil {
		return fmt.Errorf("DOOR_EVENT record body is not a batch of events: %w", err)
	}
	return nil
}
