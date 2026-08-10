package cli

import (
	"strings"
	"testing"
)

// The three kinds must all be creatable from the CLI, share one name space, and
// say what they are.
func TestAreaCreateKinds(t *testing.T) {
	dir := initInstance(t)

	out, err := run(t, "--data-dir", dir, "area", "create", "league", "--kind", "door")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "door league") {
		t.Errorf("creation did not name the kind: %s", out)
	}
	if !strings.Contains(out, "door events only") {
		t.Errorf("creation did not say what a league accepts: %s", out)
	}

	// --files predates --kind and must keep working.
	if _, err := run(t, "--data-dir", dir, "area", "create", "utils", "--files"); err != nil {
		t.Fatalf("the deprecated --files alias broke: %v", err)
	}
	// ...but not together with the flag that replaced it.
	if _, err := run(t, "--data-dir", dir, "area", "create", "x", "--files", "--kind", "door"); err == nil {
		t.Error("--files and --kind were accepted together")
	}

	if _, err := run(t, "--data-dir", dir, "area", "create", "y", "--kind", "sandwich"); err == nil {
		t.Error("an unknown kind was accepted")
	}

	// One tag namespace across all three kinds (migrations 0005, 0007).
	if _, err := run(t, "--data-dir", dir, "area", "create", "league", "--kind", "message"); err == nil {
		t.Error("a message area reused a door league's name")
	}

	list, err := run(t, "--data-dir", dir, "area", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"league", "door", "utils", "file"} {
		if !strings.Contains(list, want) {
			t.Errorf("area list is missing %q:\n%s", want, list)
		}
	}
}
