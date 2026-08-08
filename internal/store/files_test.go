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
	rec, err := record.New(key, record.Record{
		Origin: key.ID(), Seq: seq, TS: 1, Type: record.TypeFile,
		Area: record.AreaTagFor("utils"), Body: []byte("body"),
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

	junk, err := record.New(self, record.Record{
		Origin: self.ID(), Seq: 1, TS: 1_700_000_000, Type: record.TypeFile,
		Area: area.Tag, Body: []byte("not a FILE body"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.PutRecord(ctx, junk); err != nil {
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
