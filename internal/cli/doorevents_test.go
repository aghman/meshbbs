package cli

import (
	"strings"
	"testing"
)

// A league carried with no door installed must be visible and must say what it
// costs. Nothing else on the BBS would ever mention it: a league's records are
// interpreted by a door, so a node holding records no door can read has no
// other surface that shows them.
func TestDoorEventsNamesLeaguesWithNoDoor(t *testing.T) {
	dir := initInstance(t)

	if _, err := run(t, "--data-dir", dir, "area", "create", "lordleague", "--kind", "door"); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "--data-dir", dir, "area", "federate", "lordleague"); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "--data-dir", dir, "door", "events")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "lordleague") {
		t.Errorf("the league is missing from the summary:\n%s", out)
	}
	if !strings.Contains(out, "(none installed)") {
		t.Errorf("a league with no door was not flagged:\n%s", out)
	}
	// The cost has to be stated, not implied. Carrying a league means serving
	// its records to peers that ask.
	if !strings.Contains(out, "served to peers") {
		t.Errorf("the airtime cost of carrying a league is not explained:\n%s", out)
	}

	// Installing a door pointed at it stops the warning.
	if _, err := run(t, "--data-dir", dir, "door", "add", "lord", "/bin/echo",
		"--api-level", "3", "--league-area", "lordleague"); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, "--data-dir", dir, "door", "events")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "(none installed)") {
		t.Errorf("a league with a door is still flagged as unplayed:\n%s", out)
	}
}

// With no leagues at all the command says so and says how to make one, rather
// than printing an empty table.
func TestDoorEventsWithNoLeagues(t *testing.T) {
	dir := initInstance(t)
	out, err := run(t, "--data-dir", dir, "door", "events")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No door leagues") {
		t.Errorf("output does not say there are none:\n%s", out)
	}
	if !strings.Contains(out, "--kind door") {
		t.Errorf("output does not say how to create one:\n%s", out)
	}
}

// Naming an area that is not a league is refused, and says what it actually is.
func TestDoorEventsRefusesANonLeague(t *testing.T) {
	dir := initInstance(t)
	out, err := run(t, "--data-dir", dir, "door", "events", "general")
	if err == nil {
		t.Fatalf("listing a forum as a league succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "message area") {
		t.Errorf("error does not say what the area is: %v", err)
	}
}
