//go:build !windows

package door

import (
	"errors"
	"syscall"
)

// processGone reports whether a pid no longer names a live process. Signal 0
// performs the permission and existence checks without delivering anything.
func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
