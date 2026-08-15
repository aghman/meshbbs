package bundle

import (
	"testing"

	"github.com/aghman/meshbbs/internal/dictcorpus"
	"github.com/aghman/meshbbs/internal/record"
)

// §12.7 asks for a compression ratio gate against a fixed corpus, "so a
// dictionary change that helps one case and hurts overall gets caught".
//
// Every number here is measured on internal/dictcorpus's HOLDOUT half, which
// tools/traindict never sees. Measuring on the training half would report how
// well the dictionary memorised its corpus — at a 64 KiB budget that reads as
// 3.19x against a real 1.41x, so the distinction is not academic.
//
// Thresholds sit a little under the measured values. They are regression alarms,
// not targets: the point is to notice a change that makes things worse, and a
// threshold set exactly at today's number would flake on any harmless reordering.

// ratioFor compresses one bundle shape and reports body-in over packed-out.
func ratioFor(t *testing.T, d *Dictionary, area string, recs []*record.Record) (float64, int, int) {
	t.Helper()
	b := &Bundle{Area: record.AreaTagFor(area), BaseTS: 1700000000, Records: recs}
	body, err := EncodeBody(b)
	if err != nil {
		t.Fatalf("encode body: %v", err)
	}
	packed, err := d.Compress(body)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	return float64(len(body)) / float64(len(packed)), len(body), len(packed)
}

func holdoutSet(t *testing.T) *dictcorpus.Set {
	t.Helper()
	s, err := dictcorpus.Holdout()
	if err != nil {
		t.Fatalf("load holdout corpus: %v", err)
	}
	return s
}

func ratioDicts(t *testing.T) (none, d0, d1 *Dictionary) {
	t.Helper()
	var err error
	if none, err = NewRawDictionary(9, nil); err != nil {
		t.Fatalf("plain zstd: %v", err)
	}
	if d0, err = Dictionary0(); err != nil {
		t.Fatalf("dictionary 0: %v", err)
	}
	if d1, err = Dictionary1(); err != nil {
		t.Fatalf("dictionary 1: %v", err)
	}
	t.Cleanup(func() { none.Close(); d0.Close(); d1.Close() })
	return none, d0, d1
}

// TestDictionary1BeatsItsPredecessors is the headline gate.
//
// Dictionary 0 exists and buys about 1% over plain zstd, which is why §7.4 spent
// four versions asking for a trained one. If a future dictionary cannot clear
// both of those on unseen data it is not worth the wire-format commitment.
func TestDictionary1BeatsItsPredecessors(t *testing.T) {
	h := holdoutSet(t)
	none, d0, d1 := ratioDicts(t)

	shapes := []struct {
		name string
		area string
		recs []*record.Record
	}{
		{"posts-1", "general", h.Posts[:1]},
		{"posts-5", "general", h.Posts[:5]},
		{"posts-all", "general", h.Posts},
		{"files-1", "files/uploads", h.Files[:1]},
		{"files-5", "files/uploads", h.Files[:5]},
		{"doors-1", "league/x", h.Doors[:1]},
	}

	for _, s := range shapes {
		rNone, body, _ := ratioFor(t, none, s.area, s.recs)
		r0, _, _ := ratioFor(t, d0, s.area, s.recs)
		r1, _, packed1 := ratioFor(t, d1, s.area, s.recs)

		t.Logf("%-10s body=%-5d plain=%.3f dict0=%.3f dict1=%.3f", s.name, body, rNone, r0, r1)

		if r1 < rNone {
			t.Errorf("%s: dictionary 1 (%.3fx) is worse than plain zstd (%.3fx)", s.name, r1, rNone)
		}
		if r1 < r0 {
			t.Errorf("%s: dictionary 1 (%.3fx) is worse than dictionary 0 (%.3fx)", s.name, r1, r0)
		}
		// A compressor that makes a packet bigger is worse than useless on a link
		// where §1.1 counts every byte. Plain zstd does exactly that to a lone
		// catalog entry, and rescuing that case is most of why a dictionary is
		// worth carrying at all.
		if packed1 > body {
			t.Errorf("%s: dictionary 1 EXPANDS the bundle, %d -> %d bytes", s.name, body, packed1)
		}
	}
}

// TestDictionary1RatioFloor pins the per-shape ratios.
//
// Separate from the comparison above because the two catch different things: a
// change can keep dictionary 1 ahead of its predecessors while making everything
// worse, which is the "helps one case and hurts overall" failure §12.7 names.
func TestDictionary1RatioFloor(t *testing.T) {
	h := holdoutSet(t)
	_, _, d1 := ratioDicts(t)

	cases := []struct {
		name string
		area string
		recs []*record.Record
		min  float64
	}{
		{"posts-1", "general", h.Posts[:1], 1.18},
		{"posts-5", "general", h.Posts[:5], 1.39},
		{"files-1", "files/uploads", h.Files[:1], 1.05},
		{"files-5", "files/uploads", h.Files[:5], 1.12},
		{"doors-1", "league/x", h.Doors[:1], 1.51},
	}

	for _, c := range cases {
		got, body, packed := ratioFor(t, d1, c.area, c.recs)
		if got < c.min {
			t.Errorf("%s: %.3fx (%d -> %d bytes), floor is %.3fx", c.name, got, body, packed, c.min)
		}
	}
}

