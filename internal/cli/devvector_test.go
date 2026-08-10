package cli

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

// countsByArea parses `dev vector` output into area name -> count.
func countsByArea(t *testing.T, out string) map[string]uint64 {
	t.Helper()
	counts := map[string]uint64{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, " ") || !strings.Contains(line, "count=") {
			continue
		}
		fields := strings.Fields(line)
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "count=") {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimPrefix(f, "count="), 10, 64)
			if err != nil {
				t.Fatalf("parsing %q: %v", line, err)
			}
			counts[fields[0]] = n
		}
	}
	return counts
}

func seedAndFederate(t *testing.T, seed string, posts int) string {
	t.Helper()
	dir := initInstance(t)
	if _, err := run(t, "--data-dir", dir, "dev", "seed",
		"--seed", seed, "--users", "3", "--posts", strconv.Itoa(posts)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, area := range seededAreas {
		if _, err := run(t, "--data-dir", dir, "area", "federate", area); err != nil {
			t.Fatalf("federate %s: %v", area, err)
		}
	}
	return dir
}

// The vector must account for every record the instance actually holds.
//
// This is the assertion the bench rests on. A node that under-reports its own
// state converges to a number both sides agree on and both sides have wrong,
// which is indistinguishable from success in a diff. The roster is checked
// separately rather than folded into the total, because it holds this node's
// own NODE record and sweeping it into the post count would hide an off-by-one
// in either place.
func TestDevVectorAccountsForEverySeededPost(t *testing.T) {
	const posts = 30
	dir := seedAndFederate(t, "1", posts)

	out, err := run(t, "--data-dir", dir, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	counts := countsByArea(t, out)

	var total uint64
	for _, area := range seededAreas {
		n, ok := counts[area]
		if !ok {
			t.Errorf("area %q is missing from the vector; its seeded posts cannot federate", area)
			continue
		}
		if n == 0 {
			t.Errorf("area %q federates but holds nothing", area)
		}
		total += n
	}
	if total != posts {
		t.Errorf("seeded areas hold %d records, want all %d seeded posts", total, posts)
	}
	// The roster carries this node's own NODE record and must not be swept into
	// the post count above.
	if got := counts["roster"]; got != 1 {
		t.Errorf("roster count = %d, want 1 (this node's own NODE record)", got)
	}
}

// addForeignOrigins writes records signed by other node keys, the way a
// completed sync leaves them.
//
// Needed because a freshly seeded instance holds exactly one origin — its own —
// and a single-entry vector is sorted no matter what Origins does. The property
// under test only exists once several origins share an area, which is precisely
// the state the bench is trying to reach.
func addForeignOrigins(t *testing.T, dir string, n, perOrigin int) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "bbs.db"), clock.NewReal())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < n; i++ {
		key, err := identity.GenerateNodeKey(rng.NewSecret())
		if err != nil {
			t.Fatal(err)
		}
		for seq := 1; seq <= perOrigin; seq++ {
			r, err := record.New(key, record.Record{
				Seq:  uint64(seq),
				TS:   uint32(1765310400 + seq),
				Type: record.TypePost,
				Area: record.AreaTagFor("general"),
				Body: []byte("from a peer"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.PutRecord(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The output is a diff target, so identical state must produce identical bytes.
//
// Version vectors are built from a map, and Go randomises map iteration
// (§6.2.1 rule 2). Vector.Origins sorts for exactly this reason; if that ever
// regresses, two converged nodes would diff as divergent and the bench would
// chase a protocol bug that does not exist. Several origins are planted first,
// because with one the ordering is trivially stable and this proves nothing.
func TestDevVectorOutputIsStable(t *testing.T) {
	dir := seedAndFederate(t, "1", 20)
	addForeignOrigins(t, dir, 6, 3)

	first, err := run(t, "--data-dir", dir, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(first, "origin="); got < 7 {
		t.Fatalf("expected several origins to order, got %d lines", got)
	}
	for i := 0; i < 10; i++ {
		again, err := run(t, "--data-dir", dir, "dev", "vector")
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("output changed between runs on unchanged state:\n%s\n---\n%s", first, again)
		}
	}
}

// Two instances seeded differently must diverge, or the bring-up has nothing to
// reconcile and a green run proves nothing.
func TestDevVectorShowsDivergenceBetweenSeeds(t *testing.T) {
	a := seedAndFederate(t, "1", 20)
	b := seedAndFederate(t, "2", 20)

	outA, err := run(t, "--data-dir", a, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	outB, err := run(t, "--data-dir", b, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	if outA == outB {
		t.Fatal("two differently seeded instances produced identical vectors")
	}

	// Same areas on both sides — they must be comparable — but different
	// contents, which is the divergence anti-entropy has to close.
	ca, cb := countsByArea(t, outA), countsByArea(t, outB)
	for _, area := range seededAreas {
		if _, ok := cb[area]; !ok {
			t.Errorf("area %q present on A and missing on B", area)
		}
	}
	same := true
	for _, area := range seededAreas {
		if ca[area] != cb[area] {
			same = false
		}
	}
	if same {
		t.Error("per-area counts are identical; the seeds are not producing divergence")
	}
}

// Before an area federates it must not appear, and the command must say why the
// output is nearly empty — "nothing federates yet" and "the sync is broken"
// otherwise produce the same diff.
func TestDevVectorExplainsAnUnfederatedInstance(t *testing.T) {
	dir := initInstance(t)
	if _, err := run(t, "--data-dir", dir, "dev", "seed", "--users", "2", "--posts", "5"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "--data-dir", dir, "dev", "vector")
	if err != nil {
		t.Fatal(err)
	}
	for _, area := range seededAreas {
		if strings.Contains(out, area+" tag=") {
			t.Errorf("area %q appears before it was federated", area)
		}
	}
	if !strings.Contains(out, "area federate") {
		t.Errorf("output does not explain how to federate an area:\n%s", out)
	}
}
