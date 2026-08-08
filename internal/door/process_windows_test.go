//go:build windows

package door

import "golang.org/x/sys/windows"

// stillActive is Windows' STILL_ACTIVE, the exit code a running process
// reports.
const stillActive = 259

// processGone reports whether a pid no longer names a live process.
func processGone(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code != stillActive
}
