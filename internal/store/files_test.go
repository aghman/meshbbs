package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/record"
)

func fileHashOf(b byte) blobstore.Hash {
	var h blobstore.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

func TestPutAndListFiles(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "Utilities", false); err != nil {
		t.Fatal(err)
	}

	saved, err := s.PutFile(ctx, "utils", File{
		Name: "ARCHIVE.ZIP", Hash: fileHashOf(0x11), Size: 4096,
		Description: "an archive", Uploader: "austin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == 0 {
		t.Error("PutFile returned no row ID")
	}
	if saved.Area != "utils" {
		t.Errorf("area is %q, want utils", saved.Area)
	}
	if saved.Published() {
		t.Error("a file in a local-only area should not claim to be published")
	}

	files, err := s.ListFiles(ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("listed %d files, want 1", len(files))
	}
	got := files[0]
	if got.Name != "ARCHIVE.ZIP" || got.Size != 4096 || got.Uploader != "austin" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if got.Hash != fileHashOf(0x11) {
		t.Errorf("hash came back as %s", got.Hash)
	}
}

// §6.5's dedup: the same content in two areas is one blob and two entries.
func TestSameContentInTwoAreasSharesOneBlob(t *testing.T) {
	s, ctx := testStore(t)
	for _, name := range []string{"utils", "games"} {
		if _, err := s.CreateFileArea(ctx, name, "", false); err != nil {
			t.Fatal(err)
		}
	}

	h := fileHashOf(0x22)
	for _, area := range []string{"utils", "games"} {
		if _, err := s.PutFile(ctx, area, File{Name: "SHARED.DAT", Hash: h, Size: 12}); err != nil {
			t.Fatalf("put in %s: %v", area, err)
		}
	}

	var blobs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 1 {
		t.Errorf("two areas holding identical content left %d blob rows, want 1", blobs)
	}
}

func TestDuplicateNameInAreaIsRefused(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFile(ctx, "utils", File{Name: "DUP.TXT", Hash: fileHashOf(1), Size: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := s.PutFile(ctx, "utils", File{Name: "DUP.TXT", Hash: fileHashOf(2), Size: 1})
	if !errors.Is(err, ErrFileExists) {
		t.Errorf("second upload under the same name returned %v, want ErrFileExists", err)
	}
}

// The reason file areas share the `areas` table: a name collision between the
// two kinds would be an AreaTag collision, so it has to be refused at creation.
func TestFileAreaCannotShareANameWithAMessageArea(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateArea(ctx, "general", "messages", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileArea(ctx, "general", "files", false); !errors.Is(err, ErrAreaExists) {
		t.Errorf("creating a file area named after a message area returned %v, want ErrAreaExists", err)
	}
}

// The reserved well-known tags guard file areas too.
//
// This is an interaction between two changes that landed separately: the
// reserved-tag rule was written for message areas, and file areas go through
// the same constructor. It matters most for mail — a federated file area
// sharing record.DMArea's tag would put private mail on the wire, which is the
// exact leak the reservation exists to prevent — and nothing else asserts that
// the rule reaches this constructor.
func TestFileAreasCannotClaimAReservedTag(t *testing.T) {
	s, ctx := testStore(t)
	for _, name := range []string{"_mail", "_directory"} {
		if _, err := s.CreateFileArea(ctx, name, "", true); err == nil {
			t.Errorf("a file area named %q was created over a reserved tag", name)
		}
	}
}

// Posting into a catalog, or cataloguing into a message base, must not be
// reachable by naming the wrong area.
func TestAreaLookupsAreKindScoped(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateArea(ctx, "general", "messages", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileArea(ctx, "utils", "files", false); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetFileArea(ctx, "general"); !errors.Is(err, ErrWrongAreaKind) {
		t.Errorf("GetFileArea on a message area returned %v, want ErrWrongAreaKind", err)
	}
	if _, err := s.GetArea(ctx, "utils"); !errors.Is(err, ErrWrongAreaKind) {
		t.Errorf("GetArea on a file area returned %v, want ErrWrongAreaKind", err)
	}
	if _, err := s.GetAnyArea(ctx, "utils"); err != nil {
		t.Errorf("GetAnyArea on a file area returned %v", err)
	}
	if _, err := s.GetAnyArea(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetAnyArea on a missing area returned %v, want ErrNotFound", err)
	}
}

// The forum UI lists message bases and the file browser lists file areas;
// neither may show the other's.
func TestListingsAreSeparated(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateArea(ctx, "general", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.ListAreas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Name != "general" {
		t.Errorf("ListAreas returned %+v, want just general", msgs)
	}
	if msgs[0].Kind != KindMessage {
		t.Errorf("message area has kind %q", msgs[0].Kind)
	}

	files, err := s.ListFileAreas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "utils" {
		t.Errorf("ListFileAreas returned %+v, want just utils", files)
	}
	if files[0].Kind != KindFile {
		t.Errorf("file area has kind %q", files[0].Kind)
	}
}

// Areas that predate migration 0005 must still be message areas.
func TestExistingAreasDefaultToMessageKind(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO areas (name, tag, description, federated, created_at)
		 VALUES ('legacy', x'01020304', '', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	a, err := s.GetAnyArea(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != KindMessage {
		t.Errorf("an area created without a kind came back as %q, want message", a.Kind)
	}
}

func TestRemoveFileReportsOrphanedBlob(t *testing.T) {
	s, ctx := testStore(t)
	for _, name := range []string{"utils", "games"} {
		if _, err := s.CreateFileArea(ctx, name, "", false); err != nil {
			t.Fatal(err)
		}
	}
	h := fileHashOf(0x33)
	for _, area := range []string{"utils", "games"} {
		if _, err := s.PutFile(ctx, area, File{Name: "SHARED.DAT", Hash: h, Size: 8}); err != nil {
			t.Fatal(err)
		}
	}

	// The other area still holds it, so the bytes must stay.
	orphaned, gotHash, err := s.RemoveFile(ctx, "utils", "SHARED.DAT")
	if err != nil {
		t.Fatal(err)
	}
	if orphaned {
		t.Error("blob reported orphaned while another area still references it")
	}
	if gotHash != h {
		t.Errorf("RemoveFile reported hash %s, want %s", gotHash, h)
	}

	// Now nothing does.
	orphaned, _, err = s.RemoveFile(ctx, "games", "SHARED.DAT")
	if err != nil {
		t.Fatal(err)
	}
	if !orphaned {
		t.Error("blob not reported orphaned after its last reference went away")
	}

	var blobs int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs`).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != 0 {
		t.Errorf("%d blob rows survived the last reference", blobs)
	}
}

func TestHoldsBlobMatchesTruncatedWireHash(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	h := fileHashOf(0x44)
	if _, err := s.PutFile(ctx, "utils", File{Name: "HELD.BIN", Hash: h, Size: 3}); err != nil {
		t.Fatal(err)
	}

	// A FILE record carries 16 bytes, not 32.
	held, err := s.HoldsBlob(ctx, h[:16])
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("HoldsBlob said no for content we hold, matched on the wire prefix")
	}

	absent := fileHashOf(0x99)
	held, err = s.HoldsBlob(ctx, absent[:16])
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("HoldsBlob said yes for content we do not hold")
	}
}

func TestSetFileRecord(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", true); err != nil {
		t.Fatal(err)
	}
	key := testKey(t, 7)
	if err := s.PutNode(ctx, Node{ID: key.ID(), PublicKey: key.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}

	saved, err := s.PutFile(ctx, "utils", File{Name: "PUB.TXT", Hash: fileHashOf(0x55), Size: 2})
	if err != nil {
		t.Fatal(err)
	}

	seq, err := s.NextSeq(ctx, record.AreaTagFor("utils"))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := record.TruncateFileHash(saved.Hash[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(key, seq, 1, record.AreaTagFor("utils"), record.FileBody{
		Name: "PUB.TXT", Size: 2, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFileRecord(ctx, saved.ID, rec.ID()); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetFile(ctx, "utils", "PUB.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Published() {
		t.Fatal("file did not come back published")
	}
	if got.Record != rec.ID() {
		t.Errorf("attached record is %s, want %s", got.Record, rec.ID())
	}
}

func TestValidateFileName(t *testing.T) {
	valid := []string{"README", "ARCHIVE.ZIP", "a", "file-name_1.tar.gz",
		strings.Repeat("x", MaxFileNameLen)}
	for _, name := range valid {
		if err := ValidateFileName(name); err != nil {
			t.Errorf("ValidateFileName(%q) = %v, want nil", name, err)
		}
	}

	// Every one of these reaches a wire record, an SFTP path, or a terminal.
	invalid := []string{"", ".", "..", "dir/file", `dir\file`, "bad\x00name",
		"line\nbreak", strings.Repeat("x", MaxFileNameLen+1)}
	for _, name := range invalid {
		if err := ValidateFileName(name); err == nil {
			t.Errorf("ValidateFileName(%q) accepted it", name)
		}
	}
}

func TestGetFileNotFound(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFile(ctx, "utils", "ABSENT.TXT"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFile for a missing file returned %v, want ErrNotFound", err)
	}
}

func TestCountFiles(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"A.TXT", "B.TXT", "C.TXT"} {
		if _, err := s.PutFile(ctx, "utils", File{Name: name, Hash: fileHashOf(byte(len(name))), Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.CountFiles(ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("CountFiles = %d, want 3", n)
	}
}

// A catalog entry arriving from a PEER, through the real ingest path.
//
// This is the half of §6.5 that nothing else covers: emission is tested where
// records are minted, but the receiving side has to verify a stranger's
// signature, store the record, and then surface a file it does not hold. Two
// real stores, and the record crosses between them as bytes.
func TestCatalogEntryFromAPeer(t *testing.T) {
	local, ctx := testStore(t)
	area, err := local.CreateFileArea(ctx, "meshwide", "shared", true)
	if err != nil {
		t.Fatal(err)
	}

	// Us, and a peer whose NODE record we hold — without it the record is
	// quarantined, which is §6.1.2 working rather than a failure.
	self := testKey(t, 1)
	if err := local.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	peer := trustedKey(t, local, ctx, 2)

	g, err := NewGossipStore(ctx, local, func(err error) { t.Logf("gossipstore: %v", err) })
	if err != nil {
		t.Fatal(err)
	}

	peerHash := fileHashOf(0xEE)
	wire, err := record.TruncateFileHash(peerHash[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_500, area.Tag, record.FileBody{
		Name: "PEER.ZIP", Size: 512_000, Hash: wire, Description: "Held over there",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cross the wire as bytes, so the signature is checked against what was
	// actually transmitted rather than an in-process struct.
	arrived, err := record.Unmarshal(rec.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	added, err := g.Apply(area.Tag, []*record.Record{arrived})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("Apply took %d records, want 1", added)
	}

	entries, err := local.ListCatalog(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("catalog has %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "PEER.ZIP" || e.Size != 512_000 || e.Description != "Held over there" {
		t.Errorf("entry is %+v", e)
	}
	if e.Origin != peer.ID() {
		t.Error("the entry is not attributed to the peer that announced it")
	}
	if e.Local {
		t.Error("a peer's entry is marked local")
	}
	// The whole point of the catalog: we know about it and we do not have it.
	if e.Held {
		t.Error("we claim to hold content that never crossed the mesh")
	}
}

// Our own files and a peer's appear in one listing, each labelled.
func TestCatalogMixesLocalAndRemote(t *testing.T) {
	local, ctx := testStore(t)
	area, err := local.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := local.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	peer := trustedKey(t, local, ctx, 2)
	g, err := NewGossipStore(ctx, local, func(err error) { t.Logf("gossipstore: %v", err) })
	if err != nil {
		t.Fatal(err)
	}

	// Ours: catalogued locally AND announced, so we hold the content.
	ourHash := fileHashOf(0x11)
	if _, err := local.PutFile(ctx, "meshwide", File{
		Name: "OURS.ZIP", Hash: ourHash, Size: 100,
	}); err != nil {
		t.Fatal(err)
	}
	ourWire, err := record.TruncateFileHash(ourHash[:])
	if err != nil {
		t.Fatal(err)
	}
	ourSeq, err := local.NextSeq(ctx, area.Tag)
	if err != nil {
		t.Fatal(err)
	}
	ourRec, err := record.NewFileRecord(self, ourSeq, 1_700_000_100, area.Tag, record.FileBody{
		Name: "OURS.ZIP", Size: 100, Hash: ourWire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.PutRecord(ctx, ourRec); err != nil {
		t.Fatal(err)
	}

	// Theirs: announced only.
	theirHash := fileHashOf(0x22)
	theirWire, err := record.TruncateFileHash(theirHash[:])
	if err != nil {
		t.Fatal(err)
	}
	theirRec, err := record.NewFileRecord(peer, 1, 1_700_000_200, area.Tag, record.FileBody{
		Name: "THEIRS.ZIP", Size: 200, Hash: theirWire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Apply(area.Tag, []*record.Record{theirRec}); err != nil {
		t.Fatal(err)
	}

	entries, err := local.ListCatalog(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("catalog has %d entries, want 2", len(entries))
	}
	byName := map[string]CatalogEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if ours := byName["OURS.ZIP"]; !ours.Local || !ours.Held {
		t.Errorf("our own file is %+v, want local and held", ours)
	}
	if theirs := byName["THEIRS.ZIP"]; theirs.Local || theirs.Held {
		t.Errorf("the peer's file is %+v, want neither local nor held", theirs)
	}
}

// Content addressing means a peer announcing a file we ALREADY have is
// recognised as such, without either side asking. That is what makes "held by"
// answerable at all (§6.5).
func TestCatalogRecognisesContentWeAlreadyHold(t *testing.T) {
	local, ctx := testStore(t)
	area, err := local.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := local.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	peer := trustedKey(t, local, ctx, 2)
	g, err := NewGossipStore(ctx, local, func(err error) { t.Logf("gossipstore: %v", err) })
	if err != nil {
		t.Fatal(err)
	}

	// We hold this content, under our own name for it.
	shared := fileHashOf(0x33)
	if _, err := local.PutFile(ctx, "meshwide", File{
		Name: "MY-NAME-FOR-IT.ZIP", Hash: shared, Size: 4096,
	}); err != nil {
		t.Fatal(err)
	}

	// The peer announces the same bytes under a different name.
	wire, err := record.TruncateFileHash(shared[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_300, area.Tag, record.FileBody{
		Name: "THEIR-NAME.ZIP", Size: 4096, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Apply(area.Tag, []*record.Record{rec}); err != nil {
		t.Fatal(err)
	}

	entries, err := local.ListCatalog(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	var theirs CatalogEntry
	for _, e := range entries {
		if e.Name == "THEIR-NAME.ZIP" {
			theirs = e
		}
	}
	if theirs.Name == "" {
		t.Fatal("the peer's entry is missing")
	}
	if !theirs.Held {
		t.Error("we hold these exact bytes but the catalog says we do not")
	}
}

// One unparseable entry from one peer must not blank the listing.
func TestCatalogSkipsAnUnparseableEntry(t *testing.T) {
	local, ctx := testStore(t)
	area, err := local.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := local.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}

	// A record with an unparseable FILE body can no longer be BUILT — §7.5 is
	// enforced in record.New and on decode. So this writes one straight into
	// the table, standing in for a row that predates that rule or arrives in
	// some future format. The skip below is the last line of defence, and it
	// still has to hold.
	junkHash := fileHashOf(0x99)
	junkWire, err := record.TruncateFileHash(junkHash[:])
	if err != nil {
		t.Fatal(err)
	}
	junk, err := record.NewFileRecord(self, 1, 1_700_000_000, area.Tag, record.FileBody{
		Name: "JUNK.ZIP", Size: 1, Hash: junkWire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.PutRecord(ctx, junk); err != nil {
		t.Fatal(err)
	}
	id := junk.ID()
	if _, err := local.db.ExecContext(ctx,
		`UPDATE records SET body = ? WHERE id = ?`, []byte("not a FILE body"), id[:]); err != nil {
		t.Fatal(err)
	}

	hash := fileHashOf(0x44)
	wire, err := record.TruncateFileHash(hash[:])
	if err != nil {
		t.Fatal(err)
	}
	good, err := record.NewFileRecord(self, 2, 1_700_000_001, area.Tag, record.FileBody{
		Name: "GOOD.ZIP", Size: 1, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.PutRecord(ctx, good); err != nil {
		t.Fatal(err)
	}

	entries, err := local.ListCatalog(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "GOOD.ZIP" {
		t.Errorf("catalog is %+v, want just the parseable entry", entries)
	}
}

// A local-only area publishes nothing, so the record log is empty and the
// files table is the whole listing. Reading only the log would show an empty
// area that visibly has files in it.
func TestAreaContentsForALocalOnlyArea(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFile(ctx, "utils", File{
		Name: "LOCAL.ZIP", Hash: fileHashOf(0x11), Size: 10, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	if entries, err := s.ListCatalog(ctx, "utils"); err != nil {
		t.Fatal(err)
	} else if len(entries) != 0 {
		t.Fatalf("a local-only area announced %d entries", len(entries))
	}

	entries, err := s.ListAreaContents(ctx, "utils")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("area contents has %d entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Name != "LOCAL.ZIP" || !e.Local || !e.Held || e.Uploader != "austin" {
		t.Errorf("entry is %+v", e)
	}
}

// A federated area's own file appears in the log AND the table. It must be one
// row, carrying what only the table knows.
func TestAreaContentsDoesNotDoubleCountOurOwnFile(t *testing.T) {
	s, ctx := testStore(t)
	area, err := s.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := s.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}

	hash := fileHashOf(0x22)
	if _, err := s.PutFile(ctx, "meshwide", File{
		Name: "OURS.ZIP", Hash: hash, Size: 100, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}
	wire, err := record.TruncateFileHash(hash[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(self, 1, 1_700_000_000, area.Tag, record.FileBody{
		Name: "OURS.ZIP", Size: 100, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListAreaContents(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("our own file appears %d times, want 1", len(entries))
	}
	// The merged row keeps what only the local table knows.
	if entries[0].Uploader != "austin" {
		t.Errorf("the merged row lost the uploader: %+v", entries[0])
	}
}

// A peer's file gets the sysop's petname for the holding node, because that is
// the only human-facing name in the design ([D9]) — and the sysop's contact,
// which is the only actionable thing a user has for a file we cannot fetch.
func TestAreaContentsLabelsTheHolder(t *testing.T) {
	s, ctx := testStore(t)
	area, err := s.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := s.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	peer := testKey(t, 2)
	if err := s.PutNode(ctx, Node{
		ID: peer.ID(), PublicKey: peer.Public,
		DisplayName: "Pacific NW BBS", SysopContact: "sysop@pnw.example",
	}); err != nil {
		t.Fatal(err)
	}

	theirHash := fileHashOf(0x33)
	wire, err := record.TruncateFileHash(theirHash[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_000, area.Tag, record.FileBody{
		Name: "THEIRS.ZIP", Size: 50, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// With no alias set, the node's own display name is the fallback.
	entries, err := s.ListAreaContents(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	if entries[0].Holder != "Pacific NW BBS" {
		t.Errorf("holder is %q, want the node's display name", entries[0].Holder)
	}
	if entries[0].HolderContact != "sysop@pnw.example" {
		t.Errorf("holder contact is %q", entries[0].HolderContact)
	}

	// A petname beats the display name: it is the name this sysop chose, and
	// [D9] makes it the human-facing surface.
	if err := s.SetAlias(ctx, "pnw", peer.ID()); err != nil {
		t.Fatal(err)
	}
	entries, err = s.ListAreaContents(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Holder != "pnw" {
		t.Errorf("holder is %q, want the local petname", entries[0].Holder)
	}
}

// Two BBSes may hold different files under one name. Both are listed, because
// collapsing them would hide one node's file behind another's.
func TestAreaContentsKeepsBothWhenNamesCollide(t *testing.T) {
	s, ctx := testStore(t)
	area, err := s.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := s.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	peer := trustedKey(t, s, ctx, 2)

	if _, err := s.PutFile(ctx, "meshwide", File{
		Name: "README.TXT", Hash: fileHashOf(0x44), Size: 10, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}
	theirHash := fileHashOf(0x55)
	wire, err := record.TruncateFileHash(theirHash[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_000, area.Tag, record.FileBody{
		Name: "README.TXT", Size: 999, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	entries, err := s.ListAreaContents(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want both files named README.TXT", len(entries))
	}
	var mine, theirs bool
	for _, e := range entries {
		if e.Local && e.Held {
			mine = true
		}
		if !e.Local && !e.Held && e.Size == 999 {
			theirs = true
		}
	}
	if !mine || !theirs {
		t.Errorf("entries are %+v", entries)
	}
}

func TestMayDescribe(t *testing.T) {
	own := File{Uploader: "austin"}
	unowned := File{Uploader: ""}

	cases := []struct {
		name  string
		f     File
		nick  string
		sysop bool
		want  bool
	}{
		{"the uploader", own, "austin", false, true},
		{"the uploader in another case", own, "AUSTIN", false, true},
		{"someone else", own, "bob", false, false},
		{"a sysop, on someone else's file", own, "bob", true, true},
		{"a guest", own, "guest", false, false},
		// An empty uploader is a file with nobody to own it, not a wildcard.
		{"an empty nick against an unowned file", unowned, "", false, false},
		{"a named user against an unowned file", unowned, "austin", false, false},
		{"a sysop against an unowned file", unowned, "austin", true, true},
	}
	for _, tc := range cases {
		if got := tc.f.MayDescribe(tc.nick, tc.sysop); got != tc.want {
			t.Errorf("%s: MayDescribe(%q, sysop=%v) = %v, want %v",
				tc.name, tc.nick, tc.sysop, got, tc.want)
		}
	}
}

func TestSetFileDescription(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFile(ctx, "utils", File{
		Name: "A.TXT", Hash: fileHashOf(0x11), Size: 10, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	// An SFTP upload arrives with no description — that is the whole reason
	// this method exists.
	f, err := s.GetFile(ctx, "utils", "A.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if f.Description != "" {
		t.Fatalf("a fresh upload has description %q, want empty", f.Description)
	}

	if err := s.SetFileDescription(ctx, "utils", "A.TXT", "  Notes on the thing  ", "austin"); err != nil {
		t.Fatal(err)
	}
	f, err = s.GetFile(ctx, "utils", "A.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if f.Description != "Notes on the thing" {
		t.Errorf("description = %q, want it trimmed to %q", f.Description, "Notes on the thing")
	}

	// Clearing is setting it to nothing, not a separate operation.
	if err := s.SetFileDescription(ctx, "utils", "A.TXT", "", "austin"); err != nil {
		t.Fatal(err)
	}
	f, err = s.GetFile(ctx, "utils", "A.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if f.Description != "" {
		t.Errorf("description = %q after clearing, want empty", f.Description)
	}
}

// A published file whose description changes has to be re-announced: the FILE
// record carries the description, so the one peers hold is now stale.
func TestSetFileDescriptionDetachesTheRecord(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", true); err != nil {
		t.Fatal(err)
	}
	key := testKey(t, 7)
	if err := s.PutNode(ctx, Node{ID: key.ID(), PublicKey: key.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	f, err := s.PutFile(ctx, "utils", File{Name: "A.TXT", Hash: fileHashOf(0x11), Size: 1})
	if err != nil {
		t.Fatal(err)
	}

	seq, err := s.NextSeq(ctx, record.AreaTagFor("utils"))
	if err != nil {
		t.Fatal(err)
	}
	// A real catalog entry: §7.5 refuses a FILE record whose body is anything
	// else, so a placeholder body cannot stand in for one any more.
	wire, err := record.TruncateFileHash(f.Hash[:])
	if err != nil {
		t.Fatal(err)
	}
	rec, err := record.NewFileRecord(key, seq, 1, record.AreaTagFor("utils"), record.FileBody{
		Name: "A.TXT", Size: 1, Hash: wire,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFileRecord(ctx, f.ID, rec.ID()); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetFile(ctx, "utils", "A.TXT"); err != nil {
		t.Fatal(err)
	} else if !got.Published() {
		t.Fatal("the file is not published after SetFileRecord; this test cannot detect a detach")
	}

	if err := s.SetFileDescription(ctx, "utils", "A.TXT", "now described", "austin"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFile(ctx, "utils", "A.TXT")
	if err != nil {
		t.Fatal(err)
	}
	if got.Published() {
		t.Error("the file is still marked published after its description changed, " +
			"so the stale FILE record on peers would never be superseded")
	}
}

// A description longer than the wire allows must be refused where it is typed.
// Accepting it here would produce a file that exists locally and silently never
// publishes, because MarshalFileBody would reject it much later.
func TestSetFileDescriptionRefusesUnwireableText(t *testing.T) {
	s, ctx := testStore(t)
	if _, err := s.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFile(ctx, "utils", File{Name: "A.TXT", Hash: fileHashOf(0x11), Size: 1}); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{
		strings.Repeat("x", MaxFileDescLen+1),
		"a control\x00character",
		"a line\nbreak",
	} {
		if err := s.SetFileDescription(ctx, "utils", "A.TXT", bad, "cli"); err == nil {
			t.Errorf("SetFileDescription(%q) accepted it", bad)
		}
	}

	// The limit is the wire's, not one of this package's own invention.
	ok := strings.Repeat("x", MaxFileDescLen)
	if err := s.SetFileDescription(ctx, "utils", "A.TXT", ok, "cli"); err != nil {
		t.Errorf("SetFileDescription at exactly the limit failed: %v", err)
	}
	if err := record.ValidateFileDescription(ok); err != nil {
		t.Errorf("the store accepted a description the wire refuses: %v", err)
	}
}
