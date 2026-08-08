package tui

import (
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	tea "github.com/charmbracelet/bubbletea"
)

// seedFiles gives the fixture a file area with some contents.
func seedFiles(t *testing.T, f *fixture) {
	t.Helper()
	if _, err := f.store.CreateFileArea(f.ctx, "utils", "Utilities and tools", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CreateFileArea(f.ctx, "meshwide", "Shared with peers", true); err != nil {
		t.Fatal(err)
	}

	// Distinct upload times, deliberately NOT in alphabetical order: the list
	// is ordered by when a file arrived, and identical timestamps would let it
	// fall back to the name and pass without testing anything.
	files := []struct {
		name, desc, uploader string
		size                 int64
		hash                 byte
		at                   int64
	}{
		{"ARCHIVE.ZIP", "A compressed archive", "austin", 4096, 0x11, 1_700_000_000},
		{"README.TXT", "Notes about this area", "bob", 812, 0x22, 1_700_000_100},
		{"BIG.IMG", "A disk image", "austin", 4_404_019, 0x33, 1_700_000_200},
	}
	for _, spec := range files {
		var h blobstore.Hash
		for i := range h {
			h[i] = spec.hash
		}
		if _, err := f.store.PutFile(f.ctx, "utils", store.File{
			Name: spec.name, Hash: h, Size: spec.size,
			Description: spec.desc, Uploader: spec.uploader,
			UploadedAt: spec.at,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFileBrowserListsAreas(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	s := f.login(t, "austin").typeRunes("f")
	s.contains("File Areas", "utils", "meshwide", "Utilities and tools")

	// A federated file area must say what that means, and what it does NOT
	// mean: the catalog travels, the files never do (§7.5).
	s.containsProse("The files themselves never do, at any size.")
}

// The message areas and the file areas are separate listings. Neither may show
// the other's, or a user posts into a catalog.
func TestFileBrowserDoesNotShowMessageAreas(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	f.login(t, "austin").typeRunes("f").notContains("general", "sysop")
	f.login(t, "austin").typeRunes("m").notContains("utils", "meshwide")
}

func TestFileBrowserListsFiles(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	// Areas list alphabetically, so "meshwide" is first and "utils" is second.
	s := f.login(t, "austin").typeRunes("f").typeRunes("j").enter()
	s.contains("Files in utils", "ARCHIVE.ZIP", "README.TXT", "BIG.IMG")
	// Sizes are rounded for the listing, which is what a glance wants.
	s.contains("4.0 KB", "812 B", "4.2 MB")
	// The column names the BBS holding each file, not the person who uploaded
	// it: a FILE record carries no uploader, so a node is all the network knows.
	// The uploader is still on the detail screen, where it is knowable.
	s.contains("Held by", "here")
}

func TestFileBrowserShowsFetchCommand(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	cfg := f.config(IntentAuthenticated, "austin")
	cfg.SSHPort = 2222
	u, err := f.store.GetUser(f.ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	cfg.User = u

	s := newSession(t, cfg).typeRunes("f").typeRunes("j").enter().enter()
	// The exact byte count belongs here, where someone verifying a transfer
	// will look, alongside the rounded figure.
	s.contains("ARCHIVE.ZIP", "4096 bytes", "A compressed archive")
	// Files move over SFTP and nowhere else (§5.1), so the browser has to say
	// how — with the port this instance actually listens on.
	s.contains("sftp -P 2222 austin@this-bbs", "get /utils/ARCHIVE.ZIP")
}

// Port 22 is the default, so naming it would be noise in the command someone
// is about to copy.
func TestFetchCommandOmitsTheDefaultPort(t *testing.T) {
	f := newFixture(t)
	m := New(f.config(IntentAuthenticated, "austin"))
	m.cfg.SSHPort = 22
	m.nick = "austin"

	if got := m.fetchCommand("utils", "A.TXT"); got != "sftp austin@this-bbs  then:  get /utils/A.TXT" {
		t.Errorf("fetch command is %q", got)
	}
}

func TestFileBrowserNavigatesBack(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	s := f.login(t, "austin").typeRunes("f").typeRunes("j").enter()
	s.contains("Files in utils")

	s.enter().contains("ARCHIVE.ZIP")            // detail
	s.typeRunes("q").contains("Files in utils")  // back to the list
	s.typeRunes("q").contains("File Areas")      // back to the areas
	s.typeRunes("q").contains("Message areas —") // back to the menu
}

func TestFileBrowserMovesTheCursor(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	s := f.login(t, "austin").typeRunes("f").typeRunes("j").enter()
	s.typeRunes("j").enter()
	// Row two in UPLOAD order, which is not row two alphabetically — BIG.IMG
	// would be if the list had fallen back to sorting by name.
	s.contains("README.TXT", "Notes about this area")
	s.notContains("A disk image")
}

// The listing is in the order files arrived, oldest first.
func TestFileListIsInUploadOrder(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	view := f.login(t, "austin").typeRunes("f").typeRunes("j").enter().view()
	first := strings.Index(view, "ARCHIVE.ZIP")
	second := strings.Index(view, "README.TXT")
	third := strings.Index(view, "BIG.IMG")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("not all files are listed:\n%s", view)
	}
	if !(first < second && second < third) {
		t.Errorf("files are not in upload order:\n%s", view)
	}
}

func TestEmptyFileAreaSaysSo(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.CreateFileArea(f.ctx, "empty", "Nothing here", false); err != nil {
		t.Fatal(err)
	}
	f.user(t, "austin", "")

	f.login(t, "austin").typeRunes("f").enter().contains("This area has no files yet.")
}

// A BBS with no file areas at all must point at the remedy rather than showing
// an empty box.
func TestNoFileAreasNamesTheRemedy(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")

	f.login(t, "austin").typeRunes("f").
		contains("No file areas yet.").
		containsProse("The sysop makes one with `meshbbs area create").
		containsProse("upload_files capability")
}

// Enter on an empty area must not index off the end of the slice.
func TestEnterOnEmptyFileAreaIsSafe(t *testing.T) {
	f := newFixture(t)
	f.user(t, "austin", "")

	// No file areas at all: enter on the area list, then again.
	f.login(t, "austin").typeRunes("f").enter().enter().contains("File Areas")
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{4096, "4.0 KB"},
		{4_404_019, "4.2 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Guests browse the file areas read-only, the same way they browse forums.
func TestGuestCanBrowseFiles(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)

	s := newSession(t, f.config(IntentGuest, "guest")).typeRunes("f")
	s.contains("File Areas", "utils")
	s.typeRunes("j").enter().contains("ARCHIVE.ZIP")
}

// "f" used to be an undocumented alias for the message areas. It now means
// files, and "m" — the binding that was always on screen — still means messages.
func TestFKeyOpensFilesAndMStillOpensMessages(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	f.login(t, "austin").typeRunes("f").contains("File Areas")
	f.login(t, "austin").typeRunes("m").contains("Message Areas")
}

// seedPeerFile announces a file from another BBS into a federated area, the way
// anti-entropy would.
func seedPeerFile(t *testing.T, f *fixture, area, name string, size uint64, alias, contact string) identity.NodeID {
	t.Helper()
	a, err := f.store.GetFileArea(f.ctx, area)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := identity.GenerateNodeKey(rng.TestSecret(42))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutNode(f.ctx, store.Node{
		ID: peer.ID(), PublicKey: peer.Public,
		DisplayName: "Pacific NW", SysopContact: contact,
	}); err != nil {
		t.Fatal(err)
	}
	if alias != "" {
		if err := f.store.SetAlias(f.ctx, alias, peer.ID()); err != nil {
			t.Fatal(err)
		}
	}

	var full blobstore.Hash
	full[0], full[1] = 0xAB, 0xCD
	wire, err := record.TruncateFileHash(full[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_300, a.Tag, record.FileBody{
		Name: name, Size: size, Hash: wire, Description: "Held over there",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.PutRecord(f.ctx, rec); err != nil {
		t.Fatal(err)
	}
	return peer.ID()
}

// The catalog shows the whole network's files, not just ours. That is the
// entire point of replicating it (§6.5).
func TestFileBrowserShowsPeerFiles(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")
	seedPeerFile(t, f, "meshwide", "REMOTE.ZIP", 900_000, "pnw", "sysop@pnw.example")

	// "meshwide" is the first area alphabetically.
	s := f.login(t, "austin").typeRunes("f").enter()
	s.contains("REMOTE.ZIP", "878.9 KB", "pnw")
	// Said once for the listing rather than repeated on every row.
	s.containsProse("File listings travel the mesh; the files themselves never do")
}

// A file we do not hold must say so plainly, and must NOT promise a fetch that
// does not exist.
func TestPeerFileDetailIsHonest(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")
	seedPeerFile(t, f, "meshwide", "REMOTE.ZIP", 900_000, "pnw", "sysop@pnw.example")

	s := f.login(t, "austin").typeRunes("f").enter().enter()
	s.contains("REMOTE.ZIP", "Held by pnw")
	s.containsProse("This BBS does not have the file, only its listing")
	s.contains("sysop@pnw.example")

	// No download instructions, because there is no download. And no promise of
	// a sneakernet queue: that is Phase 5, nothing records a request today, and
	// a promise nothing will keep is worse than saying so.
	s.notContains("To download it:", "sftp ")
	s.notContains("queued", "queue")
}

// Our own file still offers the fetch command, so the honest-about-remote path
// has not made the local one unhelpful.
func TestHeldFileStillShowsTheFetchCommand(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	cfg := f.config(IntentAuthenticated, "austin")
	cfg.SSHPort = 2222
	u, err := f.store.GetUser(f.ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	cfg.User = u

	s := newSession(t, cfg).typeRunes("f").typeRunes("j").enter().enter()
	s.contains("To download it:", "sftp -P 2222 austin@this-bbs", "get /utils/ARCHIVE.ZIP")
	s.notContains("Held by pnw", "does not have the file")
}

// With no petname and no display name, the short ID is the fallback that always
// exists — a holder column can never be blank.
func TestPeerFileWithoutAPetnameShowsTheID(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")
	id := seedPeerFile(t, f, "meshwide", "ANON.ZIP", 100, "", "")

	s := f.login(t, "austin").typeRunes("f").enter()
	// No alias was set, so the node's own display name is what shows.
	s.contains("Pacific NW")

	s.enter().containsProse("Ask your sysop if you need it")
	if id.IsZero() {
		t.Fatal("the peer has no ID")
	}
}

// A local-only area has no records at all, so a listing built from the log
// alone would show an empty area that visibly has files in it.
func TestLocalOnlyAreaStillListsItsFiles(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	// "utils" is local-only and holds three files.
	s := f.login(t, "austin").typeRunes("f").typeRunes("j").enter()
	s.contains("ARCHIVE.ZIP", "README.TXT", "BIG.IMG")
	// Nothing here is elsewhere, so the mesh caveat is not shown.
	s.notContains("File listings travel the mesh")
}

// openFiles walks to the file listing for "utils", the second area
// alphabetically. The listing is ordered by upload time, so row 0 is
// ARCHIVE.ZIP (austin's), row 1 is README.TXT (bob's).
func openFiles(t *testing.T, f *fixture, nick string) *session {
	t.Helper()
	return f.login(t, nick).typeRunes("f").typeRunes("j").enter()
}

// The gap this closes: SFTP cannot carry a description, so before this every
// real upload had one that was empty and no way to change it.
func TestDescribeOwnUpload(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	s := openFiles(t, f, "austin")
	s.contains("d describe")

	s.typeRunes("d").contains("Describe ARCHIVE.ZIP")
	// The current text is loaded for editing rather than cleared, so fixing a
	// word does not mean retyping the sentence.
	s.contains("A compressed archive")

	s.press(tea.KeyCtrlU).line("Now a better description")
	s.contains("Now a better description")

	got, err := f.store.GetFile(f.ctx, "utils", "ARCHIVE.ZIP")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "Now a better description" {
		t.Errorf("stored description = %q, want %q", got.Description, "Now a better description")
	}
}

// The rule is own uploads or sysop. bob's file is not austin's to describe.
func TestCannotDescribeSomeoneElsesUpload(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	// Move to README.TXT, which bob uploaded.
	s := openFiles(t, f, "austin").typeRunes("j")
	if strings.Contains(s.view(), "d describe") {
		t.Error("the describe hint is shown on a file the user cannot describe")
	}

	// The hint being absent is not the check. A browser sends the same key
	// names an SSH user can type, so "d" arrives regardless.
	s.typeRunes("d")
	s.contains("You can only describe files you uploaded")

	got, err := f.store.GetFile(f.ctx, "utils", "README.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "Notes about this area" {
		t.Errorf("description changed to %q despite the refusal", got.Description)
	}
}

func TestSysopCanDescribeAnyUpload(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	if _, err := f.store.CreateUser(f.ctx, store.CreateUserOptions{
		Nick: "carol", CanLogin: true, IsSysop: true,
		Capabilities: store.DefaultCapabilities,
	}); err != nil {
		t.Fatal(err)
	}

	// README.TXT belongs to bob, and carol uploaded nothing at all.
	s := openFiles(t, f, "carol").typeRunes("j")
	s.contains("d describe")
	s.typeRunes("d").contains("Describe README.TXT")
	s.press(tea.KeyCtrlU).line("Sysop rewrote this")

	got, err := f.store.GetFile(f.ctx, "utils", "README.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "Sysop rewrote this" {
		t.Errorf("stored description = %q, want the sysop's text", got.Description)
	}
}

func TestDescribeCanBeCancelled(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	s := openFiles(t, f, "austin")
	s.typeRunes("d").press(tea.KeyCtrlU).typeRunes("discard me").escape()

	got, err := f.store.GetFile(f.ctx, "utils", "ARCHIVE.ZIP")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "A compressed archive" {
		t.Errorf("escape saved %q; it must discard the edit", got.Description)
	}
}

// An empty description is how you clear one, not an error.
func TestDescribeWithEmptyTextClears(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	openFiles(t, f, "austin").typeRunes("d").press(tea.KeyCtrlU).enter()

	got, err := f.store.GetFile(f.ctx, "utils", "ARCHIVE.ZIP")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "" {
		t.Errorf("description = %q after clearing, want empty", got.Description)
	}
}

// The detail screen returns to the listing on any key, which is why "d" needed
// a real handler there rather than falling through to that.
func TestDescribeFromTheDetailScreen(t *testing.T) {
	f := newFixture(t)
	seedFiles(t, f)
	f.user(t, "austin", "")

	s := openFiles(t, f, "austin").enter()
	s.contains("d describe")
	s.typeRunes("d").contains("Describe ARCHIVE.ZIP")
}
