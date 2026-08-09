package door

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A limit this platform cannot apply is refused when the door launches. Never
// ignored: a sysop who set a memory limit believes there is one (§9.4).
func TestUnsupportedLimitsAreRefused(t *testing.T) {
	h := newHarness(t, realClock(), "exit", "0")
	h.spec.MemLimit = 64 << 20

	_, err := h.mgr.Run(context.Background(), h.spec, h.session())
	if Supported(LimitMemory) {
		if err != nil {
			t.Errorf("a supported memory limit was refused: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("%s accepted a memory limit it cannot enforce", runtime.GOOS)
	}
	for _, want := range []string{LimitMemory, runtime.GOOS} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// The refusal names only what is actually unsupported, so a sysop is not told
// to clear a limit that works.
func TestSupportedLimitsAreAccepted(t *testing.T) {
	if !Supported(LimitCPU) {
		t.Skipf("%s cannot enforce a cpu limit", runtime.GOOS)
	}
	h := newHarness(t, realClock(), "exit", "5")
	h.spec.CPULimit = 30 * time.Second

	res, err := h.mgr.Run(context.Background(), h.spec, h.session())
	if err != nil {
		t.Fatalf("a door with a cpu limit would not start: %v", err)
	}
	// And it went through the helper without disturbing anything: same exit
	// code, because syscall.Exec replaces the image rather than wrapping it.
	if res.ExitCode != 5 {
		t.Errorf("exit code %d, want 5 — the helper changed the door's status", res.ExitCode)
	}
}

func TestUnsupportedLimitsReportsOnlyWhatIsUnsupported(t *testing.T) {
	got := UnsupportedLimits(0, 0)
	if len(got) != 0 {
		t.Errorf("a door with no limits reported %v", got)
	}
	got = UnsupportedLimits(time.Minute, 1<<20)
	for _, name := range got {
		if Supported(name) {
			t.Errorf("%s is supported here and was reported unsupported", name)
		}
	}
	if Supported(LimitCPU) && Supported(LimitMemory) && len(got) != 0 {
		t.Errorf("a platform that supports both reported %v", got)
	}
}

// The helper is a sentinel, not a subcommand: it must not be reachable by
// anything a user would plausibly type.
//
// Windows has no helper at all — the job object applies limits directly, so
// there is nothing to re-execute for — and IsHelper is false there for every
// input. Asserting that explicitly rather than skipping, because "this platform
// never takes the helper path" is the contract and not an absence of one.
func TestHelperIsRecognisedOnlyAsArgvOne(t *testing.T) {
	sentinel := []string{"meshbbs", HelperArg, "1", "0", "/bin/true"}
	if got := IsHelper(sentinel); got != !isWindows() {
		t.Errorf("IsHelper(argv[1]=%s) is %v on %s", HelperArg, got, runtime.GOOS)
	}
	for _, args := range [][]string{
		{"meshbbs"},
		{"meshbbs", "serve"},
		{"meshbbs", "serve", HelperArg},
		{"meshbbs", "door", "add", HelperArg},
	} {
		if IsHelper(args) {
			t.Errorf("the helper was reached by %v", args)
		}
	}
}
