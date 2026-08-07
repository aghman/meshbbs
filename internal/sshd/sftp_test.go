package sshd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/pkg/sftp"
)

func testAreaFS(t *testing.T, areas ...string) *areaFS {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenMemory(ctx, clock.NewVirtual(time.Unix(1_700_000_000, 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	blobs, err := blobstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range areas {
		if _, err := st.CreateFileArea(ctx, name, "", false); err != nil {
			t.Fatal(err)
		}
	}
	return &areaFS{
		ctx: ctx, store: st, blobs: blobs, nick: "austin",
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// putFile runs a whole transfer: open, write, close.
func putFile(t *testing.T, a *areaFS, path, content string) error {
	t.Helper()
	w, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: path})
	if err != nil {
		return err
	}
	if _, err := w.WriteAt([]byte(content), 0); err != nil {
		return err
	}
	return w.(io.Closer).Close()
}

func download(t *testing.T, a *areaFS, path string) (string, error) {
	t.Helper()
	r, err := a.Fileread(&sftp.Request{Method: "Get", Filepath: path})
	if err != nil {
		return "", err
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	buf := make([]byte, 4096)
	n, err := r.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return "", err
	}
	return string(buf[:n]), nil
}

func names(infos []os.FileInfo) []string {
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.Name())
	}
	return out
}

func list(t *testing.T, a *areaFS, path string) []os.FileInfo {
	t.Helper()
	l, err := a.Filelist(&sftp.Request{Method: "List", Filepath: path})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]os.FileInfo, 64)
	n, err := l.ListAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return buf[:n]
}

