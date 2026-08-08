package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/blobstore"
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
// # The filesystem is a projection, not a directory
//
// What a client sees is exactly two levels deep and comes from the database:
//
//	/                the file areas (§6.5)
//	/utils/          the files catalogued in the `utils` area
//	/utils/FOO.ZIP   one file, whose bytes come from the blob store
//
// No part of a client's path is ever joined onto a real one. That is a stronger
// property than the prefix check this handler used to rely on: a traversal
// attempt cannot escape a root it never reaches, because the only thing a path
// can name is an area row and a catalog row. The bytes live under hashes the
// client never supplies (§6.5).

// sftpHandler serves one session's SFTP subsystem.
func (s *Server) sftpHandler(sess ssh.Session) {
	d, ok := decisionFrom(sess)
	if !ok || d.Intent != IntentAuthenticated {
		// Guests and half-authenticated sessions get no filesystem at all.
		fmt.Fprintln(sess.Stderr(), "file transfer requires a registered account")
		_ = sess.Exit(1)
		return
	}

	if s.blobs == nil || s.svc == nil {
		s.log.Error("sftp unavailable", "err", "no file area directory configured")
		fmt.Fprintln(sess.Stderr(), "file areas are not configured on this BBS")
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

	fsys := &areaFS{
		ctx:      ctx,
		store:    s.store,
		svc:      s.svc,
		blobs:    s.blobs,
		nick:     d.Nick,
		readOnly: !allowed,
		log:      s.log,
	}
	srv := sftp.NewRequestServer(sess, sftp.Handlers{
		FileGet:  fsys,
		FilePut:  fsys,
		FileCmd:  fsys,
		FileList: fsys,
	})
	defer srv.Close()

	s.log.Info("sftp session", "user", d.Nick, "writable", allowed)
	if err := srv.Serve(); err != nil && err != io.EOF {
		s.log.Debug("sftp session ended", "user", d.Nick, "err", err)
	}
}

// areaFS projects the file catalog as a virtual filesystem.
type areaFS struct {
	ctx   context.Context
	store *store.Store
	// svc catalogues an upload and announces it where the area federates. The
	// store alone would only do the first half, and a file that never reaches
	// the network is the failure §6.5 exists to prevent.
	svc      *bbs.Service
	blobs    *blobstore.Store
	nick     string
	readOnly bool
	log      *slog.Logger
}

// vpath is a parsed client path: root, an area, or a file within one.
type vpath struct {
	area string // "" at the root
	name string // "" for the root and for an area directory
}

func (v vpath) isRoot() bool { return v.area == "" }
func (v vpath) isFile() bool { return v.area != "" && v.name != "" }

// The two ways a well-formed path can still be the wrong shape. Both keep
// their own wording rather than borrowing an errno, because neither is what a
// POSIX filesystem would be complaining about: this is a two-level projection,
// and saying so is more use to whoever is reading the message than ENOTDIR.
var (
	errNotAFile = errors.New("not a file: files live at /area/name")
	errTooDeep  = errors.New("file areas are two levels deep: /area/file")
)

// parsePath turns a client path into an area and name.
//
// path.Clean collapses "." and ".." before anything else happens, so a
// traversal resolves to the root instead of escaping — and then the segment
// count refuses anything deeper than an area and a file. Nothing here touches
// the real filesystem.
func parsePath(virtual string) (vpath, error) {
	clean := path.Clean("/" + strings.TrimPrefix(virtual, "/"))
	if clean == "/" {
		return vpath{}, nil
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	switch len(parts) {
	case 1:
		return vpath{area: parts[0]}, nil
	case 2:
		return vpath{area: parts[0], name: parts[1]}, nil
	default:
		return vpath{}, errTooDeep
	}
}

// Fileread serves a file's bytes out of the blob store.
func (a *areaFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	v, err := parsePath(r.Filepath)
	if err != nil {
		return nil, err
	}
	if !v.isFile() {
		return nil, fmt.Errorf("%s: %w", r.Filepath, errNotAFile)
	}

	f, err := a.store.GetFile(a.ctx, v.area, v.name)
	if err != nil {
		return nil, translate("open", r.Filepath, err)
	}
	blob, err := a.blobs.Open(f.Hash)
	if errors.Is(err, blobstore.ErrNotFound) {
		// The catalog knows about content this node does not hold. Once FILE
		// records replicate, that is the ordinary case for a file held by
		// another BBS (§6.5) — but over SFTP there is nothing to offer, and
		// saying so beats a truncated download.
		return nil, fmt.Errorf("%s is catalogued here but its contents are not held on this BBS", r.Filepath)
	}
	return blob, err
}

// Filewrite stages an upload, which becomes a catalog entry when it closes.
func (a *areaFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if a.readOnly {
		return nil, fmt.Errorf("permission denied: your account cannot upload files")
	}
	v, err := parsePath(r.Filepath)
	if err != nil {
		return nil, err
	}
	if !v.isFile() {
		return nil, fmt.Errorf("%s: %w", r.Filepath, errNotAFile)
	}
	if err := store.ValidateFileName(v.name); err != nil {
		return nil, err
	}
	// Fail before a byte is transferred rather than after. A client that has
	// just spent four minutes pushing a file over a slow link deserves to have
	// been told about a missing area at the start.
	area, err := a.store.GetFileArea(a.ctx, v.area)
	if err != nil {
		return nil, translate("open", r.Filepath, err)
	}
	// Every reason this upload could be refused, checked before a byte moves:
	// the area's own read-only flag, the [N7] capability gate on a federated
	// area, and a name already taken.
	if err := a.svc.CanUploadTo(a.ctx, area.Name, a.nick); err != nil {
		return nil, fmt.Errorf("permission denied: %w", err)
	}
	if _, err := a.store.GetFile(a.ctx, v.area, v.name); err == nil {
		return nil, fmt.Errorf("%s already exists in %s", v.name, area.Name)
	}

	staging, err := a.blobs.Temp()
	if err != nil {
		return nil, err
	}
	return &upload{fs: a, area: area.Name, name: v.name, staging: staging}, nil
}

// upload is a transfer in progress.
//
// SFTP hands us an io.WriterAt: a client may write byte ranges in any order, so
// the content cannot be hashed as it arrives. It is staged inside the blob
// store — same filesystem, so finishing is a rename — and becomes a blob and a
// catalog row only when the handle closes cleanly.
type upload struct {
	fs      *areaFS
	area    string
	name    string
	staging *os.File

	mu     sync.Mutex
	failed error
	done   bool
}

func (u *upload) WriteAt(p []byte, off int64) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failed != nil {
		return 0, u.failed
	}
	return u.staging.WriteAt(p, off)
}

// TransferError is called by the SFTP server when the session dies mid-flight.
//
// It runs before Close, which is what makes this work: Close sees the flag and
// discards the staging file instead of publishing a partial upload under a name
// that says it is complete.
func (u *upload) TransferError(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.failed == nil {
		u.failed = err
	}
}

// Close finalizes the upload: hash the staged bytes, adopt them as a blob, and
// write the catalog row.
func (u *upload) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return nil
	}
	u.done = true

	name := u.staging.Name()
	if u.failed != nil {
		u.staging.Close()
		_ = os.Remove(name)
		u.fs.log.Debug("sftp upload abandoned", "user", u.fs.nick,
			"area", u.area, "file", u.name, "err", u.failed)
		return nil
	}

	hash, size, err := u.fs.blobs.Adopt(u.staging)
	if err != nil {
		_ = os.Remove(name)
		return err
	}

	// The bytes are in the store before the row exists. If this fails, what is
	// left behind is an unreferenced blob a maintenance pass can collect —
	// never a row promising content that was never written.
	saved, err := u.fs.svc.AddFile(u.fs.ctx, u.area, store.File{
		Name:     u.name,
		Hash:     hash,
		Size:     size,
		Uploader: u.fs.nick,
	})
	if err != nil {
		return err
	}
	u.fs.log.Info("file uploaded", "user", u.fs.nick, "area", u.area,
		"file", u.name, "bytes", size, "announced", saved.Published())
	return nil
}

