package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Session time limits (§11.5, users.session_time_limit).
//
// # Why this is in the model rather than in each front end
//
// There are three ways into this BBS and one state machine behind them
// (webui.md §2). A limit implemented in the SSH listener would not apply over
// telnet; one implemented in each would be three countdowns to keep in step,
// and the first person to add a fourth front end would inherit none of them.
// The model already knows who is connected, holds the injected clock, and owns
// the only path out of a session, so it is the one place where "your time is
// up" can be both said and acted on.
//
// It is also what makes a door's time_remaining honest: §9.1 promises a door
// the time left in the session, and until there was a session clock the only
// truthful answer was the door's own wall-clock limit.
//
// # Why sysops are exempt
//
// Not in §11.5, and a deliberate addition. The limit exists to share a scarce
// resource — lines — between callers, and the sysop is not competing for it;
// they are the person who will be logged in at 3am fixing whatever went wrong.
// A board that disconnects its own operator mid-repair has turned a courtesy
// into a hazard. This is stated here rather than made configurable because a
// sysop who wants to be timed out can set their own limit by other means, and
// nobody has ever wanted that.

// timeWarnings are the marks at which a user is told, longest first. Two is
// enough: one to finish reading, one to finish typing.
var timeWarnings = []time.Duration{5 * time.Minute, time.Minute}

// timeCheckMsg asks the model to look at the clock.
type timeCheckMsg struct{}

// timeLimited reports whether this session is on the clock.
func (m Model) timeLimited() bool {
	return m.cfg.SessionLimit > 0 && !m.sysop && m.cfg.Clock != nil
}

// Remaining returns how long this session has left, and whether it is limited
// at all.
//
// The second return is not a convenience. A door handed a bare zero cannot tell
// "no limit" from "no time left", and those call for opposite behaviour: one
// means play on, the other means save and quit now.
func (m Model) Remaining() (time.Duration, bool) {
	if !m.timeLimited() {
		return 0, false
	}
	left := m.cfg.SessionLimit - m.clockNow().Sub(m.startedAt)
	return max(left, 0), true
}

// watchTimeLimit sleeps until the next thing worth saying, then asks for a
// check.
//
// It waits exactly as long as it needs to rather than polling on a fixed
// interval, which costs one wake-up per warning and one at the cut instead of
// one every few seconds for every session on the board. It also means the cut
// lands on time rather than up to an interval late.
//
// The clock is the injected one, as everywhere (§12.1). Under the simulator's
// Virtual clock this fires when time is advanced, which is what makes a
// two-hour limit testable in a millisecond.
func (m Model) watchTimeLimit() tea.Cmd {
	left, ok := m.Remaining()
	if !ok || m.cfg.DisableWatchers {
		return nil
	}
	wait := m.untilNextTimeEvent(left)
	clk, ctx := m.cfg.Clock, m.ctx
	return func() tea.Msg {
		select {
		case <-clk.After(wait):
			return timeCheckMsg{}
		case <-ctx.Done():
			// The session ended first. Returning nil rather than a message
			// keeps a dead session from waking the pump.
			return nil
		}
	}
}

// untilNextTimeEvent is how long until the next warning, or the cut.
func (m Model) untilNextTimeEvent(left time.Duration) time.Duration {
	for _, at := range timeWarnings {
		// Only marks not yet given, and only ones still ahead of us.
		if m.timeWarned != 0 && at >= m.timeWarned {
			continue
		}
		if left > at {
			return left - at
		}
	}
	return left
}

// enforceTimeLimit warns, or ends the session.
func (m Model) enforceTimeLimit() (tea.Model, tea.Cmd) {
	left, ok := m.Remaining()
	if !ok {
		return m, nil
	}
	if left <= 0 {
		return m.leaveBecause(fmt.Sprintf(
			"Your %s on this call is up. Thanks for calling.",
			humanDuration(m.cfg.SessionLimit)))
	}

	for _, at := range timeWarnings {
		if m.timeWarned != 0 && at >= m.timeWarned {
			continue
		}
		if left <= at {
			m.timeWarned = at
			m.status = fmt.Sprintf("%s left on this call.", humanDuration(left))
			m.statusErr = false
			break
		}
	}
	return m, m.watchTimeLimit()
}

// humanDuration renders a duration the way someone would say it out loud.
// A user told "4m59.83s remaining" has been given a number, not a warning.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		secs := int(d.Round(time.Second).Seconds())
		if secs <= 1 {
			return "1 second"
		}
		return fmt.Sprintf("%d seconds", secs)
	}
	mins := int(d.Round(time.Minute).Minutes())
	if mins == 1 {
		return "1 minute"
	}
	if mins < 60 {
		return fmt.Sprintf("%d minutes", mins)
	}
	hours := mins / 60
	rem := mins % 60
	switch {
	case hours == 1 && rem == 0:
		return "1 hour"
	case rem == 0:
		return fmt.Sprintf("%d hours", hours)
	case hours == 1:
		return fmt.Sprintf("1 hour %d minutes", rem)
	default:
		return fmt.Sprintf("%d hours %d minutes", hours, rem)
	}
}
