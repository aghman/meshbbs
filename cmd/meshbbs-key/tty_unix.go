//go:build !windows

package main

// ttyPath is the controlling terminal.
//
// /dev/tty rather than /dev/stdin: the whole reason this exists is that stdin
// is carrying a message, so the passphrase has to come from the terminal the
// process is attached to regardless of what stdin was redirected to.
const ttyPath = "/dev/tty"
