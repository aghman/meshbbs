package dictcorpus

import (
	"strings"
	"testing"
)

// minima are what internal/bundle's compression gate slices out of each half.
// Below these it panics on a slice bound rather than reporting anything useful,
// which is how a CRLF checkout on Windows presented itself: the forum file
// parsed into ONE post because the separator stopped matching, and the first
// symptom was a bounds panic in another package.
const (
	minPosts = 10 // the largest post batch the gate builds
	minFiles = 6  // the largest catalog batch
	minDoors = 1
)

func TestCorpusShape(t *testing.T) {
	for _, half := range []string{"train", "holdout"} {
		s, err := load(half)
		if err != nil {
			t.Fatalf("%s: %v", half, err)
		}
		t.Logf("%-8s posts=%d files=%d doors=%d", half, len(s.Posts), len(s.Files), len(s.Doors))

		if len(s.Posts) < minPosts {
			t.Errorf("%s: %d posts, need at least %d — if this is 1, the post separator "+
				"stopped matching and the whole file parsed as a single post (line endings?)",
				half, len(s.Posts), minPosts)
		}
		if len(s.Files) < minFiles {
			t.Errorf("%s: %d catalog entries, need at least %d", half, len(s.Files), minFiles)
		}
		if len(s.Doors) < minDoors {
			t.Errorf("%s: %d door batches, need at least %d", half, len(s.Doors), minDoors)
		}
	}
}

// TestCorpusIsLF guards the bytes the dictionary was trained on.
//
// .gitattributes marks data/ as -text and readText normalises anyway, so this
// should be unreachable. It is here because the consequence is silent: a corpus
// carrying carriage returns is a different corpus, and it would retrain a
// different dictionary and measure a different ratio without failing anything.
func TestCorpusIsLF(t *testing.T) {
	for _, half := range []string{"train", "holdout"} {
		for _, name := range []string{"forum.txt", "files.txt", "doors.txt"} {
			raw, err := readText(half, name)
			if err != nil {
				t.Fatalf("%s/%s: %v", half, name, err)
			}
			if strings.Contains(raw, "\r") {
				t.Errorf("%s/%s contains a carriage return after normalisation", half, name)
			}
		}
	}
}

// TestTrainAndHoldoutAreDisjoint is the invariant every quoted ratio rests on.
//
// data/README.md says the two halves share a register and no content. If they
// ever share content the holdout figure stops being a measurement of
// generalisation and starts being a measurement of memory — and it would move in
// the flattering direction, which is the kind of error nobody goes looking for.
func TestTrainAndHoldoutAreDisjoint(t *testing.T) {
	train, err := Train()
	if err != nil {
		t.Fatal(err)
	}
	holdout, err := Holdout()
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, r := range train.All() {
		seen[string(r.Body)] = true
	}
	for _, r := range holdout.All() {
		if seen[string(r.Body)] {
			t.Errorf("a %s record body appears in both halves:\n%.120q", r.Type, r.Body)
		}
	}

	// Whole-body equality is the floor, not the bar: two posts differing only in
	// a trailing newline would pass it while being the same sample for every
	// purpose that matters. Long shared lines are the practical signal, and the
	// threshold is well above the boilerplate the register legitimately shares
	// ("73 de", "> " quoting, a sign-off).
	const longLine = 40
	trainLines := map[string]bool{}
	for _, r := range train.Posts {
		for _, line := range strings.Split(string(r.Body), "\n") {
			if line = strings.TrimSpace(line); len(line) >= longLine {
				trainLines[line] = true
			}
		}
	}
	for _, r := range holdout.Posts {
		for _, line := range strings.Split(string(r.Body), "\n") {
			if line = strings.TrimSpace(line); len(line) >= longLine && trainLines[line] {
				t.Errorf("a %d-character line appears in both halves:\n%q", len(line), line)
			}
		}
	}
}
