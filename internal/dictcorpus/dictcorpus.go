// Package dictcorpus holds the sample traffic dictionary 1 is trained on and
// measured against (design §7.4).
//
// # Why it is its own package
//
// Two things need the same samples turned into the same records: the trainer in
// tools/traindict, and the compression gate in internal/bundle. Building records
// is fiddly enough — keys, sequence numbers, per-type body encoders — that two
// copies would drift, and a gate measuring something slightly different from what
// was trained on is worse than no gate.
//
// It imports record and identity but NOT bundle, so internal/bundle's own tests
// can use it without an import cycle. Callers assemble the bundles.
//
// # It is never linked into the server
//
// The corpus is embedded, which would be about 30 KB of forum prose inside every
// shipped binary if anything in cmd/ reached for it. Nothing does: the importers
// are a build-time tool and a test. Keeping it that way is the reason this is not
// simply a file in internal/bundle.
//
// # Train and holdout do not overlap
//
// See data/README.md. Every ratio this project quotes comes from Holdout, which
// the trainer never sees, because a dictionary measured on its training data
// reports how well it memorised.
package dictcorpus

import (
	"bufio"
	"crypto/ed25519"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"lukechampine.com/blake3"
)

//go:embed data
var data embed.FS

// postSeparator divides posts in forum.txt.
const postSeparator = "%%"

// Set is one half of the corpus, already turned into signed records.
//
// Grouped by kind because that is how bundles are built — a bundle carries one
// area — and because the three kinds compress very differently, which is the
// whole point of measuring them apart.
type Set struct {
	Posts  []*record.Record
	Files  []*record.Record
	Doors  []*record.Record
	Origin identity.NodeKey
}

// All returns every record in the set, in a stable order.
func (s *Set) All() []*record.Record {
	out := make([]*record.Record, 0, len(s.Posts)+len(s.Files)+len(s.Doors))
	out = append(out, s.Posts...)
	out = append(out, s.Files...)
	out = append(out, s.Doors...)
	return out
}

// Train is the half the dictionary is built from.
func Train() (*Set, error) { return load("train") }

// Holdout is the half nothing is trained on, and the only half worth quoting a
// ratio from.
func Holdout() (*Set, error) { return load("holdout") }

// corpusKey derives the signing identity for a half of the corpus.
//
// Deterministic, and different per half so that a record from one can never be
// mistaken for a record from the other in a debugging session.
func corpusKey(half string) identity.NodeKey {
	sum := blake3.Sum256([]byte("meshbbs/dictcorpus/" + half))
	priv := ed25519.NewKeyFromSeed(sum[:])
	return identity.NodeKey{Public: priv.Public().(ed25519.PublicKey), Private: priv}
}

func load(half string) (*Set, error) {
	key := corpusKey(half)
	set := &Set{Origin: key}

	// Sequence numbers are allocated across the whole half rather than per kind,
	// because that is what a real log does and because a varint that changes
	// width partway through the corpus is a size effect worth having present.
	var seq uint64
	next := func() uint64 { seq++; return seq }

	const baseTS = 1700000000

	posts, err := readPosts(half)
	if err != nil {
		return nil, err
	}
	for i, body := range posts {
		r, err := record.New(key, record.Record{
			Seq: next(), TS: uint32(baseTS + i*3600), Type: record.TypePost,
			Area: record.AreaTagFor("general"), Body: []byte(body),
		})
		if err != nil {
			return nil, fmt.Errorf("%s post %d: %w", half, i, err)
		}
		set.Posts = append(set.Posts, r)
	}

	files, err := readFiles(half)
	if err != nil {
		return nil, err
	}
	for i, f := range files {
		body, err := record.MarshalFileBody(f)
		if err != nil {
			return nil, fmt.Errorf("%s file %d (%s): %w", half, i, f.Name, err)
		}
		r, err := record.New(key, record.Record{
			Seq: next(), TS: uint32(baseTS + i*3600), Type: record.TypeFile,
			Area: record.AreaTagFor("files/uploads"), Body: body,
		})
		if err != nil {
			return nil, fmt.Errorf("%s file %d: %w", half, i, err)
		}
		set.Files = append(set.Files, r)
	}

	doors, err := readDoors(half)
	if err != nil {
		return nil, err
	}
	for i, body := range doors {
		r, err := record.New(key, record.Record{
			Seq: next(), TS: uint32(baseTS + i*3600), Type: record.TypeDoorEvent,
			Area: record.AreaTagFor("league/" + body.Game), Body: mustDoorBody(body),
		})
		if err != nil {
			return nil, fmt.Errorf("%s door %d: %w", half, i, err)
		}
		set.Doors = append(set.Doors, r)
	}

	return set, nil
}