// TestDictionary1OverallRatio is the aggregate, over every bundle shape the
// corpus produces rather than the handful sampled above.
//
// Total-in over total-out, not a mean of per-bundle ratios: those are different
// numbers, and the mean flatters small bundles, which dominate by count and not
// by bytes.
func TestDictionary1OverallRatio(t *testing.T) {
	h := holdoutSet(t)
	none, _, d1 := ratioDicts(t)

	// The same groups and the same batch sizes tools/traindict samples, so the
	// gate reports the number the trainer reports rather than a second "overall"
	// ratio differing only by weighting. Single-record post bundles appear twice
	// because §1.1's ten-packets-a-day budget makes a bundle of one the common
	// case, and a mix weighting a ten-post batch equally would be describing a
	// busier network than this one is.
	var bodyTotal, plainTotal, dictTotal int
	for _, group := range []struct {
		area  string
		recs  []*record.Record
		sizes []int
	}{
		{"general", h.Posts, []int{1, 1, 2, 3, 5, 10}},
		{"files/uploads", h.Files, []int{1, 3, 6}},
		{"league/x", h.Doors, []int{1}},
	} {
		for _, size := range group.sizes {
			for i := 0; i+size <= len(group.recs); i += size {
				b := &Bundle{
					Area: record.AreaTagFor(group.area), BaseTS: 1700000000,
					Records: group.recs[i : i+size],
				}
				body, err := EncodeBody(b)
				if err != nil {
					t.Fatal(err)
				}
				plain, err := none.Compress(body)
				if err != nil {
					t.Fatal(err)
				}
				packed, err := d1.Compress(body)
				if err != nil {
					t.Fatal(err)
				}
				bodyTotal += len(body)
				plainTotal += len(plain)
				dictTotal += len(packed)
			}
		}
	}

	plain := float64(bodyTotal) / float64(plainTotal)
	dict := float64(bodyTotal) / float64(dictTotal)
	t.Logf("holdout overall: %d bytes -> plain %.3fx, dictionary 1 %.3fx (%.1f%% fewer bytes than plain)",
		bodyTotal, plain, dict, 100*(1-float64(dictTotal)/float64(plainTotal)))

	const floor = 1.28
	if dict < floor {
		t.Errorf("overall holdout ratio is %.3fx, floor is %.3fx", dict, floor)
	}
	if dict <= plain {
		t.Errorf("dictionary 1 (%.3fx) does not beat plain zstd (%.3fx) overall", dict, plain)
	}
}

// TestDictionary1RoundTrips is the correctness half. A ratio is worthless if the
// bytes do not come back.
func TestDictionary1RoundTrips(t *testing.T) {
	h := holdoutSet(t)
	set, err := DefaultDictionarySet()
	if err != nil {
		t.Fatalf("default dictionary set: %v", err)
	}
	defer set.Close()

	d1, err := Dictionary1()
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()

	b := &Bundle{Area: record.AreaTagFor("general"), BaseTS: 1700000000, Records: h.Posts[:5], DictID: 1}
	packed, err := Pack(b, d1)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if packed[1] != 1 {
		t.Errorf("bundle header records dictionary %d, want 1", packed[1])
	}
	got, err := Unpack(packed, set)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if len(got.Records) != 5 {
		t.Fatalf("round trip returned %d records, want 5", len(got.Records))
	}
	for i, r := range got.Records {
		if r.ID() != h.Posts[i].ID() {
			t.Errorf("record %d: id %s, want %s", i, r.ID(), h.Posts[i].ID())
		}
	}
}

// TestDefaultDictionarySetHoldsZero is the compatibility promise as a test.
//
// §7.4 says old dictionaries stay supported, and the conformance corpus pins a
// bundle packed against dictionary 0. Dropping it from the read set would leave
// this build unable to decode its own history.
func TestDefaultDictionarySetHoldsZero(t *testing.T) {
	set, err := DefaultDictionarySet()
	if err != nil {
		t.Fatal(err)
	}
	defer set.Close()

	for _, id := range []uint8{0, 1} {
		if _, err := set.Get(id); err != nil {
			t.Errorf("default set cannot read dictionary %d: %v", id, err)
		}
	}
}

// TestDefaultDictionaryIsOne states the current writing choice out loud.
//
// Not a tautology: changing which dictionary a node compresses with is a
// flag-day decision while nothing negotiates them (see DefaultDictionary), so it
// should take editing a test that says so.
func TestDefaultDictionaryIsOne(t *testing.T) {
	d, err := DefaultDictionary()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.ID() != 1 {
		t.Errorf("nodes compress with dictionary %d, expected 1", d.ID())
	}
}
