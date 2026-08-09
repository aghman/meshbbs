//go:build windows

package door

import (
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The job object's limits, applied for real.
//
// Everything Windows-specific in this package is compile-checked on every
// change and executed only here, on the CI runner §12.9 calls non-negotiable.
// This is the narrowest test that can catch the failure most likely to be
// waiting: SetInformationJobObject validates the structure it is handed, and a
// wrong size, a bad flag combination or a field set without its flag comes back
// as an error rather than as a compile failure. A limit that could not be
// applied would otherwise be discovered by a door that was never bounded.
func TestJobObjectAcceptsOurLimits(t *testing.T) {
	cases := []struct {
		name string
		cpu  time.Duration
		mem  int64
	}{
		{"neither", 0, 0},
		{"cpu only", 90 * time.Second, 0},
		{"memory only", 0, 64 << 20},
		{"both", 90 * time.Second, 64 << 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, err := windows.CreateJobObject(nil, nil)
			if err != nil {
				t.Fatalf("create job object: %v", err)
			}
			defer windows.CloseHandle(job) //nolint:errcheck

			if err := applyJobLimits(job, tc.cpu, tc.mem); err != nil {
				t.Fatalf("apply cpu=%v mem=%d: %v", tc.cpu, tc.mem, err)
			}
		})
	}
}

// And the whole path, since a job that accepts limits is not the same as a door
// that starts inside one.
func TestADoorStartsInsideAJobWithLimits(t *testing.T) {
	spec := Spec{
		Name: "limited", Path: mustExecutable(t), Dir: t.TempDir(),
		Env:       []string{helperEnv + "=exit", helperArgEnv + "=0"},
		WallClock: time.Minute,
		CPULimit:  90 * time.Second,
		MemLimit:  256 << 20,
	}
	term, err := startTerminal(spec, Session{Term: "xterm", Width: 80, Height: 24})
	if err != nil {
		t.Fatalf("a door with limits would not start: %v", err)
	}
	defer term.Close()

	code, err := term.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != 0 {
		t.Errorf("the door exited %d, want 0", code)
	}
}
