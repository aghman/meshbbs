//go:build windows

package door

import (
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformName is what a refusal calls this operating system.
const platformName = "windows"

// supportedLimits is what this platform can enforce.
//
// Both, and more cleanly than Unix: a job object carries them, they apply to
// every process in the job rather than only the one we launched, and there is
// no signal for a door to ignore — the kernel terminates it.
var supportedLimits = []string{LimitCPU, LimitMemory}

// HelperArg and IsHelper exist on both platforms so that callers do not need a
// build tag to ask. Windows never uses the helper: the job object applies
// limits directly, so there is nothing to re-execute for.
const HelperArg = "--meshbbs-door-exec"

// IsHelper is always false here.
func IsHelper([]string) bool { return false }

// RunHelper is never reached on Windows.
func RunHelper([]string) error {
	return fmt.Errorf("%s is not used on windows", HelperArg)
}

// limitCommand is the identity here: limits go on the job, not the command.
func limitCommand(_ string, spec Spec) (string, []string) {
	return spec.Path, spec.Args
}

// applyJobLimits puts the CPU and memory limits on a door's job object.
//
// Extends the same JOBOBJECT_EXTENDED_LIMIT_INFORMATION that already carries
// kill-on-close, because the flags share one field and setting it twice would
// mean the second call silently dropped the first one's flags.
func applyJobLimits(job windows.Handle, cpu time.Duration, mem int64) error {
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if cpu > 0 {
		// Job object CPU limits are in 100-nanosecond units, and count USER
		// time only — a door spinning in the kernel is the wall-clock limit's
		// problem, as it is on Unix.
		info.BasicLimitInformation.PerProcessUserTimeLimit = int64(cpu / 100)
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_TIME
	}
	if mem > 0 {
		info.ProcessMemoryLimit = uintptr(mem)
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
	}

	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fmt.Errorf("apply door limits: %w", err)
	}
	return nil
}
