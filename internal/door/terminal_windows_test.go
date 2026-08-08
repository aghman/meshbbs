//go:build windows

package door

// platformHelper adds no door modes on Windows. Reading the window size from
// inside the door needs an ioctl on Unix and a console API here, and the resize
// path is covered by the Unix test plus the ConPTY call itself.
func platformHelper(_, _ string) bool { return false }
