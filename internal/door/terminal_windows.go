//go:build windows

package door

import (
	"fmt"
	"os"
	"sync"
	"syscall"

	"github.com/charmbracelet/x/conpty"
	"golang.org/x/sys/windows"
)

// envKeysAreCaseInsensitive is true on Windows: PATH and Path are one variable,
// and treating them as two produces a block with a duplicate key in it.
const envKeysAreCaseInsensitive = true

// baseEnv is the platform minimum every door gets.
//
// Longer than the Unix list, and not by preference. A Windows process started
// with an environment that lacks SYSTEMROOT typically fails before main runs —
// the loader needs it to find system DLLs — so an empty environment is not a
// strict sandbox here, it is a door that does not start. The rest of the list is
// what a console program is entitled to assume exists.
func baseEnv() []string {
	keep := []string{
		"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR",
		"PATH", "PATHEXT", "COMSPEC",
		"TEMP", "TMP",
		"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
	}
	env := make([]string, 0, len(keep))
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// windowsTerminal is a door on a ConPTY, inside a job object (§9.3).
//
// The job object is how "kill everything the door started" is expressed here:
// Windows has no process group to signal, so the door and its descendants are
// enrolled in a job and the job is terminated as a unit. It is also set to kill
// on close, so a server that crashes does not leave doors running.
type windowsTerminal struct {
	cpty *conpty.ConPty
	job  windows.Handle
	proc windows.Handle
	pid  int

	// handleMu guards the ConPTY handle against Close, for the same reason the
	// Unix side guards its raw descriptor: resizing a pseudo console that has
	// just been freed is a use-after-close on a handle the OS may have reissued.
	handleMu sync.Mutex
	closed   bool

	closeOnce sync.Once
	closeErr  error
}

func startTerminal(spec Spec, sess Session) (terminal, error) {
	cpty, err := conpty.New(max(sess.Width, 1), max(sess.Height, 1), 0)
	if err != nil {
		return nil, fmt.Errorf("create pseudo console: %w", err)
	}

	job, err := newDoorJob(spec)
	if err != nil {
		_ = cpty.Close()
		return nil, err
	}

	// argv[0] is the program's own name, as everywhere else.
	argv := append([]string{spec.Path}, spec.Args...)
	pid, handle, err := cpty.Spawn(spec.Path, argv, &syscall.ProcAttr{
		Dir: spec.Dir,
		Env: spec.environ(sess),
		Sys: &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP},
	})
	if err != nil {
		_ = windows.CloseHandle(job)
		_ = cpty.Close()
		return nil, err
	}
	proc := windows.Handle(handle)

	// Enrolled after the fact, because Spawn does not hand back the thread
	// handle a CREATE_SUSPENDED start would need in order to resume it. The
	// window is between process creation and this call, and a door that manages
	// to fork inside it escapes the job. Nothing else here depends on that
	// window being closed, and closing it means reimplementing Spawn.
	if err := windows.AssignProcessToJobObject(job, proc); err != nil {
		_ = windows.TerminateProcess(proc, 1)
		_ = windows.CloseHandle(proc)
		_ = windows.CloseHandle(job)
		_ = cpty.Close()
		return nil, fmt.Errorf("assign door to job object: %w", err)
	}

	return &windowsTerminal{cpty: cpty, job: job, proc: proc, pid: pid}, nil
}

// newDoorJob creates the job a door and its descendants live in, carrying
// kill-on-close and whatever limits the door was configured with.
func newDoorJob(spec Spec) (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create job object: %w", err)
	}
	if err := applyJobLimits(job, spec.CPULimit, spec.MemLimit); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func (t *windowsTerminal) Pid() int { return t.pid }

func (t *windowsTerminal) Read(p []byte) (int, error)  { return t.cpty.Read(p) }
func (t *windowsTerminal) Write(p []byte) (int, error) { return t.cpty.Write(p) }

func (t *windowsTerminal) Resize(width, height int) error {
	t.handleMu.Lock()
	defer t.handleMu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	return t.cpty.Resize(max(width, 1), max(height, 1))
}

func (t *windowsTerminal) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(t.proc, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(t.proc, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

// Terminate has nothing polite to say on Windows.
//
// The Unix path sends SIGTERM and gives the door a moment to save and quit.
// There is no equivalent reachable from here: a console control event has to be
// sent from a process attached to the door's console, and this server is
// attached to its own. Rather than pretend, this kills — which means the grace
// period in Run elapses without effect on Windows, and a door that wanted to
// save state on the way out does not get to.
func (t *windowsTerminal) Terminate() error { return t.KillGroup() }

func (t *windowsTerminal) KillGroup() error {
	if t.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(t.job, 1)
}

func (t *windowsTerminal) Close() error {
	t.closeOnce.Do(func() {
		t.handleMu.Lock()
		defer t.handleMu.Unlock()
		t.closed = true
		t.closeErr = t.cpty.Close()
		if t.proc != 0 {
			_ = windows.CloseHandle(t.proc)
		}
		if t.job != 0 {
			// Kill-on-close: anything still in the job goes with it.
			_ = windows.CloseHandle(t.job)
		}
	})
	return t.closeErr
}
