package door

import "runtime"

// isWindows lets a test relax a permission assertion that has no meaning
// there, without a build-tagged file per assertion.
func isWindows() bool { return runtime.GOOS == "windows" }
