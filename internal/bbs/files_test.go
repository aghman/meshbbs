package bbs

import (
	"errors"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

func fileHash(b byte) blobstore.Hash {
	var h blobstore.Hash
	for i := range h {
		h[i] = b
	}
	return h
}

// A local file area catalogues without announcing anything: no record, no
// airtime, and no capability beyond being able to upload at all.
func TestLocalFileAreaPublishesNothing(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")
	if _, err := st.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)

	saved, err := svc.AddFile(ctx, "utils", store.File{
		Name: "LOCAL.ZIP", Hash: fileHash(0x11), Size: 4096, Uploader: "austin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Published() {
		t.Error("a file in a local-only area claims to be published")
	}
	if len(pub.recs) != 0 {
		t.Errorf("%d records were published for a local area", len(pub.recs))
	}
}

// A federated file area announces the CATALOG ENTRY, and the entry is a signed
// FILE record carrying the truncated content hash.
func TestFederatedFilePublishesACatalogEntry(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw", store.CapPostFederated)
	area, err := st.CreateFileArea(ctx, "meshwide", "", true)
	if err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)

	hash := fileHash(0x22)
	saved, err := svc.AddFile(ctx, "meshwide", store.File{
		Name: "SHARED.ZIP", Hash: hash, Size: 2_400_000,
		Description: "Something worth having", Uploader: "austin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Published() {
		t.Fatal("a file in a federated area was not announced")
	}
	if len(pub.recs) != 1 {
		t.Fatalf("%d records published, want 1", len(pub.recs))
	}

	rec := pub.recs[0]
	if rec.Type != record.TypeFile {
		t.Errorf("published a %s, want FILE", rec.Type)
	}
	if rec.Area != area.Tag {
		t.Error("the entry did not land in its file area")
	}
	if pub.areas[0] != area.Tag {
		t.Error("the entry was published under the wrong area tag")
	}
	if rec.ID() != saved.Record {
		t.Error("the catalog row does not point at the record that announced it")
	}

	body, err := record.UnmarshalFileBody(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body.Name != "SHARED.ZIP" || body.Size != 2_400_000 ||
		body.Description != "Something worth having" {
		t.Errorf("the entry says %+v", body)
	}
	// The wire hash is the local hash truncated, which is what lets a receiver
	// answer "do I hold this?" without asking anyone.
	want, err := record.TruncateFileHash(hash[:])
	if err != nil {
		t.Fatal(err)
	}
	if body.Hash != want {
		t.Errorf("wire hash is %x, want %x", body.Hash, want)
	}

	// The holding node is the record's origin, which is why the body has no
	// such field.
	if rec.Origin != svc.NodeID() {
		t.Error("the entry's origin is not this node")
	}
}

// The record must verify against this node's key like any other, or a peer
// quarantines it.
func TestPublishedCatalogEntryVerifies(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw", store.CapPostFederated)
	if _, err := st.CreateFileArea(ctx, "meshwide", "", true); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)

	if _, err := svc.AddFile(ctx, "meshwide", store.File{
		Name: "SIGNED.BIN", Hash: fileHash(0x33), Size: 10, Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	self, err := st.SelfNode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := record.VerifyFileRecord(pub.recs[0], self.PublicKey); err != nil {
		t.Errorf("the published entry does not verify: %v", err)
	}
}

// [N7] applies to file areas for the same reason it applies to posts: a
// federated one spends the network's shared airtime.
func TestFederatedFileAreaNeedsTheCapability(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "nocap", "pw") // no post_federated
	if _, err := st.CreateFileArea(ctx, "meshwide", "", true); err != nil {
		t.Fatal(err)
	}

	_, err := svc.AddFile(ctx, "meshwide", store.File{
		Name: "NOPE.ZIP", Hash: fileHash(0x44), Size: 1, Uploader: "nocap",
	})
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("upload without the capability returned %v", err)
	}
	// The remedy is not something the user can do themselves, so the error has
	// to name it.
	if !strings.Contains(err.Error(), store.CapPostFederated) {
		t.Errorf("the refusal does not name the capability: %v", err)
	}

	// And nothing was catalogued: a refused upload must not leave a row.
	if _, err := st.GetFile(ctx, "meshwide", "NOPE.ZIP"); !errors.Is(err, store.ErrNotFound) {
		t.Error("a refused upload was catalogued anyway")
	}
}

// The same user may still fill a LOCAL area — the gate is on spending airtime,
// not on uploading.
func TestUncapableUserCanStillUseALocalArea(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "nocap", "pw")
	if _, err := st.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddFile(ctx, "utils", store.File{
		Name: "FINE.ZIP", Hash: fileHash(0x55), Size: 1, Uploader: "nocap",
	}); err != nil {
		t.Errorf("a local upload was refused: %v", err)
	}
}

