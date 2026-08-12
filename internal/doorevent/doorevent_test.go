package doorevent

import (
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/record"
)

func at(base time.Time, d time.Duration) Event { return Event{QueuedAt: base.Add(d)} }

func numbered(base time.Time, offsets ...time.Duration) []Event {
	var out []Event
	for i, d := range offsets {
		e := at(base, d)
		e.ID = int64(i + 1)
		out = append(out, e)
	}
	return out
}

// A full batch goes immediately: at BatchMax the record is already carrying its
// signature efficiently and waiting buys nothing.
func TestAFullBatchDoesNotWaitOutTheWindow(t *testing.T) {
	start := time.Unix(1_765_000_000, 0)
	clk := clock.NewVirtual(start)
	p := New(Config{Window: time.Hour, Clock: clk})

	var offsets []time.Duration
	for i := 0; i < record.MaxDoorEventsPerRecord; i++ {
		offsets = append(offsets, 0)
	}
	d := p.Consider(Group{Area: "league", Game: "lord", Events: numbered(start, offsets...)})

	if len(d.Flush) != record.MaxDoorEventsPerRecord {
		t.Fatalf("flushed %d, want a full batch of %d", len(d.Flush), record.MaxDoorEventsPerRecord)
	}
	if d.Wait != 0 {
		t.Errorf("a full batch reported a wait of %s", d.Wait)
	}
}

// A partial batch waits, and the wait is measured from the OLDEST event.
//
// From the newest, a door reporting something every few minutes would hold its
// first event forever — the batch would never age out because the clock kept
// being reset by new arrivals.
func TestAPartialBatchWaitsFromTheOldestEvent(t *testing.T) {
	start := time.Unix(1_765_000_000, 0)
	clk := clock.NewVirtual(start)
	p := New(Config{Window: time.Hour, Clock: clk})

	// One event 50 minutes old, one that just arrived.
	events := numbered(start, -50*time.Minute, 0)

	d := p.Consider(Group{Events: events})
	if len(d.Flush) != 0 {
		t.Fatalf("flushed early: %+v", d.Flush)
	}
	if d.Wait != 10*time.Minute {
		t.Errorf("wait = %s, want 10m (measured from the oldest)", d.Wait)
	}

	// Ten minutes later the oldest has aged out and the whole partial batch
	// goes, including the newcomer.
	clk.Advance(10 * time.Minute)
	d = p.Consider(Group{Events: events})
	if len(d.Flush) != 2 {
		t.Fatalf("flushed %d, want both", len(d.Flush))
	}
}

// Expiry runs before batching, or a week-old backlog looks like a full batch
// and puts stale news on the air at full price.
func TestExpiryIsAppliedBeforeBatching(t *testing.T) {
	start := time.Unix(1_765_000_000, 0)
	clk := clock.NewVirtual(start)
	p := New(Config{Window: time.Hour, MaxAge: 24 * time.Hour, Clock: clk})

	// A full batch's worth, all older than the max age, plus one fresh event.
	var offsets []time.Duration
	for i := 0; i < record.MaxDoorEventsPerRecord; i++ {
		offsets = append(offsets, -48*time.Hour)
	}
	offsets = append(offsets, -time.Minute)

	d := p.Consider(Group{Events: numbered(start, offsets...)})
	if len(d.Expire) != record.MaxDoorEventsPerRecord {
		t.Errorf("expired %d, want %d", len(d.Expire), record.MaxDoorEventsPerRecord)
	}
	if len(d.Flush) != 0 {
		t.Errorf("a batch made entirely of expired events was flushed: %d", len(d.Flush))
	}
	if d.Wait == 0 {
		t.Error("the surviving fresh event was not scheduled to wait")
	}
}

// More than a record can hold flushes exactly one record's worth, oldest first,
// and leaves the rest for the next pass.
func TestOverfullGroupsFlushOneRecordAtATime(t *testing.T) {
	start := time.Unix(1_765_000_000, 0)
	clk := clock.NewVirtual(start)
	p := New(Config{Window: time.Hour, Clock: clk})

	var offsets []time.Duration
	for i := 0; i < record.MaxDoorEventsPerRecord*2+3; i++ {
		offsets = append(offsets, 0)
	}
	d := p.Consider(Group{Events: numbered(start, offsets...)})

	if len(d.Flush) != record.MaxDoorEventsPerRecord {
		t.Fatalf("flushed %d, want exactly one record's worth", len(d.Flush))
	}
	// Oldest first: ids were assigned in order, so the batch must start at 1.
	if d.Flush[0].ID != 1 {
		t.Errorf("batch starts at id %d, want the oldest", d.Flush[0].ID)
	}
}

// The window is derived from what an area's share actually buys, so a measured
// R propagates without anyone editing a config file.
func TestDeriveWindowTracksTheBudget(t *testing.T) {
	// The design's own arithmetic: at R=4 and 50 instances a node originates
	// about 10.8 full packets a day, and a league on a tenth of that gets
	// about one. One packet a day is a day-long window — clamped to the
	// ceiling, because past that events expire before they are sent.
	if got := DeriveWindow(1); got != MaxWindow {
		t.Errorf("one packet a day derived %s, want the %s ceiling", got, MaxWindow)
	}

	// Eight a day is three hours.
	if got := DeriveWindow(8); got != 3*time.Hour {
		t.Errorf("eight packets a day derived %s, want 3h", got)
	}

	// A generous share on a quiet mesh floors rather than going to seconds:
	// below the floor batching stops paying for itself.
	if got := DeriveWindow(10000); got != MinWindow {
		t.Errorf("a large budget derived %s, want the %s floor", got, MinWindow)
	}

	// A share that buys nothing does not wait forever; the age rule is where
	// giving up should be visible.
	if got := DeriveWindow(0); got != MaxWindow {
		t.Errorf("no budget derived %s, want the ceiling", got)
	}

	// Halving the budget — which is what R going from 4 to 8 does — doubles
	// the window, with no other change anywhere.
	if DeriveWindow(4) != 2*DeriveWindow(8) {
		t.Errorf("halving the budget did not double the window: %s vs %s",
			DeriveWindow(4), DeriveWindow(8))
	}
}

// An empty group is not a crash and not a flush.
func TestAnEmptyGroupDecidesNothing(t *testing.T) {
	p := New(Config{Clock: clock.NewVirtual(time.Unix(1, 0))})
	d := p.Consider(Group{Area: "league", Game: "lord"})
	if len(d.Flush) != 0 || len(d.Expire) != 0 || d.Wait != 0 {
		t.Errorf("an empty group produced %+v", d)
	}
}
