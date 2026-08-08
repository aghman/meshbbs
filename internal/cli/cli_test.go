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
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

// run executes the root command with args, returning stdout+stderr.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func initInstance(t *testing.T, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	args := append([]string{"--data-dir", dir, "init", "--development"}, extra...)
	if _, err := run(t, args...); err != nil {
		t.Fatalf("init: %v", err)
	}
	return dir
}

// seedFile puts one file in a new file area, the way an SFTP upload would:
// with an empty description, because SFTP has no field to carry one.
func seedFile(t *testing.T, dir, area, name string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "bbs.db"), clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.CreateFileArea(ctx, area, "", false); err != nil {
		t.Fatal(err)
	}
	var h blobstore.Hash
	for i := range h {
		h[i] = 0x11
	}
	if _, err := st.PutFile(ctx, area, store.File{
		Name: name, Hash: h, Size: 4096, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}
}

func fileDescription(t *testing.T, dir, area, name string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "bbs.db"), clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	f, err := st.GetFile(ctx, area, name)
	if err != nil {
		t.Fatal(err)
	}
	return f.Description
}

func TestFileDescribeSetsAndClears(t *testing.T) {
	dir := initInstance(t)
	seedFile(t, dir, "utils", "ARCHIVE.ZIP")

	if got := fileDescription(t, dir, "utils", "ARCHIVE.ZIP"); got != "" {
		t.Fatalf("a seeded upload has description %q; this test cannot show the gap", got)
	}

	out, err := run(t, "--data-dir", dir, "file", "describe", "utils", "ARCHIVE.ZIP",
		"Tools for the repeater")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Tools for the repeater") {
		t.Errorf("describe did not echo the new description:\n%s", out)
	}
	if got := fileDescription(t, dir, "utils", "ARCHIVE.ZIP"); got != "Tools for the repeater" {
		t.Errorf("stored description = %q, want the text just set", got)
	}

	// It shows up in the listing, which is the point of setting it.
	out, err = run(t, "--data-dir", dir, "file", "list", "utils")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Tools for the repeater") {
		t.Errorf("file list does not show the description:\n%s", out)
	}

	// An empty string clears it rather than erroring.
	if _, err := run(t, "--data-dir", dir, "file", "describe", "utils", "ARCHIVE.ZIP", ""); err != nil {
		t.Fatal(err)
	}
	if got := fileDescription(t, dir, "utils", "ARCHIVE.ZIP"); got != "" {
		t.Errorf("description = %q after clearing, want empty", got)
	}
}

// The limit belongs to the wire (§6.5). Accepting more here would create a file
// that exists locally and silently never publishes.
func TestFileDescribeRefusesOverlongText(t *testing.T) {
	dir := initInstance(t)
	seedFile(t, dir, "utils", "ARCHIVE.ZIP")

	long := strings.Repeat("x", record.MaxFileDescLen+1)
	if _, err := run(t, "--data-dir", dir, "file", "describe", "utils", "ARCHIVE.ZIP", long); err == nil {
		t.Error("describe accepted a description longer than a FILE record can carry")
	}
	if got := fileDescription(t, dir, "utils", "ARCHIVE.ZIP"); got != "" {
		t.Errorf("description = %q after a refused set, want it unchanged", got)
	}
}

func TestFileDescribeUnknownFile(t *testing.T) {
	dir := initInstance(t)
	seedFile(t, dir, "utils", "ARCHIVE.ZIP")

	if _, err := run(t, "--data-dir", dir, "file", "describe", "utils", "ABSENT.TXT", "x"); err == nil {
		t.Error("describe succeeded for a file that does not exist")
	}
	if _, err := run(t, "--data-dir", dir, "file", "describe", "nosucharea", "ARCHIVE.ZIP", "x"); err == nil {
		t.Error("describe succeeded for an area that does not exist")
	}
}

func TestInitCreatesEverything(t *testing.T) {
	dir := initInstance(t, "--display-name", "pnw-bbs", "--sysop-nick", "austin")

	for _, want := range []string{"config.toml", "bbs.db", filepath.Join("keys", "node.ed25519")} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("init did not create %s: %v", want, err)
		}
	}

	out, err := run(t, "--data-dir", dir, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "austin") {
		t.Fatalf("sysop account missing from user list:\n%s", out)
	}
}

// init is not re-runnable, so a bad argument must abort before anything is
// written — otherwise the operator is stranded with a half-built instance.
func TestInitValidatesBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "--data-dir", dir, "init", "--sysop-nick", "a"); err == nil {
		t.Fatal("init accepted a one-character sysop nick")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("init wrote %v before failing; a failed init must leave nothing behind", names)
	}

	// And a corrected init must still work.
	if _, err := run(t, "--data-dir", dir, "init", "--sysop-nick", "austin"); err != nil {
		t.Fatalf("init failed after a corrected argument: %v", err)
	}
}

func TestInitRefusesToClobberAnExistingKey(t *testing.T) {
	dir := initInstance(t)
	if _, err := run(t, "--data-dir", dir, "init"); err == nil {
		t.Fatal("a second init overwrote the existing node key")
	}
}

