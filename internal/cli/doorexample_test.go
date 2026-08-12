package cli

import (
	"strings"
	"testing"
)

// `door examples` is the command a sysop runs to find out whether the plumbing
// is right, so what it says about a league it cannot use matters as much as what
// it installs. Every test here is about that: a grant that could never work has
// to be refused at install time, not discovered by a player on a winter evening.

// The arena door is installed with its league grant, and the grant is the one
// the sysop named.
func TestDoorExamplesInstallsTheLeagueDoor(t *testing.T) {
	dir := initInstance(t)

	if _, err := run(t, "--data-dir", dir, "area", "create", "arena", "--kind", "door", "--federated"); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "--data-dir", dir, "door", "examples", "--league-area", "arena")
	if err != nil {
		t.Fatalf("installing the examples: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Installed arena") {
		t.Errorf("the league door was not installed:\n%s", out)
	}

	show, err := run(t, "--data-dir", dir, "door", "show", "arena")
	if err != nil {
		t.Fatal(err)
	}
	if field(show, "league_area") != "arena" {
		t.Errorf("the door's league area is %q, want arena:\n%s", field(show, "league_area"), show)
	}
	// A per-hour of zero refuses every emit, so a door installed with a league
	// and no budget would be silently unable to report anything.
	if perHour := field(show, "league_per_hour"); perHour == "0" || perHour == "" {
		t.Errorf("the league door may report %q events an hour:\n%s", perHour, show)
	}
}

// With no league named, the door still installs and the command says what is
// missing — the same shape the announce area has, because it is the same
// situation: the sysop has not chosen a destination.
func TestDoorExamplesExplainsAMissingLeague(t *testing.T) {
	dir := initInstance(t)

	out, err := run(t, "--data-dir", dir, "door", "examples")
	if err != nil {
		t.Fatalf("installing without a league: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Installed arena") {
		t.Errorf("the league door was not installed without a league:\n%s", out)
	}
	if !strings.Contains(out, "--league-area") {
		t.Errorf("the command does not say how to give it a league:\n%s", out)
	}
	// And it has to say which KIND of area, because the announce area two
	// paragraphs above must be local and this one must be federated. A sysop who
	// mixes them up gets a door that cannot report and no clue why.
	if !strings.Contains(out, "--kind door --federated") {
		t.Errorf("the command does not say what kind of area a league is:\n%s", out)
	}
}

// field reads one value out of `door show`, which is a tabwriter table of
// name/value lines. Read by name rather than matched as a substring, so that a
// column width change is not a test failure and a wrong value is.
func field(show, name string) string {
	for _, line := range strings.Split(show, "\n") {
		rest, ok := strings.CutPrefix(line, name)
		if !ok || (rest != "" && !strings.HasPrefix(rest, " ")) {
			continue
		}
		return strings.TrimSpace(rest)
	}
	return ""
}

// A league that could never work is refused before anything is installed, and
// each refusal names its own remedy: a missing area, an area of the wrong kind,
// and a door area that is not on the mesh are three different mistakes.
func TestDoorExamplesRefusesALeagueThatCannotWork(t *testing.T) {
	t.Run("no such area", func(t *testing.T) {
		dir := initInstance(t)
		out, err := run(t, "--data-dir", dir, "door", "examples", "--league-area", "nowhere")
		if err == nil {
			t.Fatalf("a league pointing at nothing was accepted:\n%s", out)
		}
		if !strings.Contains(err.Error(), "--kind door --federated") {
			t.Errorf("the refusal does not say how to make one: %v", err)
		}
		// Nothing was installed, so a mistyped flag leaves the doors table as it
		// was rather than half-updated.
		if list, _ := run(t, "--data-dir", dir, "door", "list"); strings.Contains(list, "arena") {
			t.Errorf("a refused install still wrote the doors table:\n%s", list)
		}
	})

	t.Run("a forum", func(t *testing.T) {
		dir := initInstance(t)
		if _, err := run(t, "--data-dir", dir, "area", "create", "chat"); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, "--data-dir", dir, "door", "examples", "--league-area", "chat")
		if err == nil {
			t.Fatal("a forum was accepted as a league")
		}
		if !strings.Contains(err.Error(), "wrong kind") {
			t.Errorf("the refusal does not say the name is spent on something else: %v", err)
		}
	})

	t.Run("local only", func(t *testing.T) {
		dir := initInstance(t)
		if _, err := run(t, "--data-dir", dir, "area", "create", "arena", "--kind", "door"); err != nil {
			t.Fatal(err)
		}
		_, err := run(t, "--data-dir", dir, "door", "examples", "--league-area", "arena")
		if err == nil {
			t.Fatal("a local-only door area was accepted as a league")
		}
		if !strings.Contains(err.Error(), "area federate") {
			t.Errorf("the refusal does not say how to put it on the mesh: %v", err)
		}
	})
}
