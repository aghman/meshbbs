package identity

import (
	"fmt"
	"strings"

	"lukechampine.com/blake3"
)

// WordCount is the number of words in the spoken rendering of a node ID.
//
// The arithmetic, since the design document originally got this wrong: a node
// ID is 64 bits, and each word from a 2048-word list carries 11 bits. Six words
// carry 66 bits — 64 of payload plus 2 spare, which we spend on a checksum.
// Four words (44 bits) cannot represent a node ID at all; five (55 bits) cannot
// either. Six is the minimum, not a preference.
const WordCount = 6

const bitsPerWord = 11

// checksumBits is the slack in 6 × 11 bits after the 64-bit payload. Spending
// it on a checksum means a misheard or mistyped word is rejected rather than
// silently decoding to a different, valid-looking node ID — which matters
// precisely because the word form exists for reading aloud over a noisy radio.
const checksumBits = WordCount*bitsPerWord - NodeIDLen*8 // = 2

var wordIndex = func() map[string]int {
	m := make(map[string]int, len(wordList))
	for i, w := range wordList {
		m[w] = i
	}
	return m
}()

// checksum returns the top checksumBits bits of BLAKE3(id).
func checksum(id NodeID) uint16 {
	sum := blake3.Sum256(id[:])
	return uint16(sum[0] >> (8 - checksumBits))
}

// EncodeWords renders a node ID as six BIP-39 words. It encodes exactly the
// same 64 bits as the base32 form — this is a second rendering of one
// identifier, not a second identifier.
func EncodeWords(id NodeID) []string {
	// Accumulate payload bits followed by checksum bits, then peel off 11 at a
	// time, most significant first.
	var acc uint32
	var bits uint
	out := make([]string, 0, WordCount)

	emit := func() {
		for bits >= bitsPerWord {
			bits -= bitsPerWord
			out = append(out, wordList[(acc>>bits)&0x7ff])
		}
	}
	for _, b := range id {
		acc = acc<<8 | uint32(b)
		bits += 8
		emit()
	}
	acc = acc<<checksumBits | uint32(checksum(id))
	bits += checksumBits
	emit()
	return out
}

// DecodeWords parses a six-word rendering back into a node ID, verifying the
// checksum. Words are matched case-insensitively.
func DecodeWords(words []string) (NodeID, error) {
	if len(words) != WordCount {
		return NodeID{}, fmt.Errorf("expected %d words, got %d", WordCount, len(words))
	}

	var acc uint32
	var bits uint
	var out []byte
	for i, w := range words {
		idx, ok := wordIndex[strings.ToLower(strings.TrimSpace(w))]
		if !ok {
			return NodeID{}, fmt.Errorf("word %d (%q) is not in the wordlist", i+1, w)
		}
		acc = acc<<bitsPerWord | uint32(idx)
		bits += bitsPerWord
		for bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>bits))
		}
	}
	if len(out) != NodeIDLen {
		return NodeID{}, fmt.Errorf("decoded %d bytes, expected %d", len(out), NodeIDLen)
	}

	var id NodeID
	copy(id[:], out)

	got := uint16(acc) & ((1 << checksumBits) - 1)
	if got != checksum(id) {
		return NodeID{}, fmt.Errorf("checksum mismatch: a word was probably misheard or mistyped")
	}
	return id, nil
}
