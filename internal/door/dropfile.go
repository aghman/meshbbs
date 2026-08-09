package door

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Dropfiles (§9.2).
//
// A dropfile is how a BBS told a door who was calling, before anybody thought
// of an API: a plain text file of positional lines, written before the door
// starts and read by it on the way up. The formats are from the DOS era and
// have not changed since, which is the point — a door written in 1993 reads
// DOOR.SYS and nothing else.
//
// Cheap and mostly independent of the emulator, so §9.2 puts them here rather
// than with the DOS work in Phase 7. A modern door that wants this information
// should use the API (§9.1.1); this exists so that a door which cannot is not
// shut out.
//
// # The hazard these formats carry
//
// Every one of them is POSITIONAL and LINE-DELIMITED: field seven is whatever
// is on line seven. So a value containing a newline does not corrupt one field,
// it shifts every field after it — and the fields after the user's name include
// the security level. A caller whose real name is "Bob\r\n255" would hand
// themselves sysop access to any door that trusts the number it reads.
//
// Nicks cannot do this (§6.7 allows only letters, digits, underscore and
// hyphen), but real names and locations are free text, and a format whose
// safety depends on which field you put where is one nobody should have to
// reason about at the call site. So EVERY value goes through sanitizeField, and
// the fuzz target asserts the line count is fixed no matter what goes in.
//
// # What is deliberately not written
//
// DOOR.SYS line 14 is the caller's PASSWORD, in the clear. meshbbs does not
// have it — passwords are Argon2id hashes (§6.7) — and if it did, writing it
// into a file for a third-party binary to read would be indefensible. The line
// is present and empty, because the format is positional and removing it would
// shift everything below. A door that needs to authenticate its own users has
// the API's session context, which says who they are without proving it with a
// secret.

// Dropfile formats, matching the names in the doors table.
const (
	DropfileNone     = "none"
	DropfileDoorSys  = "door.sys"
	DropfileDoor32   = "door32.sys"
	DropfileDorinfo1 = "dorinfo1.def"
)

// Security levels, because the formats demand a number and meshbbs has
// capabilities (§6.7).
//
// A coarse projection, offered only because the field exists. It is NOT the
// gate: what a caller may run is decided before the door starts, by run_doors
// and the door's required_capability, and a door making its own access
// decisions from this number is trusting a value it was handed rather than one
// it checked.
const (
	levelGuest  = 10
	levelUser   = 50
	levelSysop  = 100
	unknownDate = "01/01/00"
)

// dropfileName is the filename each format is conventionally found under. Doors
// look for these by name, so they are not ours to choose.
func dropfileName(format string) string {
	switch format {
	case DropfileDoorSys:
		return "DOOR.SYS"
	case DropfileDoor32:
		return "DOOR32.SYS"
	case DropfileDorinfo1:
		return "DORINFO1.DEF"
	}
	return ""
}

// writeDropfile writes the dropfile for one invocation into dir and returns its
// path, or "" when the door asked for none.
//
// Written into the invocation's own private directory rather than the door's
// working directory, which is where a DOS-era BBS would have put it. That is
// the same idea with a stronger guarantee: classic boards gave each NODE a
// directory so two callers could not overwrite each other's dropfile, and a
// per-invocation directory cannot collide even with itself. It also means the
// file — which names a caller and says where they live — is removed when the
// door exits, rather than sitting in the door's directory until the next player
// replaces it.
func writeDropfile(dir string, spec Spec, sess Session) (string, error) {
	name := dropfileName(spec.Dropfile)
	if name == "" {
		return "", nil
	}

	body, err := dropfileBody(spec, sess)
	if err != nil {
		return "", err
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", name, err)
	}
	return path, nil
}

// dropfileBody renders a dropfile without writing it, which is what lets the
// fuzz target exercise the formats without three temporary directories per
// case.
func dropfileBody(spec Spec, sess Session) (string, error) {
	switch spec.Dropfile {
	case DropfileDoorSys:
		return doorSys(spec, sess), nil
	case DropfileDoor32:
		return door32Sys(spec, sess), nil
	case DropfileDorinfo1:
		return dorinfo1(spec, sess), nil
	}
	return "", fmt.Errorf("unknown dropfile format %q", spec.Dropfile)
}

