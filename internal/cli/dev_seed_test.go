package cli

import (
	"strings"
	"testing"
)

// seededAreas are the areas dev seed posts into. Named here so a test breaks if
// the seeder's list and the areas it creates ever drift apart again.
var seededAreas = []string{"general", "tech", "swap"}

// Every area dev seed posts into must exist as a row, not merely as a tag.
//
// None of them did. The seeder posted into general, tech and SWAP while
// SeedDefaultAreas created general, tech and SYSOP, and nothing connected the
// two lists. The failure is silent at the point it happens: NextSeq and
// PutRecord are keyed by tag and consult no area row, so a third of the seeded
// posts were written with a valid tag, a valid signature and nothing behind
// them. It surfaces two steps later, when a bench operator runs
// `area federate swap` and is told there is no such area — by which point the
// instance is seeded and the natural move is to skip that area and carry on
// with a corpus a third smaller than the one that was asked for.
func TestDevSeedCreatesTheAreasItPostsInto(t *testing.T) {
	dir := initInstance(t)
	if _, err := run(t, "--data-dir", dir, "dev", "seed", "--users", "3", "--posts", "30"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	out, err := run(t, "--data-dir", dir, "area", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, area := range seededAreas {
		if !strings.Contains(out, area) {
			t.Errorf("area %q holds seeded posts but has no row:\n%s", area, out)
		}
		// Federating is the operation the bench actually performs, and the one
		// that failed. Asserting it directly is what makes this a regression
		// test rather than a restatement of the fix.
		if _, err := run(t, "--data-dir", dir, "area", "federate", area); err != nil {
			t.Errorf("area federate %s: %v", area, err)
		}
	}
}
