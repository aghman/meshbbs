//go:build windows

package door

import "path/filepath"

// filepathBase is filepath.Base, named separately so the pipe name's
// construction reads as one thing.
func filepathBase(dir string) string { return filepath.Base(dir) }
