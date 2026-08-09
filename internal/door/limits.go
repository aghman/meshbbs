package door

import (
	"fmt"
	"strings"
	"time"
)

// CPU and memory limits (§9.4).
//
// # Why this needs a helper at all
//
// A resource limit has to be applied to the child, and on Unix the only place
// to do that is between fork and exec — a window Go does not expose without
// cgo, which §4 rules out. So the server re-executes ITSELF with a sentinel
// argument, that copy applies the limits to itself, and then it execs the door.
// syscall.Exec replaces the process image rather than spawning under it, so
// there is no wrapper process in the tree: the pid stays the same, the process
// group stays the same, and the kill path from §9.4 is untouched.
//
// Windows needs none of this. The job object the runner already creates for
// kill-on-close carries memory and CPU limits directly, and applies them to
// every process in the job.
//
// # What each platform can actually enforce
//
// Measured, not assumed, because the answers are not the ones the manual pages
// suggest:
//
//   - CPU works on Linux, macOS and Windows. On Unix it is RLIMIT_CPU, and the
//     kernel signals SIGXCPU at the soft limit.
//   - Memory works on Linux (RLIMIT_AS) and Windows (job object commit limit).
//     On macOS it does NOT: setting RLIMIT_AS or RLIMIT_DATA returns EINVAL,
//     and a child asked to allocate 256 MiB against a 64 MiB limit allocated it
//     without complaint.
//
// A limit this platform cannot apply is refused when the door launches, with
// the platform named. Silently ignoring it is the one option not available: a
// sysop who set a memory limit believes there is one.
//
// # The caveat that applies everywhere
//
// A door written in GO ignores RLIMIT_CPU. The Go runtime treats SIGXCPU as a
// signal to deliver to a listener and to ignore when there is none, so a Go
// door spins straight through its limit — measured at ten times over, on a
// limit the same test enforced against awk in one second.
//
// This is worth knowing and is not worth panicking about, because the CPU limit
// is not the safety net. WallClock is required (see Spec), applies to every
// door whatever it is written in, and kills the runaway case — a door burning
// CPU is also burning wall-clock time. The CPU limit is a refinement for a door
// that should be idle and is not.

// Limit names, as they appear in configuration and in refusals.
const (
	LimitCPU    = "cpu_limit"
	LimitMemory = "mem_limit"
)

// Supported reports whether this platform can enforce a limit.
func Supported(limit string) bool {
	for _, s := range supportedLimits {
		if s == limit {
			return true
		}
	}
	return false
}

// UnsupportedLimits names the limits in a Spec that this platform cannot apply.
//
// Exported so that the surfaces a sysop touches — the door CLI, the launcher —
// can say so in their own words at the moment it matters, rather than this
// package guessing where the message should go.
func UnsupportedLimits(cpu time.Duration, mem int64) []string {
	var out []string
	if cpu > 0 && !Supported(LimitCPU) {
		out = append(out, LimitCPU)
	}
	if mem > 0 && !Supported(LimitMemory) {
		out = append(out, LimitMemory)
	}
	return out
}

// checkLimits refuses a Spec whose limits this platform cannot apply.
func checkLimits(spec Spec) error {
	if bad := UnsupportedLimits(spec.CPULimit, spec.MemLimit); len(bad) > 0 {
		return fmt.Errorf(
			"door %s sets %s, which %s cannot enforce; clear it so that a limit "+
				"nobody applies is not mistaken for protection",
			spec.Name, strings.Join(bad, " and "), platformName)
	}
	return nil
}
