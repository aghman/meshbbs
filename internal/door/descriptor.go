package door

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// The session descriptor (§9.1, §9.1.1).
//
// # Why a file and not the environment
//
// §9.1.1 is explicit: the token goes in the descriptor, "never in argv or the
// environment of a shared process, both of which are readable by any other
// process on the box via ps or /proc". So the environment carries the PATH to
// the descriptor and the parts of the context that are not secret, and the file
// — mode 0600, in a 0700 directory, deleted when the invocation ends — carries
// the token.
//
// The non-secret context is duplicated into the environment on purpose. §9.1
// asks for both, and the reason is that a door written in ten lines of shell
// should be able to read $MESHBBS_USER without acquiring a JSON parser. A door
// that wants the token has to parse the file, and a door that wants the token
// is already doing something that warrants the ceremony.

// descriptorVersion is the descriptor's format version.
//
// Present from the first release because doors are third-party binaries built
// against whatever we shipped: adding a version field later means the first
// generation of doors cannot tell which format they are looking at, and the
// only remaining move is to never change the format.
const descriptorVersion = 1

// Descriptor is what a door is handed about the session it is serving.
type Descriptor struct {
	Version int `json:"version"`
	// Door is this door's name as the BBS knows it, which is also the name its
	// state is filed under and the name that appears in an audit row.
	Door string `json:"door"`
	// Socket is the address of the API socket: a path on Unix, a named pipe on
	// Windows.
	Socket string `json:"socket"`
	// Token authenticates to that socket. It is valid for this invocation only.
	Token string `json:"token"`
	// Level is the §9.1.1 level the sysop granted this door.
	Level int `json:"level"`
	// Session is the level-1 context, as at launch. A door that cares about
	// time remaining should ask the socket rather than trusting this copy.
	Session SessionInfo `json:"session"`
}

// writeDescriptor writes the descriptor into dir and returns its path.
func writeDescriptor(dir string, d Descriptor) (string, error) {
	d.Version = descriptorVersion
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode session descriptor: %w", err)
	}
	path := filepath.Join(dir, "session.json")
	// 0600 inside a 0700 directory. Belt and braces on purpose: the mode is
	// what a reviewer looks for, and the directory is what actually holds on a
	// system where the door runs as a different account.
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("write session descriptor: %w", err)
	}
	return path, nil
}

// descriptorEnv is the non-secret half of the context, for a door that would
// rather read a variable than parse JSON.
//
// The token is NOT here, and must never be: see the note at the top of this
// file. Everything below is visible to anything that can run ps.
func descriptorEnv(path string, d Descriptor) []string {
	env := []string{
		"MESHBBS_DOOR_DESCRIPTOR=" + path,
		"MESHBBS_DOOR=" + d.Door,
		"MESHBBS_DOOR_SOCKET=" + d.Socket,
		"MESHBBS_DOOR_API_LEVEL=" + strconv.Itoa(d.Level),
		"MESHBBS_USER=" + d.Session.Handle,
		"MESHBBS_REAL_NAME=" + d.Session.RealName,
		"MESHBBS_NODE=" + strconv.Itoa(d.Session.Node),
		"MESHBBS_COLUMNS=" + strconv.Itoa(d.Session.Width),
		"MESHBBS_LINES=" + strconv.Itoa(d.Session.Height),
		// COLUMNS and LINES without the prefix as well, because that is what
		// programs that were not written for this BBS already read.
		"COLUMNS=" + strconv.Itoa(d.Session.Width),
		"LINES=" + strconv.Itoa(d.Session.Height),
	}
	if d.Session.TimeLimited {
		env = append(env, "MESHBBS_TIME_LEFT="+strconv.Itoa(d.Session.TimeRemainingSecs))
	}
	return env
}