// Filecmd handles the mutating operations.
func (a *areaFS) Filecmd(r *sftp.Request) error {
	if a.readOnly {
		return fmt.Errorf("permission denied: your account cannot modify files")
	}
	v, err := parsePath(r.Filepath)
	if err != nil {
		return err
	}

	switch r.Method {
	case "Remove":
		if !v.isFile() {
			return fmt.Errorf("%s: %w", r.Filepath, errNotAFile)
		}
		return a.remove(r.Filepath, v)

	case "Rename", "PosixRename":
		to, err := parsePath(r.Target)
		if err != nil {
			return err
		}
		if !v.isFile() || !to.isFile() {
			return fmt.Errorf("rename %s to %s: %w", r.Filepath, r.Target, errNotAFile)
		}
		return translate("rename", r.Filepath, a.store.RenameFile(a.ctx, v.area, v.name, to.area, to.name))

	case "Mkdir":
		// A directory here IS a file area, and creating one is a sysop
		// decision: a federated area spends the network's airtime (§6.5), which
		// is not something an SFTP client should be able to do by accident.
		return errors.New("file areas are created by the sysop: meshbbs area create <name> --files")

	case "Rmdir":
		return errors.New("file areas are removed by the sysop, not over SFTP")

	case "Setstat":
		// Permissions and timestamps are ours to decide, not the client's;
		// accept and ignore rather than failing a transfer that would otherwise
		// succeed.
		return nil
	}
	return fmt.Errorf("unsupported operation %q", r.Method)
}

