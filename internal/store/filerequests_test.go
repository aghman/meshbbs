package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
)

// requestFixture is a board holding a peer's catalog entry for a file it does
// not have — the exact situation §6.5's fetch path 2 exists for.
//
// The record crosses through the real ingest path rather than being inserted,
// because a request names the hash the WIRE carried and that truncation is what
// the whole path is keyed on.
func requestFixture(t *testing.T) (*Store, context.Context, record.FileBody) {
	t.Helper()
	st, ctx := testStore(t)
	area, err := st.CreateFileArea(ctx, "meshwide", "shared", true)
	if err != nil {
		t.Fatal(err)
	}
	self := testKey(t, 1)
	if err := st.PutNode(ctx, Node{ID: self.ID(), PublicKey: self.Public, IsSelf: true}); err != nil {
		t.Fatal(err)
	}
	peer := trustedKey(t, st, ctx, 2)

	g, err := NewGossipStore(ctx, st, func(err error) { t.Logf("gossipstore: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	peerHash := fileHashOf(0xEE)
	wire, err := record.TruncateFileHash(peerHash[:])
	if err != nil {
		t.Fatal(err)
	}
	body := record.FileBody{
		Name: "PEER.ZIP", Size: 512_000, Hash: wire, Description: "Held over there",
	}
	rec, err := record.NewFileRecord(peer, 1, 1_700_000_500, area.Tag, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Apply(area.Tag, []*record.Record{rec}); err != nil {
		t.Fatal(err)
	}
	return st, ctx, body
}

func TestRequestFileQueuesAndListsIt(t *testing.T) {
	st, ctx, body := requestFixture(t)

	req, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if req.ID == 0 || req.Arrived() {
		t.Errorf("queued request came back as %+v", req)
	}

	all, err := st.ListFileRequests(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "PEER.ZIP" || all[0].Nick != "austin" {
		t.Fatalf("queue holds %+v", all)
	}
	if all[0].Hash != body.Hash {
		t.Error("the request lost the hash it was made against")
	}

	// The hash is what travels, and it is what a carrier asks for.
	hashes, err := st.OpenFileRequestHashes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 || hashes[0] != body.Hash {
		t.Fatalf("a carrier would ask for %x", hashes)
	}
}

// Two people wanting one file is one thing to carry and two people to tell.
func TestTwoUsersAskingForOneFileIsOneHashOnTheWire(t *testing.T) {
	st, ctx, body := requestFixture(t)
	for _, nick := range []string{"austin", "morgan"} {
		if _, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, nick); err != nil {
			t.Fatalf("%s: %v", nick, err)
		}
	}

	hashes, err := st.OpenFileRequestHashes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 1 {
		t.Errorf("a carrier would ask %d times for one file", len(hashes))
	}
	mine, err := st.ListFileRequests(ctx, "morgan")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Errorf("morgan's own queue holds %d", len(mine))
	}
}

// Every refusal is one the person can act on, so each is worth pinning.
func TestRequestFileRefusals(t *testing.T) {
	st, ctx, body := requestFixture(t)

	if _, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin"); err != nil {
		t.Fatal(err)
	}
	_, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin")
	if !errors.Is(err, ErrFileRequestExists) {
		t.Errorf("asking twice returned %v", err)
	}

	// Content this board holds is a download, not a week's wait.
	held := fileHashOf(0x77)
	if _, err := st.PutFile(ctx, "meshwide", File{
		Name: "HERE.ZIP", Hash: held, Size: 10, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}
	heldWire, err := record.TruncateFileHash(held[:])
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.RequestFile(ctx, "meshwide", "HERE.ZIP", heldWire, identity.NodeID{}, "morgan")
	if !errors.Is(err, ErrFileAlreadyHeld) {
		t.Errorf("asking for a held file returned %v", err)
	}

	// A name already taken by DIFFERENT content is refused here, where the
	// person can do something, rather than at arrival in front of a sysop.
	elsewhere := fileHashOf(0x99)
	other, err := record.TruncateFileHash(elsewhere[:])
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.RequestFile(ctx, "meshwide", "HERE.ZIP", other, identity.NodeID{}, "morgan")
	if err == nil || !strings.Contains(err.Error(), "already holds a different file") {
		t.Errorf("a colliding name returned %v", err)
	}
}

func TestRequestQueueHasACeiling(t *testing.T) {
	st, ctx, _ := requestFixture(t)

	for i := 0; i < MaxOpenFileRequests; i++ {
		var h [record.FileHashLen]byte
		h[0], h[1] = 0xA0, byte(i)
		name := "F" + string(rune('A'+i)) + ".ZIP"
		if _, err := st.RequestFile(ctx, "meshwide", name, h, identity.NodeID{}, "austin"); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}

	var one [record.FileHashLen]byte
	one[0] = 0xB0
	_, err := st.RequestFile(ctx, "meshwide", "TOOMANY.ZIP", one, identity.NodeID{}, "austin")
	if !errors.Is(err, ErrFileRequestQueueFull) {
		t.Errorf("the %dth request returned %v", MaxOpenFileRequests+1, err)
	}
	// Somebody else's queue is their own.
	if _, err := st.RequestFile(ctx, "meshwide", "TOOMANY.ZIP", one, identity.NodeID{}, "morgan"); err != nil {
		t.Errorf("one user's full queue blocked another's: %v", err)
	}
}

// The end of fetch path 2: bytes arrive, the file becomes downloadable, and
// everyone who asked is owed a notice.
func TestSatisfyFileRequestsFilesTheContent(t *testing.T) {
	st, ctx, body := requestFixture(t)
	for _, nick := range []string{"austin", "morgan"} {
		if _, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, nick); err != nil {
			t.Fatal(err)
		}
	}

	closed, err := st.SatisfyFileRequests(ctx, fileHashOf(0xEE), 512_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 2 {
		t.Fatalf("closed %d requests, want both", len(closed))
	}
	for _, r := range closed {
		if !r.Filed() {
			t.Errorf("%s's request came back unfiled: %q", r.Nick, r.Note)
		}
	}

	// One catalog row for two requesters, and it is downloadable.
	f, err := st.GetFile(ctx, "meshwide", "PEER.ZIP")
	if err != nil {
		t.Fatalf("the arrival was not filed: %v", err)
	}
	if f.Hash != fileHashOf(0xEE) || f.Size != 512_000 {
		t.Errorf("filed as %+v", f)
	}
	if f.Uploader != "" {
		t.Errorf("uploader is %q; nobody here uploaded it", f.Uploader)
	}
	// Not announced: the network already knows, and a second origin restating
	// it would spend §1.1's shared airtime on nothing.
	if f.Published() {
		t.Error("an arrival minted a FILE record of its own")
	}

	// And nothing asks for it again.
	hashes, err := st.OpenFileRequestHashes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hashes) != 0 {
		t.Errorf("the next carrier would ask for %x again", hashes)
	}
}

// A hash nobody asked for is an ordinary outcome for a blunt --files carrier,
// not an error.
func TestSatisfyFileRequestsIgnoresContentNobodyAskedFor(t *testing.T) {
	st, ctx, _ := requestFixture(t)
	closed, err := st.SatisfyFileRequests(ctx, fileHashOf(0x42), 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 0 {
		t.Errorf("closed %d requests for content nobody wanted", len(closed))
	}
}

// The collision the request-time check cannot prevent: somebody uploads that
// name while the stick is in transit. The bytes are here and the request is
// closed with the reason, rather than re-asked forever.
func TestAnArrivalOntoATakenNameIsRecordedNotRetried(t *testing.T) {
	st, ctx, body := requestFixture(t)
	if _, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin"); err != nil {
		t.Fatal(err)
	}
	// Between the request and the carrier, an upload takes the name.
	if _, err := st.PutFile(ctx, "meshwide", File{
		Name: body.Name, Hash: fileHashOf(0x0B), Size: 3, Uploader: "morgan",
	}); err != nil {
		t.Fatal(err)
	}

	closed, err := st.SatisfyFileRequests(ctx, fileHashOf(0xEE), 512_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 {
		t.Fatalf("closed %d", len(closed))
	}
	if closed[0].Filed() {
		t.Fatal("the arrival claimed to be filed over somebody else's upload")
	}
	if !strings.Contains(closed[0].Note, "already holds a different file") {
		t.Errorf("the note does not say what happened: %q", closed[0].Note)
	}
	if hashes, _ := st.OpenFileRequestHashes(ctx, 10); len(hashes) != 0 {
		t.Error("a collision at this end would be re-asked of the other board")
	}
}

// The notice is owed to somebody who was not there when the stick arrived, and
// owed exactly once (§6.5).
func TestArrivalIsNotifiedOnceAndThenNotAgain(t *testing.T) {
	st, ctx, body := requestFixture(t)
	if _, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin"); err != nil {
		t.Fatal(err)
	}

	if pending, err := st.UnnotifiedFileRequests(ctx, "austin"); err != nil || len(pending) != 0 {
		t.Fatalf("a waiting request is not news yet: %v %v", pending, err)
	}
	if _, err := st.SatisfyFileRequests(ctx, fileHashOf(0xEE), 512_000); err != nil {
		t.Fatal(err)
	}

	news, err := st.UnnotifiedFileRequests(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if len(news) != 1 {
		t.Fatalf("owed %d notices, want 1", len(news))
	}
	// Nobody else's business.
	if other, _ := st.UnnotifiedFileRequests(ctx, "morgan"); len(other) != 0 {
		t.Error("one user's arrival was offered to another")
	}

	if err := st.MarkFileRequestsNotified(ctx, []int64{news[0].ID}); err != nil {
		t.Fatal(err)
	}
	again, err := st.UnnotifiedFileRequests(ctx, "austin")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Error("the notice would repeat at every login")
	}
}

func TestCancelFileRequestIsScopedToWhoAsked(t *testing.T) {
	st, ctx, body := requestFixture(t)
	req, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin")
	if err != nil {
		t.Fatal(err)
	}

	if err := st.CancelFileRequest(ctx, req.ID, "morgan"); !errors.Is(err, ErrNotFound) {
		t.Errorf("somebody else cancelled it: %v", err)
	}
	if err := st.CancelFileRequest(ctx, req.ID, "austin"); err != nil {
		t.Fatal(err)
	}
	if all, _ := st.ListFileRequests(ctx, ""); len(all) != 0 {
		t.Errorf("the queue still holds %+v", all)
	}
}

// The merge that keeps a requested file from being two rows: the peer's
// announcement and the copy that arrived are one file, and the browser has to
// say so.
func TestAnArrivedFileIsOneRowInTheListing(t *testing.T) {
	st, ctx, body := requestFixture(t)
	if _, err := st.RequestFile(ctx, "meshwide", body.Name, body.Hash, identity.NodeID{}, "austin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SatisfyFileRequests(ctx, fileHashOf(0xEE), 512_000); err != nil {
		t.Fatal(err)
	}

	entries, err := st.ListAreaContents(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("the listing shows %d rows for one file: %+v", len(entries), entries)
	}
	if !entries[0].Held {
		t.Error("the row does not offer the copy that just arrived")
	}
	if entries[0].Description != body.Description {
		t.Errorf("description is %q; it comes from the holder's record", entries[0].Description)
	}
}

// The coincidental case the merge must NOT collapse: one name, two boards, two
// different files.
func TestOneNameTwoContentsStaysTwoRows(t *testing.T) {
	st, ctx, body := requestFixture(t)
	if _, err := st.PutFile(ctx, "meshwide", File{
		Name: body.Name, Hash: fileHashOf(0x0C), Size: 9, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	entries, err := st.ListAreaContents(ctx, "meshwide")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("two different files sharing a name collapsed to %d row(s)", len(entries))
	}
}
