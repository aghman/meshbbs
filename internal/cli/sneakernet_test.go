package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/clock"
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
