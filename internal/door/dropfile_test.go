package door

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func dropfileSession() Session {
	return Session{
		Nick: "alice", RealName: "Alice Anderson", Node: 3,
		Width: 80, Height: 25, ANSI: true,
		Location:  "Portland, OR",
		BBSName:   "Fog City",
		SysopName: "Sam Sysop",
		TimeRemaining: func() time.Duration {
			return 42 * time.Minute
		},
	}
}

func writeTestDropfile(t *testing.T, format string, sess Session) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	spec := Spec{Name: "tw", Dir: "/opt/doors/tw", Dropfile: format, WallClock: time.Hour}
	path, err := writeDropfile(dir, spec, sess)
	if err != nil {
		t.Fatalf("write %s: %v", format, err)
	}
	if path == "" {
		t.Fatalf("%s produced no file", format)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Every one of these formats is CRLF, and a door reading them with DOS
	// line-reading gets one field per line only if we wrote them that way.
	text := string(body)
	if strings.Contains(strings.ReplaceAll(text, "\r\n", ""), "\n") {
		t.Errorf("%s contains a bare newline; these formats are CRLF", format)
	}
	got := strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n")
	return path, got
}

func TestDropfileFormatsHaveTheirFixedShape(t *testing.T) {
	for _, format := range []string{DropfileDoorSys, DropfileDoor32, DropfileDorinfo1} {
		t.Run(format, func(t *testing.T) {
			path, got := writeTestDropfile(t, format, dropfileSession())
			if want := dropfileLineCount(format); len(got) != want {
				t.Errorf("%s has %d lines, want %d", format, len(got), want)
			}
			if base := filepath.Base(path); base != dropfileName(format) {
				t.Errorf("written as %q, want %q", base, dropfileName(format))
			}
			// It names a caller and says where they live, so it is not for
			// anyone else on the machine to read.
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("%s is mode %04o", format, perm)
			}
		})
	}
}

func TestDoorSysFields(t *testing.T) {
	_, got := writeTestDropfile(t, DropfileDoorSys, dropfileSession())

	// Positional, so these are asserted by index — which is the whole reason
	// the sanitiser exists.
	for _, tc := range []struct {
		line int // 1-based, as the format is documented
		want string
	}{
		{4, "3"},               // node
		{10, "Alice Anderson"}, // full name
		{11, "Portland, OR"},   // calling from
		{14, ""},               // password: never written
		{15, "50"},             // security level
		{19, "42"},             // minutes left
		{20, "GR"},             // graphics
		{35, "Sam Sysop"},      // sysop
		{36, "alice"},          // handle
	} {
		if idx := tc.line - 1; got[idx] != tc.want {
			t.Errorf("DOOR.SYS line %d is %q, want %q", tc.line, got[idx], tc.want)
		}
	}
}

// The password line exists and is empty, in every format that has one. It is
// present because the format is positional and removing it would shift
// everything below; it is empty because meshbbs does not have the password and
// would not hand it to a third-party binary if it did (§6.7).
func TestDropfileNeverCarriesAPassword(t *testing.T) {
	sess := dropfileSession()
	for _, format := range []string{DropfileDoorSys, DropfileDoor32, DropfileDorinfo1} {
		_, got := writeTestDropfile(t, format, sess)
		for i, line := range got {
			if strings.Contains(strings.ToLower(line), "password") {
				t.Errorf("%s line %d mentions a password: %q", format, i+1, line)
			}
		}
	}
	// And the one that has a slot for it left it blank.
	_, doorsys := writeTestDropfile(t, DropfileDoorSys, sess)
	if doorsys[13] != "" {
		t.Errorf("DOOR.SYS line 14 is %q; it must never carry a password", doorsys[13])
	}
}

func TestDoor32AndDorinfoFields(t *testing.T) {
	_, d32 := writeTestDropfile(t, DropfileDoor32, dropfileSession())
	if d32[3] != "Fog City" || d32[5] != "Alice Anderson" || d32[6] != "alice" {
		t.Errorf("DOOR32.SYS identity lines are %q", d32[3:7])
	}
	if d32[8] != "42" || d32[10] != "3" {
		t.Errorf("DOOR32.SYS time/node are %q and %q", d32[8], d32[10])
	}

	_, di := writeTestDropfile(t, DropfileDorinfo1, dropfileSession())
	if di[0] != "Fog City" || di[1] != "Sam" || di[2] != "Sysop" {
		t.Errorf("DORINFO1.DEF board lines are %q", di[0:3])
	}
	if di[6] != "Alice" || di[7] != "Anderson" {
		t.Errorf("DORINFO1.DEF split the caller's name as %q %q", di[6], di[7])
	}
}