// An SFTP client is remote input. The old defence was a prefix check on a
// joined real path; there is no longer a real path to join, so what has to hold
// is that a traversal can only ever name an area and a file within it.
func TestParsePathRefusesTraversal(t *testing.T) {
	for _, attack := range []string{
		"../../../etc/passwd",
		"/../../keys/node.ed25519",
		"uploads/../../../../keys/node.ed25519",
		"....//....//keys",
		"/./../../keys",
		"/utils/../../keys/node.ed25519",
	} {
		v, err := parsePath(attack)
		if err != nil {
			continue // refused outright, which is fine
		}
		for _, seg := range []string{v.area, v.name} {
			if strings.Contains(seg, "..") || strings.ContainsAny(seg, `/\`) {
				t.Errorf("traversal %q produced segment %q", attack, seg)
			}
		}
	}
}

func TestParsePathShapes(t *testing.T) {
	cases := []struct {
		in         string
		area, name string
		wantErr    bool
	}{
		{in: "/", area: "", name: ""},
		{in: "", area: "", name: ""},
		{in: "/utils", area: "utils"},
		{in: "utils", area: "utils"},
		{in: "/utils/", area: "utils"},
		{in: "/utils/ARCHIVE.ZIP", area: "utils", name: "ARCHIVE.ZIP"},
		{in: "/utils/sub/deep.txt", wantErr: true},
		{in: "/a/b/c/d", wantErr: true},
	}
	for _, c := range cases {
		v, err := parsePath(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parsePath(%q) accepted a path deeper than /area/file", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePath(%q) = %v", c.in, err)
			continue
		}
		if v.area != c.area || v.name != c.name {
			t.Errorf("parsePath(%q) = {%q, %q}, want {%q, %q}", c.in, v.area, v.name, c.area, c.name)
		}
	}
}

func TestUploadThenDownload(t *testing.T) {
	a := testAreaFS(t, "utils")
	const content = "MESHBBS.DOC\r\nA file that went up over SFTP.\r\n"

	if err := putFile(t, a, "/utils/MESHBBS.DOC", content); err != nil {
		t.Fatal(err)
	}

	// It is in the catalog, attributed, with the right size.
	f, err := a.store.GetFile(a.ctx, "utils", "MESHBBS.DOC")
	if err != nil {
		t.Fatal(err)
	}
	if f.Uploader != "austin" {
		t.Errorf("uploader is %q, want austin", f.Uploader)
	}
	if f.Size != int64(len(content)) {
		t.Errorf("size is %d, want %d", f.Size, len(content))
	}
	if !a.blobs.Has(f.Hash) {
		t.Error("the blob store does not hold the content it was given")
	}

	got, err := download(t, a, "/utils/MESHBBS.DOC")
	if err != nil {
		t.Fatal(err)
	}
	if got != content {
		t.Errorf("downloaded %q, want %q", got, content)
	}
}

// SFTP writes with random access, so out-of-order ranges must reassemble.
func TestUploadOutOfOrderRanges(t *testing.T) {
	a := testAreaFS(t, "utils")
	w, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: "/utils/SPLIT.BIN"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("WORLD"), 6); err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("HELLO "), 0); err != nil {
		t.Fatal(err)
	}
	if err := w.(io.Closer).Close(); err != nil {
		t.Fatal(err)
	}

	got, err := download(t, a, "/utils/SPLIT.BIN")
	if err != nil {
		t.Fatal(err)
	}
	if got != "HELLO WORLD" {
		t.Errorf("reassembled to %q, want %q", got, "HELLO WORLD")
	}
}

// A dropped session must not leave a partial file catalogued as complete.
func TestAbandonedUploadPublishesNothing(t *testing.T) {
	a := testAreaFS(t, "utils")
	w, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: "/utils/HALF.BIN"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteAt([]byte("first half only"), 0); err != nil {
		t.Fatal(err)
	}

	w.(interface{ TransferError(error) }).TransferError(errors.New("connection reset"))
	if err := w.(io.Closer).Close(); err != nil {
		t.Fatalf("closing an abandoned upload returned %v", err)
	}

	if _, err := a.store.GetFile(a.ctx, "utils", "HALF.BIN"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an abandoned upload left a catalog entry (%v)", err)
	}
	files, err := a.store.ListFiles(a.ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("area holds %d files after an abandoned upload", len(files))
	}
}

func TestUploadRefusedWithoutCapability(t *testing.T) {
	a := testAreaFS(t, "utils")
	a.readOnly = true

	if _, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: "/utils/NOPE.TXT"}); err == nil {
		t.Error("a read-only session was allowed to write")
	}
	if err := a.Filecmd(&sftp.Request{Method: "Remove", Filepath: "/utils/NOPE.TXT"}); err == nil {
		t.Error("a read-only session was allowed to modify files")
	}
}

// Reading is not gated on the upload capability.
func TestReadOnlySessionCanDownload(t *testing.T) {
	a := testAreaFS(t, "utils")
	if err := putFile(t, a, "/utils/PUBLIC.TXT", "readable"); err != nil {
		t.Fatal(err)
	}

	a.readOnly = true
	got, err := download(t, a, "/utils/PUBLIC.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if got != "readable" {
		t.Errorf("downloaded %q", got)
	}
}

// The area check happens before a byte moves: a client on a slow link should
// not spend four minutes uploading into nowhere.
func TestUploadToUnknownAreaIsRefusedUpFront(t *testing.T) {
	a := testAreaFS(t, "utils")
	_, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: "/nosuch/FILE.TXT"})
	if err == nil {
		t.Fatal("upload to a nonexistent area was accepted")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error was %v, want one wrapping fs.ErrNotExist so the client says 'no such file'", err)
	}
}

// A message area is not a place to put files, even though it is an area.
func TestUploadToMessageAreaIsRefused(t *testing.T) {
	a := testAreaFS(t)
	if _, err := a.store.CreateArea(a.ctx, "general", "messages", false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: "/general/FILE.TXT"}); err == nil {
		t.Error("a message area accepted a file upload")
	}
}

func TestUploadRefusesDuplicateName(t *testing.T) {
	a := testAreaFS(t, "utils")
	if err := putFile(t, a, "/utils/DUP.TXT", "first"); err != nil {
		t.Fatal(err)
	}
	if err := putFile(t, a, "/utils/DUP.TXT", "second"); err == nil {
		t.Error("a second upload under the same name was accepted")
	}
}

func TestUploadRefusesUnwritablePaths(t *testing.T) {
	a := testAreaFS(t, "utils")
	for _, p := range []string{"/", "/utils", "/utils/sub/deep.txt", "/utils/bad\x00name"} {
		if _, err := a.Filewrite(&sftp.Request{Method: "Put", Filepath: p}); err == nil {
			t.Errorf("upload to %q was accepted", p)
		}
	}
}

func TestListingProjectsAreasAndFiles(t *testing.T) {
	a := testAreaFS(t, "utils", "games")
	if err := putFile(t, a, "/utils/A.TXT", "a"); err != nil {
		t.Fatal(err)
	}
	if err := putFile(t, a, "/utils/B.TXT", "b"); err != nil {
		t.Fatal(err)
	}

	root := list(t, a, "/")
	if got := names(root); len(got) != 2 || got[0] != "games" || got[1] != "utils" {
		t.Errorf("root listed %v, want [games utils]", got)
	}
	for _, info := range root {
		if !info.IsDir() {
			t.Errorf("area %s is not listed as a directory", info.Name())
		}
	}

	area := list(t, a, "/utils")
	if got := names(area); len(got) != 2 || got[0] != "A.TXT" || got[1] != "B.TXT" {
		t.Errorf("utils listed %v, want [A.TXT B.TXT]", got)
	}
	for _, info := range area {
		if info.IsDir() {
			t.Errorf("file %s is listed as a directory", info.Name())
		}
	}

	if list(t, a, "/games") != nil && len(list(t, a, "/games")) != 0 {
		t.Error("an empty area listed something")
	}
}

func TestStat(t *testing.T) {
	a := testAreaFS(t, "utils")
	if err := putFile(t, a, "/utils/STAT.TXT", "twelve bytes"); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/", "/utils"} {
		l, err := a.Filelist(&sftp.Request{Method: "Stat", Filepath: p})
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		buf := make([]os.FileInfo, 1)
		if _, err := l.ListAt(buf, 0); err != nil && err != io.EOF {
			t.Fatal(err)
		}
		if !buf[0].IsDir() {
			t.Errorf("%s did not stat as a directory", p)
		}
	}

	l, err := a.Filelist(&sftp.Request{Method: "Stat", Filepath: "/utils/STAT.TXT"})
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]os.FileInfo, 1)
	if _, err := l.ListAt(buf, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if buf[0].IsDir() || buf[0].Size() != 12 {
		t.Errorf("file stat is %+v, want a 12-byte file", buf[0])
	}

	if _, err := a.Filelist(&sftp.Request{Method: "Stat", Filepath: "/utils/ABSENT"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat of a missing file returned %v, want fs.ErrNotExist", err)
	}
}

func TestRemoveDeletesRowAndBytes(t *testing.T) {
	a := testAreaFS(t, "utils")
	if err := putFile(t, a, "/utils/GONE.TXT", "delete me"); err != nil {
		t.Fatal(err)
	}
	f, err := a.store.GetFile(a.ctx, "utils", "GONE.TXT")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Filecmd(&sftp.Request{Method: "Remove", Filepath: "/utils/GONE.TXT"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.GetFile(a.ctx, "utils", "GONE.TXT"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the catalog entry survived Remove")
	}
	if a.blobs.Has(f.Hash) {
		t.Error("the bytes survived the last reference to them")
	}
}

// Content addressing means two entries can be the same bytes. Deleting one must
// not break the other.
func TestRemoveKeepsBytesAnotherAreaStillWants(t *testing.T) {
	a := testAreaFS(t, "utils", "games")
	if err := putFile(t, a, "/utils/SHARED.BIN", "same bytes"); err != nil {
		t.Fatal(err)
	}
	if err := putFile(t, a, "/games/SHARED.BIN", "same bytes"); err != nil {
		t.Fatal(err)
	}
	f, err := a.store.GetFile(a.ctx, "games", "SHARED.BIN")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Filecmd(&sftp.Request{Method: "Remove", Filepath: "/utils/SHARED.BIN"}); err != nil {
		t.Fatal(err)
	}
	if !a.blobs.Has(f.Hash) {
		t.Fatal("deleting one entry destroyed content another area still catalogues")
	}
	got, err := download(t, a, "/games/SHARED.BIN")
	if err != nil {
		t.Fatal(err)
	}
	if got != "same bytes" {
		t.Errorf("the surviving entry reads back as %q", got)
	}
}

func TestRename(t *testing.T) {
	a := testAreaFS(t, "utils", "games")
	if err := putFile(t, a, "/utils/OLD.TXT", "contents"); err != nil {
		t.Fatal(err)
	}

	err := a.Filecmd(&sftp.Request{
		Method: "Rename", Filepath: "/utils/OLD.TXT", Target: "/games/NEW.TXT",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.store.GetFile(a.ctx, "utils", "OLD.TXT"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the old entry survived the rename")
	}
	got, err := download(t, a, "/games/NEW.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if got != "contents" {
		t.Errorf("renamed file reads back as %q", got)
	}
}

// Creating a file area is a sysop decision — a federated one spends the
// network's airtime — so an SFTP client must not be able to make one.
func TestMkdirIsRefused(t *testing.T) {
	a := testAreaFS(t, "utils")
	err := a.Filecmd(&sftp.Request{Method: "Mkdir", Filepath: "/newarea"})
	if err == nil {
		t.Fatal("an SFTP client created a file area")
	}
	if !strings.Contains(err.Error(), "area create") {
		t.Errorf("the refusal does not name the remedy: %v", err)
	}

	areas, err := a.store.ListFileAreas(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(areas) != 1 {
		t.Errorf("there are now %d file areas", len(areas))
	}
}

// Once FILE records replicate, the catalog will name files this node does not
// hold. Over SFTP there is nothing to offer, and saying so beats a truncated
// download.
func TestReadingUnheldContentSaysSo(t *testing.T) {
	a := testAreaFS(t, "utils")
	if _, err := a.store.PutFile(a.ctx, "utils", store.File{
		Name: "ELSEWHERE.ZIP", Hash: blobstore.Hash{0x01, 0x02}, Size: 900,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := a.Fileread(&sftp.Request{Method: "Get", Filepath: "/utils/ELSEWHERE.ZIP"})
	if err == nil {
		t.Fatal("reading unheld content succeeded")
	}
	if !strings.Contains(err.Error(), "not held on this BBS") {
		t.Errorf("error does not explain the situation: %v", err)
	}
}

func TestReadRejectsNonFilePaths(t *testing.T) {
	a := testAreaFS(t, "utils")
	for _, p := range []string{"/", "/utils"} {
		if _, err := a.Fileread(&sftp.Request{Method: "Get", Filepath: p}); err == nil {
			t.Errorf("reading %q as a file succeeded", p)
		}
	}
}