func mustDoorBody(d record.DoorEventBody) []byte {
	b, err := record.MarshalDoorEventBody(d)
	if err != nil {
		panic(err) // validated by readDoors before it gets here
	}
	return b
}

// readText reads a corpus file and normalises its line endings.
//
// .gitattributes marks this directory -text so a checkout is LF everywhere, and
// that is the real fix: these files are inputs to a committed artifact, and
// dict1.zdict was trained on their LF bytes. This normalisation is the second
// line of defence, for the copy that arrives through some path git attributes do
// not cover — an editor that rewrites endings on save, a zip, a patch pasted
// into a terminal.
//
// It is worth having twice because the failure is quiet in the direction that
// matters. Before the attribute existed, a CRLF checkout did not fail to parse:
// it parsed the entire forum file into ONE post, because the separator no longer
// matched, and the first thing to notice was a slice bounds panic several
// packages away. A corpus that silently changes shape retrains a different
// dictionary and measures a different ratio.
func readText(half, name string) (string, error) {
	raw, err := fs.ReadFile(data, "data/"+half+"/"+name)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n"), nil
}

func readPosts(half string) ([]string, error) {
	raw, err := readText(half, "forum.txt")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, chunk := range strings.Split(raw, "\n"+postSeparator+"\n") {
		if s := strings.TrimSpace(chunk); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s/forum.txt has no posts", half)
	}
	return out, nil
}

func readFiles(half string) ([]record.FileBody, error) {
	raw, err := readText(half, "files.txt")
	if err != nil {
		return nil, err
	}
	var out []record.FileBody
	sc := bufio.NewScanner(strings.NewReader(raw))
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		parts := strings.Split(text, "\t")
		if len(parts) != 3 {
			return nil, fmt.Errorf("%s/files.txt:%d: want name<TAB>size<TAB>description", half, line)
		}
		size, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s/files.txt:%d: %w", half, line, err)
		}
		// A content hash is BLAKE3 of bytes we do not have, so it is derived from
		// the name. It must be high-entropy either way: 16 incompressible bytes
		// per catalog entry is a real part of what a FILE bundle costs, and a
		// corpus that used a low-entropy placeholder would quietly overstate the
		// ratio on exactly the record type that compresses worst.
		sum := blake3.Sum256([]byte("meshbbs/dictcorpus/content/" + parts[0]))
		var h [record.FileHashLen]byte
		copy(h[:], sum[:])
		out = append(out, record.FileBody{
			Name: parts[0], Size: size, Hash: h, Description: parts[2],
		})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s/files.txt has no entries", half)
	}
	return out, nil
}

// readDoors builds event batches from the game and nick lists.
//
// Batches are full ones (MaxDoorEventsPerRecord) because that is what the
// flusher emits when a league is busy enough to matter, and a batch of one would
// measure the framing rather than the traffic.
func readDoors(half string) ([]record.DoorEventBody, error) {
	raw, err := readText(half, "doors.txt")
	if err != nil {
		return nil, err
	}
	peer := corpusKey(half + "/peer").ID()

	var out []record.DoorEventBody
	sc := bufio.NewScanner(strings.NewReader(raw))
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		parts := strings.Split(text, "\t")
		if len(parts) != 2 {
			return nil, fmt.Errorf("%s/doors.txt:%d: want game<TAB>nick,nick,...", half, line)
		}
		nicks := strings.Split(parts[1], ",")
		if len(nicks) < 2 {
			return nil, fmt.Errorf("%s/doors.txt:%d: need at least two nicks", half, line)
		}

		var events []record.DoorEvent
		for i := 0; i < record.MaxDoorEventsPerRecord; i++ {
			actor := strings.TrimSpace(nicks[i%len(nicks)])
			target := strings.TrimSpace(nicks[(i+1)%len(nicks)])
			ev := record.DoorEvent{
				Kind:       uint8(i % 5),
				Actor:      actor,
				Target:     target,
				TargetNode: peer,
				// Four bytes of door-defined payload. Opaque by definition, so it
				// is derived rather than invented: a made-up payload with
				// structure would teach the dictionary a pattern no real door
				// emits.
				Payload: derivedPayload(parts[0], i),
			}
			events = append(events, ev)
		}
		body := record.DoorEventBody{Game: parts[0], Events: events}
		if _, err := record.MarshalDoorEventBody(body); err != nil {
			return nil, fmt.Errorf("%s/doors.txt:%d: %w", half, line, err)
		}
		out = append(out, body)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s/doors.txt has no games", half)
	}
	return out, nil
}

func derivedPayload(game string, i int) []byte {
	sum := blake3.Sum256([]byte(fmt.Sprintf("meshbbs/dictcorpus/payload/%s/%d", game, i)))
	return sum[:4]
}
