package main

// ttyPath is the console input handle.
//
// CONIN$ is the Windows equivalent of /dev/tty: it opens the console this
// process is attached to, whatever stdin was redirected to. Without it, piping
// a message in would leave nowhere to read a passphrase from.
const ttyPath = "CONIN$"
