package sshd

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aghman/meshbbs/internal/store"
	"github.com/charmbracelet/ssh"
	"github.com/pkg/sftp"
)

// SFTP gives file transfer for free (§5.1).
//
// This is a large simplification over legacy BBS software: no ZMODEM, no
// XMODEM, no serial-protocol emulation. `sftp bbs.example.com` and you are in
// the file areas.
//
// The filesystem exposed is virtual and rooted at the file area directory. A
// user never sees a real path, which is what keeps a traversal attempt from
// reaching the node key sitting a couple of directories up.

// sftpHandler serves one session's SFTP subsystem.
func (s *Server) sftpHandler(sess ssh.Session) {
	d, ok := decisionFrom(sess)
	if !ok || d.Intent != IntentAuthenticated {
		// Guests and half-authenticated sessions get no filesystem at all.
		fmt.Fprintln(sess.Stderr(), "file transfer requires a registered account")
		_ = sess.Exit(1)
		return
	}

	ctx := sess.Context()
	allowed, err := s.store.HasCapability(ctx, d.Nick, store.CapUploadFiles)
	if err != nil {
		s.log.Error("sftp capability check failed", "err", err)
		_ = sess.Exit(1)
		return
	}

	root, err := s.fileRoot()
	if err != nil {
		s.log.Error("sftp root unavailable", "err", err)
		_ = sess.Exit(1)
		return
	}

	fs := &areaFS{root: root, nick: d.Nick, readOnly: !allowed}
	srv := sftp.NewRequestServer(sess, sftp.Handlers{
		FileGet:  fs,
		FilePut:  fs,
		FileCmd:  fs,
		FileList: fs,
	})
	defer srv.Close()

	s.log.Info("sftp session", "user", d.Nick, "writable", allowed)
	if err := srv.Serve(); err != nil && err != io.EOF {
		s.log.Debug("sftp session ended", "user", d.Nick, "err", err)
	}
}

// fileRoot returns the directory holding file areas, creating it if needed.
func (s *Server) fileRoot() (string, error) {
	root := s.opts.FilesDir
	if root == "" {
		return "", fmt.Errorf("no file area directory configured")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	return filepath.Abs(root)
}

// areaFS is a virtual filesystem rooted at the file areas.
type areaFS struct {
	root     string
	nick     string
	readOnly bool
}

// resolve maps a virtual path to a real one, refusing to escape the root.
//
// path.Clean on the joined virtual path collapses ".." before it is ever
// turned into a real path, and the final prefix check is the belt to that
// braces: an SFTP client is remote input, and a traversal here would reach the
// node key.
func (fs *areaFS) resolve(virtual string) (string, error) {
	clean := path.Clean("/" + strings.TrimPrefix(virtual, "/"))
	real := filepath.Join(fs.root, filepath.FromSlash(clean))

	abs, err := filepath.Abs(real)
	if err != nil {
		return "", err
	}
	if abs != fs.root && !strings.HasPrefix(abs, fs.root+string(filepath.Separator)) {
		return "", fmt.Errorf("permission denied")
	}
	return abs, nil
}

func (fs *areaFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	p, err := fs.resolve(r.Filepath)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (fs *areaFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if fs.readOnly {
		return nil, fmt.Errorf("permission denied: your account cannot upload files")
	}
	p, err := fs.resolve(r.Filepath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
}

func (fs *areaFS) Filecmd(r *sftp.Request) error {
	if fs.readOnly {
		return fmt.Errorf("permission denied: your account cannot modify files")
	}
	p, err := fs.resolve(r.Filepath)
	if err != nil {
		return err
	}
	switch r.Method {
	case "Mkdir":
		return os.Mkdir(p, 0o700)
	case "Remove":
		return os.Remove(p)
	case "Rename":
		target, err := fs.resolve(r.Target)
		if err != nil {
			return err
		}
		return os.Rename(p, target)
	case "Setstat":
		// Permissions are ours to decide, not the client's; accept and ignore
		// rather than failing a transfer that would otherwise succeed.
		return nil
	}
	return fmt.Errorf("unsupported operation %q", r.Method)
}

func (fs *areaFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	p, err := fs.resolve(r.Filepath)
	if err != nil {
		return nil, err
	}
	switch r.Method {
	case "List":
		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		infos := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue
			}
			infos = append(infos, info)
		}
		return listerAt(infos), nil
	case "Stat":
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		return listerAt{info}, nil
	}
	return nil, fmt.Errorf("unsupported operation %q", r.Method)
}

type listerAt []os.FileInfo

func (l listerAt) ListAt(f []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(f, l[offset:])
	if n < len(f) {
		return n, io.EOF
	}
	return n, nil
}
