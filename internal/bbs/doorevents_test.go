package bbs

import (
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
