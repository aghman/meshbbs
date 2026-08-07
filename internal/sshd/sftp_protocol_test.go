package sshd

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sort"
	"testing"

	"github.com/aghman/meshbbs/internal/store"
	"github.com/pkg/sftp"
)

// These tests drive the handler through a real SFTP client over a real
// protocol, rather than calling its methods directly.
//
// The difference matters. Whether the server closes a WriterAt when the client
// closes a handle, whether it asks for Stat or Lstat, and how an error becomes
// a status code are all decided inside pkg/sftp — so a handler can satisfy
// every interface, pass every direct test, and still be unusable from `sftp`.

func testSFTPClient(t *testing.T, areas ...string) (*sftp.Client, *areaFS) {
	t.Helper()
	a := testAreaFS(t, areas...)

	serverConn, clientConn := net.Pipe()
	srv := sftp.NewRequestServer(serverConn, sftp.Handlers{
		FileGet: a, FilePut: a, FileCmd: a, FileList: a,
	})
	go func() { _ = srv.Serve() }()

	client, err := sftp.NewClientPipe(clientConn, clientConn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.Close()
		srv.Close()
	})
	return client, a
}

func TestProtocolUploadAndDownload(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	const content = "PKZIP204G.EXE\r\nnot really a zip\r\n"

	w, err := client.Create("/utils/PKZIP.EXE")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(w, bytes.NewReader([]byte(content))); err != nil {
		t.Fatal(err)
	}
	// Closing the handle is what finalizes the upload. If the server does not
	// close our WriterAt, nothing below this line works.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := a.store.GetFile(a.ctx, "utils", "PKZIP.EXE")
	if err != nil {
		t.Fatalf("upload left no catalog entry: %v", err)
	}
	if f.Size != int64(len(content)) {
		t.Errorf("catalogued size is %d, want %d", f.Size, len(content))
	}
	if f.Uploader != "austin" {
		t.Errorf("uploader is %q, want austin", f.Uploader)
	}

	r, err := client.Open("/utils/PKZIP.EXE")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("downloaded %q, want %q", got, content)
	}
}

// A file large enough that the client splits it into several packets, so the
// staging path is exercised the way a real transfer exercises it.
func TestProtocolLargeUpload(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	content := bytes.Repeat([]byte("meshbbs "), 100_000) // 800 KB

	w, err := client.Create("/utils/BIG.DAT")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := a.store.GetFile(a.ctx, "utils", "BIG.DAT")
	if err != nil {
		t.Fatal(err)
	}
	if f.Size != int64(len(content)) {
		t.Fatalf("catalogued size is %d, want %d", f.Size, len(content))
	}
	// The blob's name must describe its bytes, whatever order they arrived in.
	if err := a.blobs.Verify(f.Hash); err != nil {
		t.Error(err)
	}
}

func TestProtocolListing(t *testing.T) {
	client, a := testSFTPClient(t, "utils", "games")
	if err := putFile(t, a, "/utils/A.TXT", "a"); err != nil {
		t.Fatal(err)
	}
	if err := putFile(t, a, "/utils/B.TXT", "bb"); err != nil {
		t.Fatal(err)
	}

	root, err := client.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	got := names(root)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "games" || got[1] != "utils" {
		t.Errorf("root listed %v, want [games utils]", got)
	}
	for _, info := range root {
		if !info.IsDir() {
			t.Errorf("%s did not arrive as a directory", info.Name())
		}
	}

	area, err := client.ReadDir("/utils")
	if err != nil {
		t.Fatal(err)
	}
	if len(area) != 2 {
		t.Fatalf("utils listed %v", names(area))
	}
	for _, info := range area {
		if info.IsDir() {
			t.Errorf("%s arrived as a directory", info.Name())
		}
		if info.Name() == "B.TXT" && info.Size() != 2 {
			t.Errorf("B.TXT arrived with size %d, want 2", info.Size())
		}
	}
}

// `sftp` calls Lstat before most operations. If it does not resolve, a client
// reports "no such file" for a file that is right there.
func TestProtocolStat(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	if err := putFile(t, a, "/utils/STAT.TXT", "twelve bytes"); err != nil {
		t.Fatal(err)
	}

	info, err := client.Stat("/utils/STAT.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 12 || info.IsDir() {
		t.Errorf("stat returned %+v", info)
	}

	if info, err = client.Lstat("/utils"); err != nil {
		t.Fatalf("lstat of an area: %v", err)
	}
	if !info.IsDir() {
		t.Error("an area did not lstat as a directory")
	}

	// The status code matters: a client prints something usable for
	// SSH_FX_NO_SUCH_FILE and something confusing for a generic failure.
	if _, err := client.Stat("/utils/ABSENT.TXT"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of a missing file gave %v, want os.ErrNotExist", err)
	}
	if _, err := client.Stat("/nosucharea"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat of a missing area gave %v, want os.ErrNotExist", err)
	}
}

func TestProtocolRemove(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	if err := putFile(t, a, "/utils/GONE.TXT", "delete me"); err != nil {
		t.Fatal(err)
	}
	f, err := a.store.GetFile(a.ctx, "utils", "GONE.TXT")
	if err != nil {
		t.Fatal(err)
	}

	if err := client.Remove("/utils/GONE.TXT"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.GetFile(a.ctx, "utils", "GONE.TXT"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the catalog entry survived a remove over the wire")
	}
	if a.blobs.Has(f.Hash) {
		t.Error("the bytes survived the last reference to them")
	}
}

func TestProtocolMkdirRefused(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	if err := client.Mkdir("/newarea"); err == nil {
		t.Fatal("an SFTP client created a file area over the wire")
	}
	areas, err := a.store.ListFileAreas(a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(areas) != 1 {
		t.Errorf("there are now %d file areas", len(areas))
	}
}

// A read-only account gets a refusal at the protocol level, not a broken
// transfer that fails later.
func TestProtocolUploadRefusedWithoutCapability(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	a.readOnly = true

	w, err := client.Create("/utils/NOPE.TXT")
	if err == nil {
		w.Close()
		t.Fatal("a read-only session opened a file for writing")
	}

	files, err := a.store.ListFiles(a.ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("area holds %d files after a refused upload", len(files))
	}
}

func TestProtocolWalkStaysInsideTheProjection(t *testing.T) {
	client, a := testSFTPClient(t, "utils")
	if err := putFile(t, a, "/utils/ONLY.TXT", "x"); err != nil {
		t.Fatal(err)
	}

	// A client walking the tree must never see anything but areas and files —
	// in particular not the blob store's own directories, which live on the
	// real filesystem the projection does not expose.
	var seen []string
	walker := client.Walk("/")
	for walker.Step() {
		if err := walker.Err(); err != nil {
			continue
		}
		seen = append(seen, walker.Path())
	}
	sort.Strings(seen)

	want := []string{"/", "/utils", "/utils/ONLY.TXT"}
	if len(seen) != len(want) {
		t.Fatalf("walk saw %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("walk saw %v, want %v", seen, want)
			break
		}
	}
}

var _ = os.FileInfo(nil)
