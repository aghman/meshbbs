package bbs

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

// The whole path a league event takes locally: a door reports it, it waits in
// the queue, and a batch becomes one signed record on the area.
//
// This is the test that makes "queued" stop being a promise and start being a
// description.
func TestQueuedEventsBecomeOneSignedRecord(t *testing.T) {
	svc, st, ctx := testService(t)
	h := svc.Doors()
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)

	if _, err := st.CreateDoorArea(ctx, "lordleague", "LORD", true); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		err := h.QueueDoorEvent(ctx, door.DoorEventRequest{
			Door: "lord", Area: "lordleague", Game: "lord",
			Kind: uint8(i), Actor: "alice", Payload: []byte{byte(i)},
		})
		if err != nil {
			t.Fatalf("queue %d: %v", i, err)
		}
	}

	queued, err := st.QueuedDoorEvents(ctx, "lordleague", "lord")
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 3 {
		t.Fatalf("queued %d events", len(queued))
	}

	id, err := svc.PublishDoorEvents(ctx, "lordleague", "lord", queued)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// ONE record for three events. That ratio is the whole reason batching
	// exists: three separate records would have cost three signatures.
	if len(pub.recs) != 1 {
		t.Fatalf("published %d records for one batch", len(pub.recs))
	}
	rec := pub.recs[0]
	if rec.Type != record.TypeDoorEvent {
		t.Errorf("published a %s", rec.Type)
	}
	if rec.ID() != id {
		t.Errorf("published %s but returned %s", rec.ID(), id)
	}

	body, err := record.VerifyDoorEventRecord(rec, svc.key.Public)
	if err != nil {
		t.Fatalf("the published record does not verify: %v", err)
	}
	if body.Game != "lord" || len(body.Events) != 3 {
		t.Fatalf("body is %+v", body)
	}
	for i, ev := range body.Events {
		if ev.Actor != "alice" || ev.Kind != uint8(i) {
			t.Errorf("event %d is %+v", i, ev)
		}
	}

	// And it landed in the log, on the league's own area.
	if got := rec.Area; got != record.AreaTagFor("lordleague") {
		t.Errorf("record went to area %x", got[:])
	}
}

// A league that stopped federating while events were queued must not produce a
// record nobody will ever be offered.
func TestPublishRefusesAnUnfederatedLeague(t *testing.T) {
	svc, st, ctx := testService(t)
	h := svc.Doors()

	if _, err := st.CreateDoorArea(ctx, "lordleague", "", true); err != nil {
		t.Fatal(err)
	}
	if err := h.QueueDoorEvent(ctx, door.DoorEventRequest{
		Door: "lord", Area: "lordleague", Game: "lord", Actor: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := st.QueuedDoorEvents(ctx, "lordleague", "lord")
	if err != nil {
		t.Fatal(err)
	}

	// The sysop changes their mind after the door has already been told
	// "queued".
	if err := st.SetAreaFederated(ctx, "lordleague", false, "sysop"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishDoorEvents(ctx, "lordleague", "lord", queued); err == nil {
		t.Error("published to a league that no longer federates")
	} else if !strings.Contains(err.Error(), "no longer federated") {
		t.Errorf("refusal does not say why: %v", err)
	}
}

// Publishing an empty batch is a caller bug, not an empty record: a DOOR_EVENT
// with no events is 89 bytes of framing that say nothing, and the codec refuses
// it anyway.
func TestPublishRefusesAnEmptyBatch(t *testing.T) {
	svc, st, ctx := testService(t)
	if _, err := st.CreateDoorArea(ctx, "lordleague", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishDoorEvents(ctx, "lordleague", "lord", nil); err == nil {
		t.Error("published an empty batch")
	}
}

// Deleting by id removes exactly what was sent, leaving anything that arrived
// while the batch was being published.
func TestDequeueRemovesOnlyWhatWasSent(t *testing.T) {
	_, st, ctx := testService(t)

	for i := 0; i < 5; i++ {
		if err := st.QueueDoorEvent(ctx, store.QueuedDoorEvent{
			Door: "lord", Area: "lordleague", Game: "lord", Actor: "alice",
		}); err != nil {
			t.Fatal(err)
		}
	}
	queued, err := st.QueuedDoorEvents(ctx, "lordleague", "lord")
	if err != nil {
		t.Fatal(err)
	}

	sent := []int64{queued[0].ID, queued[2].ID}
	if err := st.DeleteQueuedDoorEvents(ctx, sent); err != nil {
		t.Fatal(err)
	}

	left, err := st.QueuedDoorEvents(ctx, "lordleague", "lord")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 {
		t.Fatalf("%d events left, want 3", len(left))
	}
	for _, ev := range left {
		for _, gone := range sent {
			if ev.ID == gone {
				t.Errorf("event %d was sent and is still queued", ev.ID)
			}
		}
	}
}

// The full local round trip through the host: a door reports, the batch is
// published, and a poll reads it back with node IDs rendered as strings.
func TestADoorCanReadBackWhatItReported(t *testing.T) {
	svc, st, ctx := testService(t)
	h := svc.Doors()
	if _, err := st.CreateDoorArea(ctx, "lordleague", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateUser(ctx, store.CreateUserOptions{
		Nick: "bob", DisplayName: "Bob", CanLogin: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.QueueDoorEvent(ctx, door.DoorEventRequest{
		Door: "lord", Area: "lordleague", Game: "lord", Kind: 3,
		Actor: "alice", Target: "bob", Payload: []byte{9, 9},
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := st.QueuedDoorEvents(ctx, "lordleague", "lord")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PublishDoorEvents(ctx, "lordleague", "lord", queued); err != nil {
		t.Fatal(err)
	}

	batch, err := h.PollDoorEvents(ctx, door.DoorEventPoll{
		Door: "lord", Area: "lordleague", Game: "lord",
	})
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("poll returned %d events", len(batch.Events))
	}
	ev := batch.Events[0]
	if ev.Actor != "alice" || ev.Target != "bob" || ev.Kind != 3 {
		t.Errorf("polled %+v", ev)
	}
	if ev.Origin == "" {
		t.Error("the event carries no origin, so a door cannot tell which board it came from")
	}
	if ev.TargetNode == "" {
		t.Error("a targeted event carries no target node")
	}
	if ev.Payload == "" {
		t.Error("the payload did not survive the round trip")
	}
	if batch.Cursor == 0 {
		t.Error("poll returned no cursor to continue from")
	}
	if batch.Truncated {
		t.Error("a complete league was reported as truncated")
	}

	// Polling again from the cursor returns nothing rather than repeating.
	next, err := h.PollDoorEvents(ctx, door.DoorEventPoll{
		Door: "lord", Area: "lordleague", Game: "lord", After: batch.Cursor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Events) != 0 {
		t.Errorf("polling at the cursor repeated %d events", len(next.Events))
	}
}

// A forum is not a league in either direction.
func TestPollRefusesAnAreaThatIsNotALeague(t *testing.T) {
	svc, st, ctx := testService(t)
	h := svc.Doors()
	if _, err := st.CreateArea(ctx, "chatter", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PollDoorEvents(ctx, door.DoorEventPoll{Door: "lord", Area: "chatter"}); !errors.Is(err, door.ErrNotALeague) {
		t.Errorf("got %v, want ErrNotALeague", err)
	}
}
