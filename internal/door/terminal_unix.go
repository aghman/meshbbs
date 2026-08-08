//go:build !windows

package door

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// envKeysAreCaseInsensitive is false on Unix: PATH and Path are two variables.
const envKeysAreCaseInsensitive = false

// baseEnv is the platform minimum every door gets. See Spec.environ for why the
// list is this short.
func baseEnv() []string {
	env := []string{}
	if p := os.Getenv("PATH"); p != "" {
		env = append(env, "PATH="+p)
	}
	return env
}

// unixTerminal is a door on a pty.
type unixTerminal struct {
	ptmx *os.File
	cmd  *exec.Cmd
	// pgid is the door's process GROUP, which is what gets signalled. Signalling
	// the pid alone leaves anything the door forked running, still holding the
	// pty open — and holding the user's session with it, since the bridge waits
	// for the terminal to reach end of stream.
	pgid int

	// fdMu guards the operations that reach the RAW file descriptor against
	// Close.
	//
	// Read and Write are safe without it: they go through os.File's poller,
	// which is reference counted and fails cleanly after a close. Resize is
	// not, because setting the window size is an ioctl and an ioctl needs
	// File.Fd() — which hands out the bare descriptor and takes it out of the
	// poller's care. A resize arriving as the terminal closes would then ioctl
	// a number the kernel has already handed to something else.
	fdMu   sync.Mutex
	closed bool

	closeOnce sync.Once
	closeErr  error
}

func startTerminal(spec Spec, sess Session) (terminal, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.environ(sess)

	size := &pty.Winsize{
		Rows: uint16(max(sess.Height, 1)),
		Cols: uint16(max(sess.Width, 1)),
	}
	// StartWithSize sets Setsid and Setctty, which is what gives the door a
	// controlling terminal AND makes it a session leader — so its process group
	// id equals its pid, and one signal reaches everything it forks.
	ptmx, err := pty.StartWithSize(cmd, size)
	if err != nil {
		return nil, err
	}
	return &unixTerminal{ptmx: ptmx, cmd: cmd, pgid: cmd.Process.Pid}, nil
}

func (t *unixTerminal) Pid() int { return t.cmd.Process.Pid }

// Read passes the door's output through, translating the way a pty reports the
// far end going away.
//
// When the last process holding the slave side exits, Linux fails the master's
// read with EIO rather than returning end of stream. Passing that up as an
// error would make every clean door exit look like a fault, and would leave
// io.Copy reporting a failure for the most ordinary thing a door can do.
func (t *unixTerminal) Read(p []byte) (int, error) {
	n, err := t.ptmx.Read(p)
	if err != nil && errors.Is(err, syscall.EIO) {
		return n, io.EOF
	}
	return n, err
}

func (t *unixTerminal) Write(p []byte) (int, error) { return t.ptmx.Write(p) }

func (t *unixTerminal) Resize(width, height int) error {
	t.fdMu.Lock()
	defer t.fdMu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	return pty.Setsize(t.ptmx, &pty.Winsize{
		Rows: uint16(max(height, 1)),
		Cols: uint16(max(width, 1)),
	})
}

func (t *unixTerminal) Wait() (int, error) {
	err := t.cmd.Wait()
	if t.cmd.ProcessState != nil {
		// ExitCode is -1 for a signalled process, which is the honest answer:
		// a door we killed did not choose a status.
		return t.cmd.ProcessState.ExitCode(), err
	}
	return -1, err
}

func (t *unixTerminal) Terminate() error { return t.signal(syscall.SIGTERM) }

func (t *unixTerminal) KillGroup() error { return t.signal(syscall.SIGKILL) }

func (t *unixTerminal) signal(sig syscall.Signal) error {
	if t.pgid <= 0 {
		return nil
	}
	// The negative pid is the whole group. ESRCH means it has already gone,
	// which is the common case on the tidy-up path and not a failure.
	if err := syscall.Kill(-t.pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (t *unixTerminal) Close() error {
	t.closeOnce.Do(func() {
		t.fdMu.Lock()
		t.closed = true
		t.closeErr = t.ptmx.Close()
		t.fdMu.Unlock()
	})
	return t.closeErr
}
