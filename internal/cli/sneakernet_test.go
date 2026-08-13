package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

// seedFileForCarry puts a real blob and a real file row in an instance, the way
// an SFTP upload would — there is no CLI path that creates one.
func seedFileForCarry(t *testing.T, dir, area, name string, content []byte) blobstore.Hash {
	t.Helper()
	ctx := context.Background()

	bs, err := blobstore.Open(filepath.Join(dir, "files", "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	h, size, err := bs.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, "bbs.db"), clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateFileArea(ctx, area, "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutFile(ctx, area, store.File{
		Name: name, Hash: h, Size: size, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

// The two-trip exchange through the actual commands, ending in convergence
// asserted the way §7.3 defines it: identical vectors, not equal counts.
func TestSneakernetExportImportConverges(t *testing.T) {
	a, b := initInstance(t), initInstance(t)
	for _, dir := range []string{a, b} {
		if _, err := run(t, "--data-dir", dir, "area", "federate", "general"); err != nil {
			t.Fatal(err)
		}
	}
	seed := func(dir, s string) {
		if _, err := run(t, "--data-dir", dir, "dev", "seed", "--seed", s, "--users", "2", "--posts", "4"); err != nil {
			t.Fatal(err)
		}
	}
	seed(a, "1")
	seed(b, "2")

	away := filepath.Join(t.TempDir(), "away.mbx")
	back := filepath.Join(t.TempDir(), "back.mbx")

	if _, err := run(t, "--data-dir", a, "sneakernet", "export", away); err != nil {
		t.Fatalf("export: %v", err)
	}

	// A dry run must apply nothing.
	out, err := run(t, "--data-dir", b, "sneakernet", "import", away, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing was applied") {
		t.Errorf("dry run did not say it was one: %s", out)
	}
	beforeVec, err := run(t, "--data-dir", b, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := run(t, "--data-dir", b, "sneakernet", "import", away); err != nil {
		t.Fatalf("import: %v", err)
	}
	afterVec, err := run(t, "--data-dir", b, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	if beforeVec == afterVec {
		t.Error("the import changed nothing, so the dry run proved nothing either")
	}

	if _, err := run(t, "--data-dir", b, "sneakernet", "export", "--reply-to", away, back); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if _, err := run(t, "--data-dir", a, "sneakernet", "import", back); err != nil {
		t.Fatalf("import reply: %v", err)
	}

	av, err := run(t, "--data-dir", a, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	bv, err := run(t, "--data-dir", b, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	if av != bv {
		t.Errorf("the two boards did not converge:\n--- A ---\n%s\n--- B ---\n%s", av, bv)
	}

	// A further exchange between converged boards carries no records at all.
	third := filepath.Join(t.TempDir(), "third.mbx")
	out, err = run(t, "--data-dir", a, "sneakernet", "export", "--reply-to", back, third)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0 bundle(s)") {
		t.Errorf("a converged pair still packed bundles: %s", out)
	}
}

// --files is the one path §7.5 allows bytes on, and the CLI glue that selects
// and stores them is not covered by the package tests.
func TestSneakernetCarriesFiles(t *testing.T) {
	a, b := initInstance(t), initInstance(t)
	content := []byte("the goods, all of them")
	hash := seedFileForCarry(t, a, "warez", "goods.txt", content)

	stick := filepath.Join(t.TempDir(), "files.mbx")
	out, err := run(t, "--data-dir", a, "sneakernet", "export", "--files", stick)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(out, "1 file(s)") {
		t.Fatalf("the carrier took no files: %s", out)
	}

	if _, err := run(t, "--data-dir", b, "sneakernet", "import", stick); err != nil {
		t.Fatalf("import: %v", err)
	}

	// The bytes arrived, under their own content hash.
	bs, err := blobstore.Open(filepath.Join(b, "files", "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bs.Has(hash) {
		t.Fatal("the file body did not arrive on the other board")
	}
	f, err := bs.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, len(content))
	if _, err := f.Read(got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("arrived as %q", got)
	}

	// Without --files the same board carries records and no bodies.
	plain := filepath.Join(t.TempDir(), "plain.mbx")
	out, err = run(t, "--data-dir", a, "sneakernet", "export", plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "0 file(s)") {
		t.Errorf("files travelled without --files: %s", out)
	}
}

// A mistyped area must be refused rather than quietly omitted: the sysop would
// not find out until the other board did not receive it, a week later and on
// somebody else's desk.
func TestSneakernetAreaFilter(t *testing.T) {
	a := initInstance(t)
	if _, err := run(t, "--data-dir", a, "area", "federate", "general"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "--data-dir", a, "dev", "seed", "--users", "2", "--posts", "4"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "one.mbx")

	if _, err := run(t, "--data-dir", a, "sneakernet", "export", "--area", "geneal", dst); err == nil {
		t.Error("a mistyped area name was accepted")
	}
	// A real but local-only area is refused with its own remedy.
	_, err := run(t, "--data-dir", a, "sneakernet", "export", "--area", "tech", dst)
	if err == nil {
		t.Error("a local-only area was carried")
	} else if !strings.Contains(err.Error(), "local only") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	if _, err := run(t, "--data-dir", a, "sneakernet", "export", "--area", "general", dst); err != nil {
		t.Fatalf("a federated area was refused: %v", err)
	}
}

// A failed export must not leave a file that looks complete on a removable
// drive somebody is about to walk away with.
func TestAFailedExportLeavesNoCarrier(t *testing.T) {
	a := initInstance(t)
	dst := filepath.Join(t.TempDir(), "nested", "deep", "out.mbx")
	if _, err := run(t, "--data-dir", a, "sneakernet", "export", dst); err == nil {
		t.Skip("the destination was writable; nothing to assert")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Error("a carrier was left behind by a failed export")
	}
	if _, err := os.Stat(dst + ".partial"); err == nil {
		t.Error("a .partial file was left behind")
	}
}

// seedPublishedFile puts a real file in a FEDERATED area and announces it.
//
// Different from seedFileForCarry in the one way that matters here: it goes
// through the file service, so a FILE record is minted. That record is what the
// OTHER board's users see, and a request is made against a catalog entry — so
// without it there is nothing to ask for.
func seedPublishedFile(t *testing.T, dir, area, name string, content []byte) blobstore.Hash {
	t.Helper()
	ctx := context.Background()

	bs, err := blobstore.Open(filepath.Join(dir, "files", "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	h, size, err := bs.Put(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, "bbs.db"), clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	key, err := identity.LoadNodeKey(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	// An empty uploader is the CLI's own authority rather than a user's, which
	// is what keeps [N7]'s capability gate out of a fixture.
	svc := bbs.New(st, key, clock.NewReal())
	f, err := svc.AddFile(ctx, area, store.File{Name: name, Hash: h, Size: size})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Published() {
		t.Fatal("the fixture did not announce the file, so there is nothing to request")
	}
	return h
}

// The whole of §6.5's fetch path 2, through the built commands: a board sees a
// file it cannot fetch, asks for it, and two hand-offs later holds the bytes.
func TestSneakernetAnswersARequest(t *testing.T) {
	a, b := initInstance(t), initInstance(t)
	for _, dir := range []string{a, b} {
		if _, err := run(t, "--data-dir", dir, "area", "create", "swap",
			"--kind", "file", "--federated"); err != nil {
			t.Fatal(err)
		}
	}
	content := []byte("the file the mesh will never carry")
	hash := seedPublishedFile(t, b, "swap", "KERMIT.ZIP", content)

	// B's catalog reaches A. No --files: the mesh half of this is a listing,
	// and so is the first stick.
	first := filepath.Join(t.TempDir(), "first.mbx")
	if _, err := run(t, "--data-dir", b, "sneakernet", "export", first); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "--data-dir", a, "sneakernet", "import", first); err != nil {
		t.Fatal(err)
	}

	// A can see it and cannot have it. That is the situation the queue is for.
	out, err := run(t, "--data-dir", a, "file", "request", "swap", "KERMIT.ZIP", "--for", "austin")
	if err != nil {
		t.Fatalf("request: %v (%s)", err, out)
	}
	out, err = run(t, "--data-dir", a, "file", "requests")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "KERMIT.ZIP") || !strings.Contains(out, "waiting") {
		t.Fatalf("the queue does not show the request: %s", out)
	}

	// Trip out: the ask rides the carrier.
	away := filepath.Join(t.TempDir(), "away.mbx")
	out, err = run(t, "--data-dir", a, "sneakernet", "export", away)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "asking for 1 file(s)") {
		t.Fatalf("the carrier did not ask: %s", out)
	}

	out, err = run(t, "--data-dir", b, "sneakernet", "import", away)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "asking for 1 file(s)") {
		t.Errorf("B was not told it had been asked: %s", out)
	}

	// A reply without --files says so rather than quietly answering nothing.
	empty := filepath.Join(t.TempDir(), "empty.mbx")
	out, err = run(t, "--data-dir", b, "sneakernet", "export", "--reply-to", away, empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "re-run with --files") {
		t.Errorf("an unanswered request went unmentioned: %s", out)
	}

	// Trip back, with the bytes.
	back := filepath.Join(t.TempDir(), "back.mbx")
	out, err = run(t, "--data-dir", b, "sneakernet", "export", "--reply-to", away, "--files", back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 file(s)") {
		t.Fatalf("the reply carried no bodies: %s", out)
	}

	out, err = run(t, "--data-dir", a, "sneakernet", "import", back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "answered: swap/KERMIT.ZIP for austin") {
		t.Fatalf("the arrival did not close the request: %s", out)
	}

	// The end of the path: A holds the file and can serve it.
	out, err = run(t, "--data-dir", a, "file", "list", "swap")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "KERMIT.ZIP") {
		t.Fatalf("the arrival is not in A's catalog: %s", out)
	}
	bs, err := blobstore.Open(filepath.Join(a, "files", "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bs.Has(hash) {
		t.Fatal("A has a catalog row for content it does not hold")
	}

	// And nothing asks again.
	out, err = run(t, "--data-dir", a, "sneakernet", "export", filepath.Join(t.TempDir(), "again.mbx"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "asking for") {
		t.Errorf("an answered request is still being asked: %s", out)
	}
}

// A request is precise, and answering it must not turn into a blunt --files
// export that empties the shelf onto somebody's stick.
func TestAnAnsweredCarrierTakesOnlyWhatWasAsked(t *testing.T) {
	a, b := initInstance(t), initInstance(t)
	for _, dir := range []string{a, b} {
		if _, err := run(t, "--data-dir", dir, "area", "create", "swap",
			"--kind", "file", "--federated"); err != nil {
			t.Fatal(err)
		}
	}
	seedPublishedFile(t, b, "swap", "WANTED.ZIP", []byte("this one"))
	seedPublishedFile(t, b, "swap", "SPARE.ZIP", []byte("not this one"))

	first := filepath.Join(t.TempDir(), "first.mbx")
	if _, err := run(t, "--data-dir", b, "sneakernet", "export", first); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "--data-dir", a, "sneakernet", "import", first); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "--data-dir", a, "file", "request", "swap", "WANTED.ZIP"); err != nil {
		t.Fatal(err)
	}

	away := filepath.Join(t.TempDir(), "away.mbx")
	if _, err := run(t, "--data-dir", a, "sneakernet", "export", away); err != nil {
		t.Fatal(err)
	}
	back := filepath.Join(t.TempDir(), "back.mbx")
	out, err := run(t, "--data-dir", b, "sneakernet", "export", "--reply-to", away, "--files", back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1 file(s)") || strings.Contains(out, "2 file(s)") {
		t.Errorf("the reply carried more than was asked for: %s", out)
	}

	out, err = run(t, "--data-dir", a, "sneakernet", "import", back)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "SPARE.ZIP") {
		t.Errorf("a file nobody asked for was filed: %s", out)
	}
	if _, err := os.Stat(filepath.Join(a, "files", "blobs")); err != nil {
		t.Fatal(err)
	}
}

// A request for content this board does not hold is reported rather than
// swallowed: "we sent nothing" and "we do not have it" are different answers.
func TestAnUnanswerableRequestIsReported(t *testing.T) {
	a, b := initInstance(t), initInstance(t)
	if _, err := run(t, "--data-dir", a, "area", "create", "swap",
		"--kind", "file", "--federated"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(a, "bbs.db"), clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	var want [record.FileHashLen]byte
	want[0] = 0xD1
	if _, err := st.RequestFile(ctx, "swap", "GONE.ZIP", want, identity.NodeID{}, "austin"); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()

	away := filepath.Join(t.TempDir(), "away.mbx")
	if _, err := run(t, "--data-dir", a, "sneakernet", "export", away); err != nil {
		t.Fatal(err)
	}
	back := filepath.Join(t.TempDir(), "back.mbx")
	out, err := run(t, "--data-dir", b, "sneakernet", "export", "--reply-to", away, "--files", back)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cannot answer their request") {
		t.Errorf("B answered nothing and said nothing: %s", out)
	}
}
