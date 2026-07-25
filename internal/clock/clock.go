// Package clock provides the injected time source required by design §12.1.
//
// Domain code must never call time.Now(). Deterministic simulation (§12.1)
// depends on every read of the current time flowing through a Clock that the
// test harness can drive, and a single stray time.Now() makes a test flaky
// forever. The lint check in tools/checkdeterminism enforces this.
//
// Note also §6.2.1: wall-clock time is *advisory* in this system. An off-grid
// node may have no RTC and no NTP. Causal ordering comes from (origin, seq)
// and parent pointers, never from timestamps.
package clock

import (
	"sync"
	"time"
)

// Clock is the interface all domain code uses to read time and to wait.
type Clock interface {
	// Now returns the current time.
	Now() time.Time

	// Since is shorthand for Now().Sub(t).
	Since(t time.Time) time.Duration

	// After returns a channel that receives once d has elapsed. Under a
	// Virtual clock this fires when time is advanced, not when the host's
	// wall clock moves.
	After(d time.Duration) <-chan time.Time

	// Sleep blocks until d has elapsed.
	Sleep(d time.Duration)
}

// Real is the production Clock, backed by the host clock.
type Real struct{}

// NewReal returns a Clock backed by the host's wall clock.
func NewReal() Clock { return Real{} }

func (Real) Now() time.Time                         { return time.Now() }
func (Real) Since(t time.Time) time.Duration        { return time.Since(t) }
func (Real) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (Real) Sleep(d time.Duration)                  { time.Sleep(d) }

// Virtual is a deterministic Clock whose time only moves when Advance or Set
// is called. It is the clock the simulation harness (§12.1) runs on, and it is
// what makes a 30-day soak or a 3-hour digest interval testable at all.
//
// Virtual is safe for concurrent use.
type Virtual struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// NewVirtual returns a Virtual clock started at t.
func NewVirtual(t time.Time) *Virtual { return &Virtual{now: t} }

func (v *Virtual) Now() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.now
}

func (v *Virtual) Since(t time.Time) time.Duration { return v.Now().Sub(t) }

func (v *Virtual) After(d time.Duration) <-chan time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	ch := make(chan time.Time, 1)
	deadline := v.now.Add(d)
	if !deadline.After(v.now) {
		ch <- v.now
		return ch
	}
	v.waiters = append(v.waiters, waiter{at: deadline, ch: ch})
	return ch
}

// Sleep on a Virtual clock blocks until another goroutine advances time past
// the deadline. Simulation code drives time from a single goroutine, so this
// is only useful for tests that deliberately exercise blocking.
func (v *Virtual) Sleep(d time.Duration) { <-v.After(d) }

// Advance moves the clock forward by d and fires any waiters whose deadline
// has passed. Waiters fire in deadline order, and ties break by registration
// order, so the sequence of wakeups is fully determined by the inputs.
func (v *Virtual) Advance(d time.Duration) { v.Set(v.Now().Add(d)) }

// Set moves the clock to t. Moving time backwards is allowed — §6.2.1 requires
// tolerating nodes whose clock jumps — but it fires no waiters.
func (v *Virtual) Set(t time.Time) {
	v.mu.Lock()
	v.now = t
	// Partition waiters into fired and pending, preserving registration order
	// within each. Sorting by deadline keeps wakeup order deterministic.
	var fired []waiter
	pending := v.waiters[:0]
	for _, w := range v.waiters {
		if !w.at.After(t) {
			fired = append(fired, w)
		} else {
			pending = append(pending, w)
		}
	}
	v.waiters = pending
	v.mu.Unlock()

	// Stable insertion sort by deadline: n is tiny and this avoids pulling in
	// sort just to keep tie-breaking explicit.
	for i := 1; i < len(fired); i++ {
		for j := i; j > 0 && fired[j].at.Before(fired[j-1].at); j-- {
			fired[j], fired[j-1] = fired[j-1], fired[j]
		}
	}
	for _, w := range fired {
		w.ch <- t
	}
}
