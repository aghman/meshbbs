//go:build !windows

package door

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The CPU limit, against a door that will not stop on its own.
//
// The door is awk rather than this test binary, and the reason is the caveat
// the whole feature carries: a door written in GO ignores RLIMIT_CPU, because
// the Go runtime delivers SIGXCPU to a listener and ignores it when there is
// none. Measured — a Go child spun ten seconds against a one-second limit and
// was never signalled, while awk died in one. Testing this with a Go door would
// assert the opposite of what the code does.
//
// This is why the CPU limit is a refinement and WallClock is the safety net:
// WallClock applies to every door whatever it is written in.
func TestCPULimitStopsARunawayDoor(t *testing.T) {
	if !Supported(LimitCPU) {
		t.Skipf("%s cannot enforce a cpu limit", runtime.GOOS)
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("no awk to spin")
	}

	h := newHarness(t, realClock(), "", "")
	h.spec.Path = awk
	h.spec.Args = []string{"BEGIN{while(1){x+=1}}"}
	h.spec.Env = nil
	h.spec.CPULimit = time.Second
	// Far longer than the CPU limit, so that what stops the door is provably
	// the CPU limit and not the bound that always holds.
	h.spec.WallClock = 5 * time.Minute

	done := make(chan Result, 1)
	go func() {
		res, err := h.mgr.Run(context.Background(), h.spec, h.session())
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- res
	}()

	select {
	case res := <-done:
		// Stopped on its own, as far as the runner is concerned: the kernel
		// signalled it, so this is not the wall-clock or cancellation path.
		if res.Stop != StopExited {
			t.Errorf("stopped because %v, want %v — the CPU limit was not what "+
				"ended it", res.Stop, StopExited)
		}
		if res.Runtime > 30*time.Second {
			t.Errorf("the door ran %v against a one-second cpu limit", res.Runtime)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("a door with a one-second cpu limit outlived a minute")
	}
}

// A door with no limits is executed directly, so the helper is not in the path
// of the ordinary case and cannot break it.
func TestUnlimitedDoorsSkipTheHelper(t *testing.T) {
	spec := Spec{Path: "/opt/doors/tw", Args: []string{"-node", "1"}}
	path, args := limitCommand("/usr/local/bin/meshbbs", spec)
	if path != spec.Path || len(args) != 2 {
		t.Errorf("an unlimited door goes through %q %v", path, args)
	}

	spec.CPULimit = 90 * time.Second
	spec.MemLimit = 1 << 20
	path, args = limitCommand("/usr/local/bin/meshbbs", spec)
	if path != "/usr/local/bin/meshbbs" {
		t.Errorf("a limited door runs %q, want the server binary", path)
	}
	want := []string{HelperArg, "90", "1048576", "/opt/doors/tw", "-node", "1"}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("helper arg %d is %q, want %q", i, args[i], want[i])
		}
	}
}

// The memory limit, where the platform has one.
//
// Skipped on macOS, which cannot set RLIMIT_AS or RLIMIT_DATA at all — measured,
// not assumed: both return EINVAL and a child asked for 256 MiB against a 64 MiB
// limit gets it. So this runs on the Linux runner and nowhere else.
//
// It runs the SAME door twice, once with the limit and once without, because a
// test that only checks the limited case passes for free if awk cannot allocate
// 256 MiB on the runner for some unrelated reason. The control run is what makes
// the limited run mean something.
func TestMemoryLimitStopsAGreedyDoor(t *testing.T) {
	if !Supported(LimitMemory) {
		t.Skipf("%s cannot enforce a memory limit", runtime.GOOS)
	}
	awk, err := exec.LookPath("awk")
	if err != nil {
		t.Skip("no awk to allocate with")
	}
	// A doubling loop rather than a printf width, because the latter is not
	// portable across awk implementations. 2^28 is about 268 MiB.
	//
	// STARTED is printed BEFORE the allocation, and it is what separates the
	// two ways this test can go green. A door that never launched also never
	// prints ALLOCATED — so without this marker the case passes when the limit
	// works and equally when applying it failed and the door died on the launch
	// pad, which is the bug most likely to be here.
	const program = `BEGIN{ print "STARTED"; s="x"; for(i=0;i<28;i++) s = s s; print "ALLOCATED" }`

	run := func(t *testing.T, mem int64) string {
		t.Helper()
		h := newHarness(t, realClock(), "", "")
		h.spec.Path = awk
		h.spec.Args = []string{program}
		h.spec.Env = nil
		h.spec.MemLimit = mem
		h.spec.WallClock = 2 * time.Minute
		if _, err := h.mgr.Run(context.Background(), h.spec, h.session()); err != nil {
			t.Fatalf("run: %v", err)
		}
		return h.screen.String()
	}

	// Control: with no limit it gets what it asked for. If this fails, the
	// runner could not have allocated it anyway and the case below proves
	// nothing.
	if got := run(t, 0); !strings.Contains(got, "ALLOCATED") {
		t.Skipf("the runner could not allocate 256 MiB unlimited, so the limited "+
			"case would prove nothing: %q", got)
	}

	got := run(t, 64<<20)
	if !strings.Contains(got, "STARTED") {
		t.Fatalf("the door never ran, so the limit proved nothing — applying it "+
			"probably failed: %q", got)
	}
	if strings.Contains(got, "ALLOCATED") {
		t.Errorf("a door allocated 256 MiB against a 64 MiB limit: %q", got)
	}
}