// CanUploadTo is what lets SFTP refuse before a byte moves, so it has to agree
// with AddFile rather than approximate it.
func TestCanUploadToAgreesWithAddFile(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "capable", "pw", store.CapPostFederated)
	mkUser(t, svc, st, ctx, "nocap", "pw")
	if _, err := st.CreateFileArea(ctx, "meshwide", "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateFileArea(ctx, "utils", "", false); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		area, nick string
		wantErr    bool
	}{
		{"meshwide", "capable", false},
		{"meshwide", "nocap", true},
		{"utils", "nocap", false},
	}
	for _, c := range cases {
		preflight := svc.CanUploadTo(ctx, c.area, c.nick)
		_, actual := svc.AddFile(ctx, c.area, store.File{
			Name: "X-" + c.nick + ".BIN", Hash: fileHash(0x66), Size: 1, Uploader: c.nick,
		})
		if (preflight != nil) != c.wantErr {
			t.Errorf("CanUploadTo(%s, %s) = %v, wantErr %v", c.area, c.nick, preflight, c.wantErr)
		}
		if (preflight != nil) != (actual != nil) {
			t.Errorf("%s/%s: preflight said %v but the upload said %v",
				c.area, c.nick, preflight, actual)
		}
	}
}

// A publish failure must not fail the upload. The file is catalogued and it is
// the user's; §7.3 repairs a missed announcement by anti-entropy.
func TestPublishFailureDoesNotFailTheUpload(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw", store.CapPostFederated)
	if _, err := st.CreateFileArea(ctx, "meshwide", "", true); err != nil {
		t.Fatal(err)
	}

	var reported error
	OnPublishError = func(err error) { reported = err }
	t.Cleanup(func() { OnPublishError = func(error) {} })
	svc.SetPublisher(&recordingPublisher{err: errors.New("radio is on fire")})

	saved, err := svc.AddFile(ctx, "meshwide", store.File{
		Name: "STILL.ZIP", Hash: fileHash(0x77), Size: 1, Uploader: "austin",
	})
	if err != nil {
		t.Fatalf("a radio failure failed the upload: %v", err)
	}
	if !saved.Published() {
		t.Error("the record was not minted despite the transmit failing")
	}
	if reported == nil {
		t.Error("the failure was not reported to the sysop")
	}

	// The record is in the log, so anti-entropy can still carry it.
	if _, err := st.GetFile(ctx, "meshwide", "STILL.ZIP"); err != nil {
		t.Errorf("the file is not catalogued: %v", err)
	}
}

// A read-only area refuses uploads whatever the user holds.
func TestReadOnlyFileAreaRefusesUploads(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw", store.CapPostFederated)
	if _, err := st.CreateFileArea(ctx, "readonly", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE areas SET read_only = 1 WHERE name = 'readonly'`); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.AddFile(ctx, "readonly", store.File{
		Name: "NO.ZIP", Hash: fileHash(0x88), Size: 1, Uploader: "austin",
	}); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("a read-only area accepted an upload: %v", err)
	}
}

// A message area is not somewhere files go, even by name.
func TestAddFileRefusesAMessageArea(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw")

	if _, err := svc.AddFile(ctx, "general", store.File{
		Name: "WRONG.ZIP", Hash: fileHash(0x99), Size: 1, Uploader: "austin",
	}); !errors.Is(err, store.ErrWrongAreaKind) {
		t.Errorf("a message area accepted a file: %v", err)
	}
}

// The catalog entry a peer receives is the one this node wrote, byte for byte.
// Signatures cover the bytes, so anything else means the record cannot verify.
func TestCatalogEntrySurvivesTheWire(t *testing.T) {
	svc, st, ctx := testService(t)
	mkUser(t, svc, st, ctx, "austin", "pw", store.CapPostFederated)
	if _, err := st.CreateFileArea(ctx, "meshwide", "", true); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	svc.SetPublisher(pub)

	if _, err := svc.AddFile(ctx, "meshwide", store.File{
		Name: "WIRE.TAR.GZ", Hash: fileHash(0xAA), Size: 999_999,
		Description: "Round trips", Uploader: "austin",
	}); err != nil {
		t.Fatal(err)
	}

	wire := pub.recs[0].Marshal()
	back, err := record.Unmarshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	self, err := st.SelfNode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	body, err := record.VerifyFileRecord(back, self.PublicKey)
	if err != nil {
		t.Fatalf("the entry did not survive the wire: %v", err)
	}
	if body.Name != "WIRE.TAR.GZ" || body.Description != "Round trips" {
		t.Errorf("the entry came back as %+v", body)
	}
}
