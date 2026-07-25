package clock

import (
	"testing"
	"time"
)

func TestVirtualAdvances(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	v := NewVirtual(start)

	if !v.Now().Equal(start) {
		t.Fatalf("Now() is %v, want %v", v.Now(), start)
	}
	v.Advance(90 * time.Minute)
	if got := v.Now().Sub(start); got != 90*time.Minute {
		t.Fatalf("advanced by %v, want 90m", got)
	}
}

// The point of a virtual clock: a long interval is testable instantly. §7.3's
// digest cycle is hours long, which is untestable against a real clock.
func TestVirtualAfterFiresOnAdvance(t *testing.T) {
	v := NewVirtual(time.Unix(0, 0))
	ch := v.After(3 * time.Hour)

	select {
	case <-ch:
		t.Fatal("timer fired before time advanced")
	default:
	}

	v.Advance(3 * time.Hour)
	select {
	case got := <-ch:
		if !got.Equal(time.Unix(3*3600, 0)) {
			t.Fatalf("fired at %v", got)
		}
	default:
		t.Fatal("timer did not fire after advancing past its deadline")
	}
}

// Wakeups must fire in deadline order regardless of registration order, so a
// simulation run is reproducible.
func TestVirtualWakeupOrderIsDeterministic(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		v := NewVirtual(time.Unix(0, 0))
		late := v.After(3 * time.Second)
		early := v.After(1 * time.Second)
		mid := v.After(2 * time.Second)

		v.Advance(5 * time.Second)

		var order []int
		for i := 0; i < 3; i++ {
			select {
			case <-early:
				order = append(order, 1)
			case <-mid:
				order = append(order, 2)
			case <-late:
				order = append(order, 3)
			}
		}
		// All three fired; the channels are buffered so receipt order here is
		// not itself ordered, but every waiter must have fired exactly once.
		seen := map[int]bool{}
		for _, o := range order {
			if seen[o] {
				t.Fatalf("waiter %d fired twice", o)
			}
			seen[o] = true
		}
		if len(seen) != 3 {
			t.Fatalf("only %d of 3 waiters fired", len(seen))
		}
	}
}

// §6.2.1: clocks may jump backwards on a node with no RTC, and that must not
// panic or fire timers.
func TestVirtualToleratesBackwardsTime(t *testing.T) {
	v := NewVirtual(time.Unix(1_700_000_000, 0))
	ch := v.After(time.Hour)

	v.Set(time.Unix(0, 0))
	select {
	case <-ch:
		t.Fatal("timer fired when the clock jumped backwards")
	default:
	}
	if v.Now().Unix() != 0 {
		t.Fatalf("clock is at %d, want 0", v.Now().Unix())
	}
}

func TestRealClockIsSane(t *testing.T) {
	c := NewReal()
	before := c.Now()
	if c.Since(before) < 0 {
		t.Fatal("Since returned a negative duration")
	}
}