// lines joins fields the way these formats expect: CRLF, including after the
// last one. They come from DOS and doors parse them with DOS line-reading.
func lines(fields ...string) string {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f)
		b.WriteString("\r\n")
	}
	return b.String()
}

// sanitizeField makes a value safe to put on a line of a positional format.
//
// Newlines and carriage returns become spaces rather than being stripped,
// because stripping would silently join two words, and a caller reading their
// own name in a door should see something recognisable. Other control
// characters go too: these files are read by programs that predate the idea
// that input might be hostile, and an escape sequence in a name would be
// rendered by whatever prints it.
//
// Runs of whitespace collapse to one. CRLF is two characters and would
// otherwise leave two spaces, and these are fixed-width fields being read by
// software that counts columns.
func sanitizeField(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	space := false
	for _, r := range v {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || r == ' ':
			space = true
		case r < 0x20 || r == 0x7f:
			// Dropped, and not treated as a word boundary: a control character
			// sitting inside a word did not separate it.
		default:
			if space && b.Len() > 0 {
				b.WriteRune(' ')
			}
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

// securityLevel projects a session onto the number these formats want.
func securityLevel(sess Session) int {
	switch {
	case sess.Sysop:
		return levelSysop
	case sess.Nick == "":
		return levelGuest
	default:
		return levelUser
	}
}

// minutesLeft is what the dropfile reports as time remaining.
//
// Dropfiles have no way to say "no limit", so an unlimited session reports a
// large number rather than zero — a door reading zero would show the caller a
// goodbye they have not earned. The value is deliberately not enormous: some
// doors put it in a 16-bit field.
func minutesLeft(sess Session) int {
	const noLimit = 32000
	if sess.TimeRemaining == nil {
		return noLimit
	}
	mins := int(sess.TimeRemaining().Minutes())
	return min(max(mins, 0), noLimit)
}

// firstLast splits a name the way the DOS-era formats want it, since several
// of them have separate fields and callers have one name.
func firstLast(name string) (string, string) {
	name = sanitizeField(name)
	first, last, ok := strings.Cut(name, " ")
	if !ok {
		return name, ""
	}
	return first, strings.TrimSpace(last)
}

// graphicsFlag is the ANSI indicator: doors have used several spellings.
func graphicsFlag(ansi bool, yes, no string) string {
	if ansi {
		return yes
	}
	return no
}

// doorSys is the 52-line GAP format, the most widely supported one.
func doorSys(spec Spec, sess Session) string {
	level := strconv.Itoa(securityLevel(sess))
	mins := minutesLeft(sess)
	node := strconv.Itoa(sess.Node)
	name := sanitizeField(sess.RealName)
	if name == "" {
		name = sanitizeField(sess.Nick)
	}

	return lines(
		"COM0:",                             //  1 comm port; 0 means local, which every door here is
		"0",                                 //  2 baud rate
		"8",                                 //  3 parity
		node,                                //  4 node number
		"0",                                 //  5 DTE rate
		"Y",                                 //  6 screen display
		"N",                                 //  7 printer
		"Y",                                 //  8 page bell
		"Y",                                 //  9 caller alarm
		name,                                // 10 full name
		sanitizeField(sess.Location),        // 11 calling from
		"",                                  // 12 home phone
		"",                                  // 13 work phone
		"",                                  // 14 password — see the note at the top
		level,                               // 15 security level
		"1",                                 // 16 total times on
		unknownDate,                         // 17 last call date
		strconv.Itoa(mins*60),               // 18 seconds left
		strconv.Itoa(mins),                  // 19 minutes left
		graphicsFlag(sess.ANSI, "GR", "NG"), // 20 graphics mode
		strconv.Itoa(max(sess.Height, 24)),  // 21 screen length
		"N",                                 // 22 expert mode
		"",                                  // 23 conferences
		"",                                  // 24 conference exited from
		unknownDate,                         // 25 expiry date
		"1",                                 // 26 user record number
		"Z",                                 // 27 default protocol
		"0",                                 // 28 uploads
		"0",                                 // 29 downloads
		"0",                                 // 30 daily download K
		"0",                                 // 31 daily download max K
		unknownDate,                         // 32 birthdate
		sanitizeField(spec.Dir),             // 33 main directory
		sanitizeField(spec.Dir),             // 34 gen directory
		sanitizeField(sess.SysopName),       // 35 sysop's name
		sanitizeField(sess.Nick),            // 36 handle
		"00:00",                             // 37 event time
		"Y",                                 // 38 error-correcting
		graphicsFlag(sess.ANSI, "Y", "N"),   // 39 ANSI available
		"N",                                 // 40 record locking
		"7",                                 // 41 default colour
		strconv.Itoa(mins),                  // 42 time credits
		unknownDate,                         // 43 last new-file scan
		"00:00",                             // 44 time of this call
		"00:00",                             // 45 time of last call
		"0",                                 // 46 max daily files
		"0",                                 // 47 files today
		"0",                                 // 48 total K uploaded
		"0",                                 // 49 total K downloaded
		"",                                  // 50 comment
		"0",                                 // 51 doors opened
		"0",                                 // 52 messages left
	)
}

// door32Sys is the modern one: eleven lines, and the only format that was
// designed after telnet existed.
func door32Sys(spec Spec, sess Session) string {
	emulation := "0" // ascii
	if sess.ANSI {
		emulation = "1"
	}
	name := sanitizeField(sess.RealName)
	if name == "" {
		name = sanitizeField(sess.Nick)
	}
	return lines(
		"0",                               //  1 comm type: 0 is local
		"0",                               //  2 socket handle; none, we bridge a pty
		"0",                               //  3 baud rate
		sanitizeField(sess.BBSName),       //  4 BBS name
		"1",                               //  5 user record position
		name,                              //  6 real name
		sanitizeField(sess.Nick),          //  7 handle
		strconv.Itoa(securityLevel(sess)), //  8 security level
		strconv.Itoa(minutesLeft(sess)),   //  9 minutes left
		emulation,                         // 10 emulation
		strconv.Itoa(sess.Node),           // 11 node number
	)
}

// dorinfo1 is the RBBS/QuickBBS format.
func dorinfo1(spec Spec, sess Session) string {
	sysopFirst, sysopLast := firstLast(sess.SysopName)
	userFirst, userLast := firstLast(sess.RealName)
	if userFirst == "" {
		userFirst = sanitizeField(sess.Nick)
	}
	return lines(
		sanitizeField(sess.BBSName),       //  1 BBS name
		sysopFirst,                        //  2 sysop first name
		sysopLast,                         //  3 sysop last name
		"COM0",                            //  4 comm port
		"0 BAUD,N,8,1",                    //  5 line settings
		"0",                               //  6 networked
		userFirst,                         //  7 user first name
		userLast,                          //  8 user last name
		sanitizeField(sess.Location),      //  9 user location
		graphicsFlag(sess.ANSI, "1", "0"), // 10 graphics
		strconv.Itoa(securityLevel(sess)), // 11 security level
		strconv.Itoa(minutesLeft(sess)),   // 12 minutes left
		"-1",                              // 13 FOSSIL flag
	)
}

// dropfileLineCount is how many lines each format has, so that the invariant
// can be asserted rather than believed.
func dropfileLineCount(format string) int {
	switch format {
	case DropfileDoorSys:
		return 52
	case DropfileDoor32:
		return 11
	case DropfileDorinfo1:
		return 13
	}
	return 0
}

// expandDropfileTokens substitutes the dropfile's path into a door's arguments.
//
// Doors from this era are usually told where their dropfile is on the command
// line, and every BBS spelled that differently. Two tokens cover both shapes a
// door asks for — the file, or the directory holding it — and a door that wants
// neither simply has neither in its arguments.
func expandDropfileTokens(args []string, path string) []string {
	if path == "" {
		return args
	}
	dir := filepath.Dir(path)
	out := make([]string, 0, len(args))
	for _, a := range args {
		a = strings.ReplaceAll(a, "{dropfile}", path)
		a = strings.ReplaceAll(a, "{dropfile_dir}", dir)
		out = append(out, a)
	}
	return out
}

// dropfileEnv tells a door where its dropfile is without needing an argument.
func dropfileEnv(path string) []string {
	if path == "" {
		return nil
	}
	return []string{
		"MESHBBS_DROPFILE=" + path,
		"MESHBBS_DROPFILE_DIR=" + filepath.Dir(path),
	}
}