// Both renderings must appear, and must round-trip through `peer resolve`.
func TestIDRenderingsRoundTripThroughCLI(t *testing.T) {
	dir := initInstance(t)

	compact, err := run(t, "--data-dir", dir, "id", "--compact")
	if err != nil {
		t.Fatal(err)
	}
	words, err := run(t, "--data-dir", dir, "id", "--words")
	if err != nil {
		t.Fatal(err)
	}
	compact, words = strings.TrimSpace(compact), strings.TrimSpace(words)

	if len(compact) != 13 {
		t.Fatalf("compact ID is %d characters, want 13: %q", len(compact), compact)
	}
	if n := len(strings.Split(words, "-")); n != 6 {
		t.Fatalf("word form has %d words, want 6: %q", n, words)
	}

	// Aliasing by the word form must produce the same ID as the base32 form —
	// they are two renderings of one identifier (§6.1.4.2).
	if _, err := run(t, "--data-dir", dir, "peer", "alias", "byword", words); err != nil {
		t.Fatalf("alias by word form: %v", err)
	}
	resolved, err := run(t, "--data-dir", dir, "peer", "resolve", "byword")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resolved, compact[:4]) {
		t.Fatalf("word-form alias resolved to a different ID:\n%s\nwant %s", resolved, compact)
	}
}

// §11.3: an unknown key is a hard error, and the message must point at the
// reference so the sysop can find the right spelling.
func TestUnknownConfigKeyFailsCheck(t *testing.T) {
	dir := initInstance(t)
	path := filepath.Join(dir, "config.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The generated config ends inside [log], so a bare key lands there —
	// appending another [log] header would be a duplicate-section error and
	// would test the wrong thing.
	if err := os.WriteFile(path, append(body, []byte("levle = \"debug\"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "--data-dir", dir, "config", "check")
	if err == nil {
		t.Fatal("config check passed with an unknown key")
	}
	combined := out + err.Error()
	if !strings.Contains(combined, "levle") {
		t.Errorf("error does not name the offending key:\n%s", combined)
	}
	if !strings.Contains(combined, "config reference") {
		t.Errorf("error does not point at `config reference`:\n%s", combined)
	}
}

// §6.7: dev commands exist in every binary but refuse a production datadir.
func TestDevCommandsRefuseProduction(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "--data-dir", dir, "init", "--sysop-nick", "austin"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "--data-dir", dir, "dev", "seed", "--users", "1")
	if err == nil {
		t.Fatal("dev seed ran against a production datadir")
	}
	combined := out + err.Error()
	if !strings.Contains(combined, "development") {
		t.Errorf("refusal does not explain the remedy:\n%s", combined)
	}
}

// Determinism is the whole point of dev seed: the same seed must produce the
// same accounts on independent instances, which is what makes reproducible
// federation tests possible.
func TestDevSeedIsDeterministic(t *testing.T) {
	a := initInstance(t)
	b := initInstance(t)

	for _, dir := range []string{a, b} {
		if _, err := run(t, "--data-dir", dir, "dev", "seed",
			"--seed", "7", "--users", "8", "--posts", "5"); err != nil {
			t.Fatal(err)
		}
	}

	outA, err := run(t, "--data-dir", a, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	outB, err := run(t, "--data-dir", b, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	if outA != outB {
		t.Fatalf("the same seed produced different accounts:\n--- a ---\n%s\n--- b ---\n%s", outA, outB)
	}

	// A different seed must produce something different, or the seed is being
	// ignored and the test above proves nothing.
	c := initInstance(t)
	if _, err := run(t, "--data-dir", c, "dev", "seed",
		"--seed", "8", "--users", "8", "--posts", "5"); err != nil {
		t.Fatal(err)
	}
	outC, err := run(t, "--data-dir", c, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	if outA == outC {
		t.Fatal("different seeds produced identical accounts; --seed is being ignored")
	}
}

// [N7]: new accounts must not be able to spend the network's airtime, and the
// CLI should say so rather than leaving the operator to discover it.
func TestUserAddWithholdsFederatedPosting(t *testing.T) {
	dir := initInstance(t)

	out, err := run(t, "--data-dir", dir, "user", "add", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "post_federated,") || strings.Contains(out, ", post_federated") {
		t.Fatalf("new account was granted post_federated:\n%s", out)
	}
	if !strings.Contains(out, "meshbbs user grant bob post_federated") {
		t.Errorf("output does not tell the sysop how to grant federated posting:\n%s", out)
	}

	if _, err := run(t, "--data-dir", dir, "user", "grant", "bob", "post_federated"); err != nil {
		t.Fatal(err)
	}
	shown, err := run(t, "--data-dir", dir, "user", "show", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shown, "post_federated") {
		t.Fatalf("grant did not take effect:\n%s", shown)
	}
}

// §11.2: the reference is generated, so it must cover every key with docs.
func TestConfigReferenceIsComplete(t *testing.T) {
	dir := initInstance(t)
	out, err := run(t, "--data-dir", dir, "config", "reference")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"node.display_name", "node.environment", "log.level",
		"log.format", "storage.data_dir", "storage.database", "storage.keys_dir",
	} {
		if !strings.Contains(out, key) {
			t.Errorf("config reference omits %s", key)
		}
	}
	if !strings.Contains(out, "MESHBBS_LOG_LEVEL") {
		t.Error("config reference omits environment variable names")
	}
}

// The binary must be usable before init has run, so that `--help` and
// `config reference` work on a fresh checkout.
func TestWorksWithNoConfigFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := run(t, "--data-dir", dir, "config", "reference"); err != nil {
		t.Fatalf("config reference failed with no config file: %v", err)
	}
	out, err := run(t, "--data-dir", dir, "config", "check")
	if err != nil {
		t.Fatalf("config check failed with no config file: %v", err)
	}
	if !strings.Contains(out, "defaults") {
		t.Errorf("check should say defaults are in use:\n%s", out)
	}
}