// remove deletes a catalog entry, and the bytes if nothing else wants them.
func (a *areaFS) remove(display string, v vpath) error {
	orphaned, hash, err := a.store.RemoveFile(a.ctx, v.area, v.name)
	if err != nil {
		return translate("remove", display, err)
	}
	if !orphaned {
		// Another area still catalogues this content. Content addressing means
		// they are the same bytes, so deleting them would break the other entry.
		return nil
	}
	if err := a.blobs.Remove(hash); err != nil {
		// The row is already gone, so the file IS deleted as far as the user is
		// concerned. Report the leftover bytes rather than failing an operation
		// that mostly succeeded.
		a.log.Error("blob left behind after delete", "hash", hash, "err", err)
	}
	return nil
}

// Filelist serves directory listings and stats.
func (a *areaFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	v, err := parsePath(r.Filepath)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "List":
		if v.isFile() {
			return nil, fmt.Errorf("%s is a file, not a directory", r.Filepath)
		}
		if v.isRoot() {
			areas, err := a.store.ListFileAreas(a.ctx)
			if err != nil {
				return nil, err
			}
			infos := make([]os.FileInfo, 0, len(areas))
			for _, area := range areas {
				infos = append(infos, dirInfo{name: area.Name, modTime: unix(area.CreatedAt)})
			}
			return listerAt(infos), nil
		}
		files, err := a.store.ListFiles(a.ctx, v.area)
		if err != nil {
			return nil, translate("readdir", r.Filepath, err)
		}
		infos := make([]os.FileInfo, 0, len(files))
		for _, f := range files {
			infos = append(infos, fileInfo{f: f})
		}
		return listerAt(infos), nil

	case "Stat", "Lstat":
		info, err := a.stat(r.Filepath, v)
		if err != nil {
			return nil, err
		}
		return listerAt{info}, nil
	}
	return nil, fmt.Errorf("unsupported operation %q", r.Method)
}

// stat describes one path.
func (a *areaFS) stat(display string, v vpath) (os.FileInfo, error) {
	if v.isRoot() {
		return dirInfo{name: "/"}, nil
	}
	if !v.isFile() {
		area, err := a.store.GetFileArea(a.ctx, v.area)
		if err != nil {
			return nil, translate("stat", display, err)
		}
		return dirInfo{name: area.Name, modTime: unix(area.CreatedAt)}, nil
	}
	f, err := a.store.GetFile(a.ctx, v.area, v.name)
	if err != nil {
		return nil, translate("stat", display, err)
	}
	return fileInfo{f: f}, nil
}

// translate maps store errors onto ones an SFTP client understands.
//
// It must return a *fs.PathError specifically, not an error wrapping
// fs.ErrNotExist. pkg/sftp decides the status code with os.IsNotExist, which
// unwraps *fs.PathError and nothing else — a fmt.Errorf("%w") wrapper comes out
// as SSH_FX_FAILURE, and a client prints a generic failure for a file that is
// merely absent. Caught by driving a real client in sftp_protocol_test.go;
// calling the handler's methods directly cannot see it.
//
// A message area translates to "does not exist" rather than to an explanation.
// That is not a lost message: this filesystem projects the FILE areas, so a
// message area genuinely is not a path here (§6.5).
func translate(op, path string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrWrongAreaKind):
		return &fs.PathError{Op: op, Path: path, Err: fs.ErrNotExist}
	default:
		return err
	}
}

func unix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// dirInfo describes a file area as a directory.
type dirInfo struct {
	name    string
	modTime time.Time
}

func (d dirInfo) Name() string       { return d.name }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() os.FileMode  { return os.ModeDir | 0o555 }
func (d dirInfo) ModTime() time.Time { return d.modTime }
func (d dirInfo) IsDir() bool        { return true }
func (d dirInfo) Sys() any           { return nil }

// fileInfo describes a catalogued file.
//
// The mode is read-only even for a user who may upload: SFTP has no way to
// express "you may create and delete here but not modify in place", and
// modifying in place is exactly what content addressing does not do. A client
// that believes it can seek into an existing blob and rewrite a byte range
// would be wrong in a way that is expensive to discover.
type fileInfo struct{ f store.File }

func (f fileInfo) Name() string       { return f.f.Name }
func (f fileInfo) Size() int64        { return f.f.Size }
func (f fileInfo) Mode() os.FileMode  { return 0o444 }
func (f fileInfo) ModTime() time.Time { return unix(f.f.UploadedAt) }
func (f fileInfo) IsDir() bool        { return false }
func (f fileInfo) Sys() any           { return nil }

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
