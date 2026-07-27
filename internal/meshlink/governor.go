package meshlink

import (
	"time"

	"github.com/aghman/meshbbs/internal/link"
)

// Governor decides whether the radio may transmit (§7.6).
//
// It is an interface here, and implemented in the next part of Phase 3, because
// the two halves have genuinely different jobs: this package knows how to put a
// packet on the air, and the governor knows what putting it there costs the
// commons. Splitting them also means the airtime model can be tested against
// the simulator's controllable mesh rather than against a radio.
//
// A nil Governor refuses every send. That is deliberate — the failure mode of
// an accidentally permissive default is a BBS that transmits without a budget
// on a shared band, which §1.1 calls the one thing the design exists to
// prevent. Being unable to send is a bug someone fixes in a minute; being a bad
// neighbour is a bug nobody local can fix at all.
type Governor interface {
	// Charge asks to spend the airtime for one packet carrying n payload
	// bytes, and reports whether the budget allows it. An implementation that
	// returns true has already charged for the packet.
	Charge(n int) bool
	// Budget reports the current allowance, for callers deciding whether to
	// start work at all.
	Budget() link.Budget
}

// Unmetered is a Governor that permits everything.
//
// It exists for tests, for bench setups on a dummy load, and for bringing up
// new hardware where the point is to see any packet move at all. It is NOT the
// production default and nothing wires it in automatically: a link built
// without a governor refuses to send rather than quietly getting this one.
type Unmetered struct{}

func (Unmetered) Charge(int) bool { return true }

func (Unmetered) Budget() link.Budget {
	return link.Budget{Available: time.Hour, PerDatagram: 0}
}
