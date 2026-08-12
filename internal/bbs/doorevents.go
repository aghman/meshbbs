package bbs

import (
	"context"
	"fmt"

	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

// PublishDoorEvents mints one DOOR_EVENT record from a batch and puts it on the
// league (§9.5).
//
// # Why this lives here and not in the flusher
//
// It is the same shape as AddFile's tail: mint with the node key, store, then
// hand to the publisher. Keeping it beside the other record-minting paths means
// the sequence allocation, the signing key and the publish call have one home
// rather than three, and a flusher in another package cannot get the ordering
// wrong.
//
// The batch is the caller's decision. This does not re-derive it, does not
// re-check ages, and does not look at the queue — it turns exactly these events
// into exactly one record, which is what makes the caller's snapshot and what
// goes on the air the same set.
func (s *Service) PublishDoorEvents(ctx context.Context, areaName, game string, events []store.QueuedDoorEvent) (record.ID, error) {
	if len(events) == 0 {
		return record.ID{}, fmt.Errorf("no events to publish")
	}
	area, err := s.store.GetDoorArea(ctx, areaName)
	if err != nil {
		return record.ID{}, err
	}
	if !area.Federated {
		// The grant check at emit time said this was federated. If it is not
		// now, a sysop un-federated the league while events were queued, and
		// the honest thing is to refuse rather than to write a record nobody
		// will ever be offered.
		return record.ID{}, fmt.Errorf("%s is no longer federated", area.Name)
	}

	body := record.DoorEventBody{Game: game}
	for _, ev := range events {
		body.Events = append(body.Events, record.DoorEvent{
			Kind:       ev.Kind,
			Actor:      ev.Actor,
			Target:     ev.Target,
			TargetNode: ev.TargetNode,
			Payload:    ev.Payload,
		})
	}

	seq, err := s.store.NextSeq(ctx, area.Tag)
	if err != nil {
		return record.ID{}, err
	}
	rec, err := record.NewDoorEventRecord(s.key, seq, uint32(s.clock.Now().Unix()), area.Tag, body)
	if err != nil {
		return record.ID{}, err
	}
	if err := s.store.PutRecord(ctx, rec); err != nil {
		return record.ID{}, err
	}
	s.publish(area.Tag, rec)
	return rec.ID(), nil
}
