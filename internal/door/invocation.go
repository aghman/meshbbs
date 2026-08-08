package door

import (
	"fmt"
	"log/slog"
	"os"
)

// invocation is one door launch's API surface: a private directory, a socket
// inside it, a descriptor beside it, and a token that means nothing anywhere
// else.
//
// §9.1.1 asked four questions about token lifetime and answered them all with
// "the invocation", so the invocation is a thing here rather than an idea. When
// this is closed the socket is gone, the descriptor is deleted, and a door that
// kept a copy of the token has a string that no longer opens anything.
type invocation struct {
	dir    string
	server *apiServer
}

// startAPI prepares the API for one launch, appending the descriptor's
// environment to the spec. It returns nil when no Host is configured.
func (m *Manager) startAPI(spec *Spec, sess Session) (*invocation, error) {
	if m.host == nil {
		return nil, nil
	}

	// A short prefix on purpose: this becomes part of a Unix socket path, and
	// sun_path is about a hundred bytes on every platform we ship (see
	// listen_unix.go). MkdirTemp creates it 0700.
	dir, err := os.MkdirTemp("", "mbdoor-")
	if err != nil {
		return nil, fmt.Errorf("create the door's private directory: %w", err)
	}

	inv := &invocation{dir: dir}
	fail := func(err error) (*invocation, error) {
		inv.close(m)
		return nil, err
	}

	token, err := newToken()
	if err != nil {
		return fail(err)
	}
	ln, addr, err := listenAPI(dir)
	if err != nil {
		return fail(err)
	}
	inv.server = m.serveAPI(ln, token, *spec, sess)

	desc := Descriptor{
		Door:    spec.Name,
		Socket:  addr,
		Token:   token,
		Level:   spec.Grant.Level,
		Session: inv.server.sessionInfo(),
	}
	path, err := writeDescriptor(dir, desc)
	if err != nil {
		return fail(err)
	}

	// Appended, so a door-specific setting of the same name would win — except
	// that these names are ours and a door row setting MESHBBS_USER is a
	// mistake worth letting the sysop make visible rather than one we silently
	// override.
	spec.Env = append(spec.Env, descriptorEnv(path, desc)...)
	return inv, nil
}

// close tears the invocation down: the socket stops answering and the
// descriptor, with the token in it, is removed from the disk.
func (inv *invocation) close(m *Manager) {
	if inv == nil {
		return
	}
	if inv.server != nil {
		_ = inv.server.Close()
	}
	if inv.dir != "" {
		if err := os.RemoveAll(inv.dir); err != nil {
			// Worth an error rather than a debug line: what is left behind is a
			// file with a token in it, and the token is only harmless because
			// the socket it opens has gone.
			m.log.Error("could not remove a door's private directory",
				"dir", inv.dir, "err", err, slog.String("consequence",
					"a session descriptor containing a spent token is still on disk"))
		}
	}
}
