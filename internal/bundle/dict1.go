package bundle

import (
	_ "embed"
)

// dict1Raw is the trained dictionary 1, produced by tools/traindict from
// internal/dictcorpus and committed as an artifact (§7.4).
//
// Committed rather than built, for the same reason the Meshtastic protobuf
// bindings are: building it needs a corpus and a trainer, and a wire-format
// constant that is regenerated during `go build` is a wire-format constant that
// can change without anyone deciding it should.
//
//go:embed dict1.zdict
var dict1Raw []byte

// Dictionary1 is the trained dictionary: the one §7.4 has been asking for since
// the first bullet of its implementation list.
//
// # What it is, against what dictionary 0 is
//
// Dictionary 0 is a RAW dictionary — about 130 bytes of forum vocabulary with no
// entropy tables at all, written when a post was the only thing being compressed
// and predating both FILE and DOOR_EVENT. Measured, it buys roughly 1% over
// plain zstd, which is to say nothing.
//
// This one carries 16 KiB of selected content AND the literal, offset,
// match-length and literal-length tables trained on real bundle bodies. On a
// holdout corpus it has never seen it returns 1.298x where plain zstd returns
// 1.170x — an 11% saving on what actually goes on the air, against 1%.
//
// # Why the number is not §7.4's "3-5x", and never could have been
//
// That figure is a claim about 400 bytes of POST TEXT. What gets compressed is a
// bundle body, and a bundle body is 19-42% Ed25519 signature depending on shape
// — 64 incompressible bytes per record. Even if text and framing compressed to
// literally zero, a five-post bundle would only reach 4.3x and a six-entry FILE
// bundle 2.4x. The interesting cases are at the small end, where there is no
// internal redundancy for a compressor to find and a dictionary supplies the
// model it cannot build:
//
//	shape             body   plain zstd   dictionary 1
//	1 post             344          312            286
//	1 FILE entry       195          203            182
//
// A lone catalog entry is the sharpest illustration: plain zstd makes it BIGGER,
// and the dictionary is what turns that into a saving.
//
// A door league gains almost nothing (1.53x against 1.50x) and that is expected
// rather than disappointing — eight near-identical events in one record are
// already redundant enough for the compressor to exploit unaided.
func Dictionary1() (*Dictionary, error) {
	return NewDictionary(1, dict1Raw)
}

// DefaultDictionary is the dictionary a node compresses WITH.
//
// Reading is a different question from writing: a node holds every dictionary it
// knows (see DefaultDictionarySet) so that it can decode anything a peer sends,
// and picks exactly one to encode with. This is that one.
//
// # There is no negotiation, and that is a debt with a due date
//
// §7.4 says nodes announce which dictionaries they hold in their digest. They do
// not — the digest carries areas and nothing else, and that half was never built.
// So a peer running a build older than this one cannot read what this node sends,
// and there is no handshake in which it could say so.
//
// That is acceptable exactly once, and only now. §7.1's pre-freeze policy is
// that development releases make no compatibility promises, and no two real
// boards have ever exchanged anything (§13) — there is no installed base to
// strand. It stops being acceptable at the Phase 6 freeze, because after it a
// dictionary 2 could not ship at all without a way to find out who can read it.
// The announcement is therefore a freeze prerequisite and is recorded as one.
func DefaultDictionary() (*Dictionary, error) { return Dictionary1() }

// DefaultDictionarySet is every dictionary this build can READ.
//
// Dictionary 0 is in here forever. §7.4 promises old dictionaries stay supported
// and the conformance corpus (§12.6) pins a bundle packed against it, so it is
// kept and never edited — a retrain ships as a new ID rather than replacing the
// content under an existing one.
//
// The caller owns the returned set and must Close it.
func DefaultDictionarySet() (*DictionarySet, error) {
	d0, err := Dictionary0()
	if err != nil {
		return nil, err
	}
	d1, err := Dictionary1()
	if err != nil {
		d0.Close()
		return nil, err
	}
	set, err := NewDictionarySet(d0, d1)
	if err != nil {
		d0.Close()
		d1.Close()
		return nil, err
	}
	return set, nil
}
