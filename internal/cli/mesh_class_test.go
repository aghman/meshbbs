package cli

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aghman/meshbbs/internal/governor"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/link"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

// classLink is the outbox's link, recording the priority class it was handed.
type classLink struct {
	sent    int
	classes []governor.Class
}

func (l *classLink) MTU() int { return 233 }

func (l *classLink) Send(ctx context.Context, to identity.NodeID, payload []byte) error {
	return l.SendClass(ctx, to, payload, governor.ClassForum)
}

func (l *classLink) SendClass(ctx context.Context, to identity.NodeID, payload []byte, class governor.Class) error {
	l.sent++
	l.classes = append(l.classes, class)
	return nil
}

func (l *classLink) Budget() link.Budget { return link.Budget{Available: time.Hour} }

func classTestRecords(t *testing.T, key identity.NodeKey, area record.AreaTag, n int) []*record.Record {
	t.Helper()
	var out []*record.Record
	for i := 1; i <= n; i++ {
		r, err := record.New(key, record.Record{
			Origin: key.ID(), Seq: uint64(i), TS: uint32(1_800_000_000 + i),
			Type: record.TypePost, Area: area, Body: []byte("body text"),
		})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// §7.6: the roster is control traffic, mail is DM traffic, a file area is
// catalog traffic at the bottom of the order, and everything else is a forum
// post. The mapping is asserted directly because it is policy a reader should
// be able to check against the design without running a mesh.
func TestClassifyArea(t *testing.T) {
	utils := record.AreaTagFor("utils")
	classify := classifierFor(func(a record.AreaTag) bool { return a == utils })

	for _, c := range []struct {
		name string
		area record.AreaTag
		want governor.Class
	}{
		{"roster", store.RosterArea, governor.ClassControl},
		{"mail", record.DMArea, governor.ClassDM},
		{"forum", record.AreaTagFor("general"), governor.ClassForum},
		{"directory", record.ProfileArea, governor.ClassForum},
		{"file area", utils, governor.ClassFileCatalog},
	} {
		if got := classify(c.area); got != c.want {
			t.Errorf("%s area classified %v, want %v", c.name, got, c.want)
		}
	}
}

// A file area is only catalog traffic because something said so. The tag is a
// hash of a name and carries no kind, so a classifier with no lookup must fall
// back to forum rather than guess — under-pricing catalog traffic is a
// throughput question, mispricing anything else is a correctness one.
func TestClassifyWithoutAFileAreaLookup(t *testing.T) {
	classify := classifierFor(nil)
	if got := classify(record.AreaTagFor("utils")); got != governor.ClassForum {
		t.Errorf("with no lookup, an area classified %v, want ClassForum", got)
	}
	if got := classify(store.RosterArea); got != governor.ClassControl {
		t.Errorf("with no lookup, the roster classified %v, want ClassControl", got)
	}
}

// The gate this exists to close: §8.3's Part 97 refusal is written and unit
// tested, but it only fires on traffic classified ClassDM — and the outbox was
// built WITHOUT a classifier, whose default calls everything forum traffic. So
// the running binary would have put mail on the air under an amateur licence
// while every test of the block passed.
//
// This drives the real production wiring: the classifier `serve` installs, the
// real outbox, and the real compression dictionary.
func TestProductionWiringFiresTheHamModeRefusal(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(7))
	if err != nil {
		t.Fatal(err)
	}
	dict, _, err := federationDictionaries()
	if err != nil {
		t.Fatal(err)
	}
	fl := &classLink{}
	// The sysop log, so the refusal can be checked where a sysop would read it
	// (§8.3 requires it to be said clearly rather than dropped silently).
	var sysopLog bytes.Buffer

	// The production constructor, not a hand-rolled config: the defect was a
	// missing field in exactly this call, so a test that supplied its own
	// config would have gone on passing.
	out, err := federationOutbox(key.ID(), fl, dict,
		// No file areas in this fixture; the catalog arm has its own test.
		func(record.AreaTag) bool { return false },
		// Ham mode: the radio reports is_licensed, so encryption is off limits.
		func() bool { return false },
		slog.New(slog.NewTextHandler(&sysopLog, nil)))
	if err != nil {
		t.Fatal(err)
	}

	if err := out.SendRecords(record.DMArea, classTestRecords(t, key, record.DMArea, 3)); err != nil {
		t.Fatal(err)
	}
	if fl.sent != 0 {
		t.Fatalf("%d mail symbols went on the air under an amateur licence", fl.sent)
	}
	if !strings.Contains(sysopLog.String(), "mail held back") {
		t.Errorf("the refusal never reached the sysop log: %q", sysopLog.String())
	}
	if out.Stats().RefusedHamMode != 1 {
		t.Errorf("RefusedHamMode = %d, want 1", out.Stats().RefusedHamMode)
	}

	// The other half of §8.3: signing is fine, only confidentiality is the
	// problem. A ham-mode node federates public traffic normally, and the
	// roster gets the control priority that keeps it converging.
	general := record.AreaTagFor("general")
	if err := out.SendRecords(general, classTestRecords(t, key, general, 3)); err != nil {
		t.Fatal(err)
	}
	if fl.sent == 0 {
		t.Fatal("public forum traffic was blocked in ham mode")
	}
	for _, c := range fl.classes {
		if c != governor.ClassForum {
			t.Errorf("forum bundle sent as %v", c)
		}
	}

	fl.classes = nil
	if err := out.SendRecords(store.RosterArea, classTestRecords(t, key, store.RosterArea, 2)); err != nil {
		t.Fatal(err)
	}
	if len(fl.classes) == 0 {
		t.Fatal("the roster was not transmitted")
	}
	for _, c := range fl.classes {
		if c != governor.ClassControl {
			t.Errorf("roster bundle sent as %v, want control — under backpressure it "+
				"would be dropped with the forum traffic", c)
		}
	}
}

// The file-catalog arm through the real outbox.
//
// Written the same way as the ham-mode test above and for the same reason: the
// defect class here is a policy that is correct in a unit test and never
// installed in the binary. Asserting the class that reaches the LINK is the
// only version of this that would have caught the classifier being unset.
func TestProductionWiringPricesAFileCatalog(t *testing.T) {
	key, err := identity.GenerateNodeKey(rng.TestSecret(8))
	if err != nil {
		t.Fatal(err)
	}
	dict, _, err := federationDictionaries()
	if err != nil {
		t.Fatal(err)
	}
	fl := &classLink{}

	utils := record.AreaTagFor("utils")
	out, err := federationOutbox(key.ID(), fl, dict,
		func(a record.AreaTag) bool { return a == utils },
		func() bool { return true },
		slog.New(slog.NewTextHandler(new(bytes.Buffer), nil)))
	if err != nil {
		t.Fatal(err)
	}

	if err := out.SendRecords(utils, classTestRecords(t, key, utils, 2)); err != nil {
		t.Fatal(err)
	}
	if fl.sent == 0 {
		t.Fatal("nothing was sent for a federated file area")
	}
	for i, c := range fl.classes {
		if c != governor.ClassFileCatalog {
			t.Fatalf("symbol %d went out as %v, want ClassFileCatalog", i, c)
		}
	}

	// A forum area through the same outbox still prices as a forum post, so the
	// arm is selective rather than a blanket downgrade.
	before := len(fl.classes)
	general := record.AreaTagFor("general")
	if err := out.SendRecords(general, classTestRecords(t, key, general, 2)); err != nil {
		t.Fatal(err)
	}
	for i, c := range fl.classes[before:] {
		if c != governor.ClassForum {
			t.Fatalf("forum symbol %d went out as %v, want ClassForum", i, c)
		}
	}
}
