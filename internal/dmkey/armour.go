// Package dmkey is the client half of tier-3 DM key custody (design §8.2).
//
// # What tier 3 changes
//
// Tiers 1 and 2 keep the user's private key on the server: tier 1 in the clear,
// tier 2 wrapped under a passphrase the server sees only during a session. Tier
// 3 keeps it on the user's own machine, so the sysop holds ciphertext they
// cannot open even in principle.
//
// The BBS is unchanged in every respect except one. It still discovers keys,
// addresses mail, verifies signatures and delivers — all of which work from
// public keys alone, which is the boundary §8.2 told Phase 1 to protect and
// which it did. The single thing it can no longer do is decrypt for display, so
// it hands the user the sealed bytes instead and this package opens them.
//
// # Why a separate binary and not a clever protocol
//
// `[N3]` weighed an ssh-agent-derived construction — deriving X25519 from a
// deterministic Ed25519 signature over a domain-separation string — and
// rejected it. Nicer UX, but it depends on agent forwarding being available and
// on a homebrew derivation that would need real cryptographic review before it
// protected anyone's mail. This is meant to be boring, obviously correct and
// reviewable in an afternoon, which for the one component whose failure mode is
// silent is the right trade.
//
// Accordingly there is no new cryptography here. Sealing and opening are
// internal/keyring's, unchanged and already reviewed; this package adds a file
// to keep a key in and a text format to move ciphertext through a terminal.
package dmkey

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Armour delimiters.
//
// The header is not decoration. A user pasting into a terminal has to be able
// to tell what they are pasting, and a bare base64 blob is indistinguishable
// from every other bare base64 blob — including, dangerously, a wrapped PRIVATE
// key, which is the one thing that must never be handed to anybody. Naming the
// contents is what makes "paste your message here" a safe instruction.
const (
	armourBegin = "-----BEGIN MESHBBS DM-----"
	armourEnd   = "-----END MESHBBS DM-----"
)

// armourWidth is where base64 wraps.
//
// 64 rather than 76 or unwrapped: §5 says the BBS renders to an 80-column
// CP437 terminal, and a line that reaches the edge is a line some terminal
// somewhere will hard-wrap, silently, in a way the user cannot see. Wrapping it
// ourselves means the line breaks are ours and the decoder knows to ignore
// them.
const armourWidth = 64

// MaxArmourBytes bounds what Unarmour will decode.
//
// A DM body is bounded by the record it travels in, so anything approaching
// this is not a message that came from this network. The bound exists because
// this decoder's input is a paste buffer — the least trustworthy input in the
// system, since the user is instructed to paste something they did not write.
const MaxArmourBytes = 64 << 10

// ErrNoArmour is returned when the input contains no DM block.
var ErrNoArmour = errors.New("no MESHBBS DM block found")

// ErrManyArmour is returned when the input contains more than one.
//
// Refused rather than taking the first, because a user who pastes two messages
// and gets one plaintext has no way to tell which one they are reading. A
// decoder that guesses here is a decoder that shows people the wrong mail.
var ErrManyArmour = errors.New("more than one MESHBBS DM block found; paste one at a time")

// Armour renders sealed bytes as a block a user can copy out of a terminal.
func Armour(sealed []byte) string {
	var sb strings.Builder
	sb.WriteString(armourBegin)
	sb.WriteByte('\n')
	enc := base64.StdEncoding.EncodeToString(sealed)
	for len(enc) > armourWidth {
		sb.WriteString(enc[:armourWidth])
		sb.WriteByte('\n')
		enc = enc[armourWidth:]
	}
	sb.WriteString(enc)
	sb.WriteByte('\n')
	sb.WriteString(armourEnd)
	sb.WriteByte('\n')
	return sb.String()
}

// Unarmour extracts the sealed bytes from a pasted block.
//
// Deliberately tolerant about what surrounds the block and strict about the
// block itself. A user copying from a terminal takes the prompt, the status
// line and whatever else was on screen along with the message; refusing that
// paste would teach them to trim by hand, which is how the block itself gets
// damaged. So everything outside the delimiters is ignored.
//
// Inside them nothing is guessed: CR is stripped because a Windows terminal or
// PuTTY will introduce it, and whitespace is stripped because a paste may be
// re-wrapped, but a character that is not base64 is an error rather than
// something to skip past. The difference matters — ignoring stray characters
// inside the payload would let a corrupted paste decode to plausible-looking
// bytes that then fail authentication with a message about the wrong thing.
func Unarmour(s string) ([]byte, error) {
	if len(s) > MaxArmourBytes {
		return nil, fmt.Errorf("input is %d bytes, limit is %d", len(s), MaxArmourBytes)
	}

	var blocks [][]string
	var cur []string
	inBlock := false
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
		switch {
		case trimmed == armourBegin:
			if inBlock {
				// A second BEGIN before an END. The paste is damaged or is two
				// messages run together; either way the boundaries are not
				// trustworthy and guessing which lines belong to which message
				// is exactly the wrong kind of helpful.
				return nil, ErrManyArmour
			}
			inBlock, cur = true, nil
		case trimmed == armourEnd:
			if !inBlock {
				return nil, fmt.Errorf("%w: an END with no BEGIN before it", ErrNoArmour)
			}
			blocks = append(blocks, cur)
			inBlock, cur = false, nil
		case inBlock:
			if trimmed != "" {
				cur = append(cur, trimmed)
			}
		}
	}
	if inBlock {
		return nil, fmt.Errorf("%w: a BEGIN with no END after it", ErrNoArmour)
	}
	switch len(blocks) {
	case 0:
		return nil, ErrNoArmour
	case 1:
	default:
		return nil, ErrManyArmour
	}

	body := strings.Join(blocks[0], "")
	if body == "" {
		return nil, errors.New("the MESHBBS DM block is empty")
	}
	sealed, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("the MESHBBS DM block is not valid base64 (a partial copy will do this): %w", err)
	}
	return sealed, nil
}