// The hazard the whole file is written around: these formats are positional, so
// a newline in a value does not corrupt one field, it shifts every field after
// it — including the security level.
func TestANewlineInANameCannotShiftTheSecurityLevel(t *testing.T) {
	sess := dropfileSession()
	// What an attacker would try: end the name line early and write their own
	// security level onto the next one.
	sess.RealName = "Bob\r\n255\r\nEvil"
	sess.Location = "Nowhere\n999"

	_, got := writeTestDropfile(t, DropfileDoorSys, sess)
	if len(got) != dropfileLineCount(DropfileDoorSys) {
		t.Fatalf("the file has %d lines, want %d: the name shifted the format",
			len(got), dropfileLineCount(DropfileDoorSys))
	}
	if got[14] != strconv.Itoa(levelUser) {
		t.Errorf("security level is %q, want %q — a caller rewrote it",
			got[14], strconv.Itoa(levelUser))
	}
	if strings.Contains(got[9], "\r") || strings.Contains(got[9], "\n") {
		t.Errorf("the name line still holds a line break: %q", got[9])
	}
	// Sanitised, not deleted: the caller should still recognise themselves.
	if !strings.Contains(got[9], "Bob") {
		t.Errorf("the name was thrown away rather than made safe: %q", got[9])
	}
}

func TestSanitizeField(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Alice Anderson", "Alice Anderson"},
		{"Bob\r\n255", "Bob 255"},
		{"tab\there", "tab here"},
		{"bell\aand\x00nul", "bellandnul"},
		{"\x1b[31mred", "[31mred"},
		{"  padded  ", "padded"},
		// CRLF is two characters and these are fixed-width fields.
		{"Ann\r\n\r\nBeth", "Ann Beth"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sanitizeField(tc.in); got != tc.want {
			t.Errorf("sanitizeField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A dropfile has no way to say "no limit", and a door reading zero would show
// the caller a goodbye they have not earned.
func TestUnlimitedSessionsReportPlentyOfTime(t *testing.T) {
	sess := dropfileSession()
	sess.TimeRemaining = nil
	if got := minutesLeft(sess); got < 1000 {
		t.Errorf("an unlimited session reports %d minutes left", got)
	}

	sess.TimeRemaining = func() time.Duration { return -5 * time.Minute }
	if got := minutesLeft(sess); got != 0 {
		t.Errorf("an overrun session reports %d minutes left, want 0", got)
	}
}

func TestSecurityLevelProjection(t *testing.T) {
	sess := dropfileSession()
	if got := securityLevel(sess); got != levelUser {
		t.Errorf("a user is level %d, want %d", got, levelUser)
	}
	sess.Sysop = true
	if got := securityLevel(sess); got != levelSysop {
		t.Errorf("a sysop is level %d, want %d", got, levelSysop)
	}
	sess = dropfileSession()
	sess.Nick = ""
	if got := securityLevel(sess); got != levelGuest {
		t.Errorf("a guest is level %d, want %d", got, levelGuest)
	}
}

// A door asking for no dropfile gets none, rather than an empty one it might
// try to parse.
func TestNoDropfileMeansNoFile(t *testing.T) {
	dir := t.TempDir()
	path, err := writeDropfile(dir, Spec{Name: "tw", Dropfile: DropfileNone}, dropfileSession())
	if err != nil || path != "" {
		t.Fatalf("writeDropfile returned %q, %v", path, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a door wanting no dropfile got %d files", len(entries))
	}
}

func TestDropfileTokensReachTheDoor(t *testing.T) {
	path := "/tmp/mbdoor-x/DOOR.SYS"
	got := expandDropfileTokens(
		[]string{"-d", "{dropfile}", "--node-dir={dropfile_dir}", "plain"}, path)
	want := []string{"-d", path, "--node-dir=/tmp/mbdoor-x", "plain"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d is %q, want %q", i, got[i], want[i])
		}
	}

	// A door that asked for no dropfile keeps its arguments verbatim, tokens
	// and all: substituting an empty path would silently point it at "/".
	untouched := expandDropfileTokens([]string{"{dropfile}"}, "")
	if untouched[0] != "{dropfile}" {
		t.Errorf("with no dropfile the argument became %q", untouched[0])
	}

	env := dropfileEnv(path)
	if len(env) != 2 || !strings.HasPrefix(env[0], "MESHBBS_DROPFILE=") {
		t.Errorf("dropfile environment is %v", env)
	}
}

// A door with a dropfile and no API still gets a private directory, and the
// dropfile goes in it — and away again when the door ends.
func TestDropfileLivesAndDiesWithTheInvocation(t *testing.T) {
	mgr := New(realClock(), discardLogger())
	spec := Spec{
		Name: "tw", Path: mustExecutable(t), Dir: t.TempDir(),
		Dropfile: DropfileDoor32, WallClock: time.Minute,
		Args: []string{"{dropfile}"},
	}
	inv, err := mgr.startAPI(&spec, dropfileSession())
	if err != nil {
		t.Fatal(err)
	}
	if inv == nil {
		t.Fatal("a door wanting a dropfile got no private directory")
	}

	path := envValue(spec.Env, "MESHBBS_DROPFILE")
	if path == "" {
		t.Fatal("the door was not told where its dropfile is")
	}
	if spec.Args[0] != path {
		t.Errorf("the {dropfile} argument is %q, want %q", spec.Args[0], path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the dropfile was not written: %v", err)
	}
	// No API was configured, so there is no socket — §9.1 calls it optional.
	if inv.server != nil {
		t.Error("an API was started for a door with no host configured")
	}

	inv.close(mgr)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the dropfile outlived the door: %v", err)
	}
}
