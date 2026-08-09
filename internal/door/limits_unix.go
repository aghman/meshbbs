//go:build !windows

package door

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// platformName is what a refusal calls this operating system.
var platformName = runtime.GOOS

// supportedLimits is what this platform can actually enforce.
//
// macOS is missing memory deliberately and from measurement: setting RLIMIT_AS
// or RLIMIT_DATA there returns EINVAL, and a child asked for 256 MiB against a
// 64 MiB limit gets it. Listing memory here on darwin would make every refusal
// in this package a lie.
var supportedLimits = func() []string {
	if runtime.GOOS == "darwin" {
		return []string{LimitCPU}
	}
	return []string{LimitCPU, LimitMemory}
}()

// HelperArg is the argv[1] that turns this binary into the limit helper.
//
// A sentinel rather than a subcommand, so that it is checked before any command
// parsing and cannot be reached by a user typing something plausible.
const HelperArg = "--meshbbs-door-exec"

// IsHelper reports whether this process was started to apply limits and exec a
// door.
func IsHelper(args []string) bool {
	return len(args) > 1 && args[1] == HelperArg
}

// RunHelper applies the limits named in argv and becomes the door. It returns
// only on failure.
//
// Layout: <self> --meshbbs-door-exec <cpuSeconds> <memBytes> <path> [args...]
func RunHelper(args []string) error {
	if len(args) < 5 {
		return fmt.Errorf("%s needs cpu, memory and a program", HelperArg)
	}
	cpuSecs, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return fmt.Errorf("%s: bad cpu limit %q", HelperArg, args[2])
	}
	memBytes, err := strconv.ParseInt(args[3], 10, 64)
	if err != nil {
		return fmt.Errorf("%s: bad memory limit %q", HelperArg, args[3])
	}
	path := args[4]

	// Everything that allocates happens BEFORE the limits go on. Setting
	// RLIMIT_AS below what this helper has already reserved would make the
	// allocations underneath syscall.Exec fail, and the door would never start
	// — a memory limit that stops the program launching instead of stopping it
	// growing is a confusing way to be right.
	argv := append([]string{path}, args[5:]...)
	env := os.Environ()

	if err := applyLimits(time.Duration(cpuSecs)*time.Second, memBytes); err != nil {
		return err
	}
	return syscall.Exec(path, argv, env)
}

// applyLimits sets this process's resource limits.
func applyLimits(cpu time.Duration, mem int64) error {
	if cpu > 0 {
		secs := uint64(cpu.Seconds())
		if secs == 0 {
			secs = 1
		}
		// Soft below hard, so a door that handles SIGXCPU gets its chance to
		// save and quit before the kernel stops asking. Neither is the safety
		// net — see the note in limits.go about Go doors and WallClock.
		lim := syscall.Rlimit{Cur: secs, Max: secs + 5}
		if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &lim); err != nil {
			return fmt.Errorf("apply cpu limit: %w", err)
		}
	}
	if mem > 0 && Supported(LimitMemory) {
		lim := syscall.Rlimit{Cur: uint64(mem), Max: uint64(mem)}
		if err := syscall.Setrlimit(syscall.RLIMIT_AS, &lim); err != nil {
			return fmt.Errorf("apply memory limit: %w", err)
		}
	}
	return nil
}

// limitCommand rewrites a door's path and arguments to go through the helper,
// when there is a limit to apply.
//
// A door with no CPU or memory limit is executed directly, so the helper is not
// in the path of the ordinary case and cannot break it.
func limitCommand(self string, spec Spec) (string, []string) {
	if spec.CPULimit <= 0 && spec.MemLimit <= 0 {
		return spec.Path, spec.Args
	}
	args := []string{
		HelperArg,
		strconv.FormatInt(int64(spec.CPULimit.Seconds()), 10),
		strconv.FormatInt(spec.MemLimit, 10),
		spec.Path,
	}
	return self, append(args, spec.Args...)
}
