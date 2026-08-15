// Package fountain implements L1: erasure coding for mesh broadcast
// (design §7.2).
//
// # Why a fountain code rather than ARQ
//
// A mesh broadcast reaches every listening BBS at once and each one misses a
// DIFFERENT random subset. Under ARQ, N receivers send N different NACK sets
// and the sender retransmits their union, so cost grows with peer count — at
// fifty peers that is exactly the wrong scaling. Under a fountain code the
// sender emits symbols and each receiver decodes as soon as it has collected
// any K+ε; one transmission serves everyone and there is no NACK traffic. On a
// link where a round trip costs minutes, removing the round trip is worth more
// than the coding overhead.
//
// # Why not an off-the-shelf RaptorQ
//
// Our blocks are tiny — a typical bundle is one to ten symbols, rarely more
// than thirty. Published LT/Raptor overhead figures of about 5% assume K in the
// hundreds or thousands; at K below twenty the degree distributions those codes
// rely on behave poorly and overhead can exceed 20%. RFC 6330 also carries
// Qualcomm IPR declarations, which is worth avoiding in an MIT-licensed binary.
//
// # What this actually is
//
// A SYSTEMATIC LT code with a small-K-tuned degree distribution:
//
//  1. Symbols 0..K-1 are the original fragments, sent in order. A receiver with
//     no loss decodes at ZERO coding overhead — the common case on a good link
//     costs nothing, which is the property pure fountain schemes give up.
//  2. Symbols K and beyond are XOR combinations. The combination for symbol i
//     is derived by seeding a PRNG with (sender, bundle_id, i), so the mask is
//     NEVER transmitted — both ends compute it. The sender is in that tuple
//     because bundle IDs are random per node, and two nodes colliding on one
//     would otherwise silently corrupt each other's decodes.
//  3. Decoding is belief propagation with a Gaussian-elimination fallback over
//     GF(2). At K ≤ 64 that is a 64×64 bit matrix — microseconds.
//  4. K = 1 needs no coding at all; most DMs and small post batches land there.
package fountain

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/identity"
	"lukechampine.com/blake3"
)

// HeaderSize is the L1 symbol header, leaving 225 bytes of payload on a
// 233-byte Meshtastic packet (§7.2).
const HeaderSize = 8

// MaxK bounds a source block. Gaussian elimination at K=64 is a 64x64 bit
// matrix; beyond that the bundle should be split instead.
const MaxK = 64

// MaxSymbolSize is the largest payload a symbol may carry.
const MaxSymbolSize = 1400 // an IP link's MTU; mesh symbols are far smaller

var (
	ErrTruncated  = errors.New("truncated symbol")
	ErrBadK       = errors.New("invalid source block size")
	ErrMismatched = errors.New("symbol does not belong to this block")
	ErrNotDecoded = errors.New("not enough symbols to decode")
	ErrSymbolSize = errors.New("symbol size out of range")
)

// Symbol is one encoded fragment, ready to hand to a Link.
type Symbol struct {
	// BundleID identifies the source block. Random per bundle, per sender.
	BundleID uint32
	// Index is 0..K-1 for a systematic original, or >= K for a repair symbol.
	Index uint16
	// K is the number of source symbols in the block.
	K uint8
	// Data is the symbol payload.
	Data []byte
}

// Encode serialises a symbol for transmission.
//
// Layout matches §7.2:
//
//	byte 0     : version(2b) | flags(3b) | reserved(3b)
//	bytes 1-4  : bundle_id
//	byte 5     : index low 8 bits
//	byte 6     : K
//	byte 7     : index high 8 bits
//
// The index is split across bytes 5 and 7 because §7.2 reserved byte 7 for
// "extended index high bits" and repair indices routinely exceed 255 on a lossy
// link.
func (s Symbol) Encode() []byte {
	out := make([]byte, HeaderSize, HeaderSize+len(s.Data))
	out[0] = 1 << 6 // version 1 in the top two bits
	binary.BigEndian.PutUint32(out[1:5], s.BundleID)
	out[5] = byte(s.Index & 0xff)
	out[6] = s.K
	out[7] = byte(s.Index >> 8)
	return append(out, s.Data...)
}

// DecodeSymbol parses a wire symbol.
func DecodeSymbol(b []byte) (Symbol, error) {
	if len(b) < HeaderSize {
		return Symbol{}, ErrTruncated
	}
	if version := b[0] >> 6; version != 1 {
		return Symbol{}, fmt.Errorf("unsupported symbol version %d", version)
	}
	// The low six bits are reserved. Requiring them to be zero keeps the
	// encoding canonical — otherwise 0x41 and 0x40 are two wire forms of the
	// same symbol — and leaves the bits free to be given meaning later without
	// having to guess whether an old sender set them by accident. Found by the
	// fuzzer, same class as the overlong-varint bug in the record codec.
	if reserved := b[0] & 0x3f; reserved != 0 {
		return Symbol{}, fmt.Errorf("reserved symbol header bits are set (0x%02x)", reserved)
	}
	payload := b[HeaderSize:]
	if len(payload) == 0 || len(payload) > MaxSymbolSize {
		return Symbol{}, ErrSymbolSize
	}
	k := b[6]
	if k == 0 || k > MaxK {
		return Symbol{}, fmt.Errorf("%w: K=%d", ErrBadK, k)
	}
	return Symbol{
		BundleID: binary.BigEndian.Uint32(b[1:5]),
		Index:    uint16(b[5]) | uint16(b[7])<<8,
		K:        k,
		Data:     append([]byte(nil), payload...),
	}, nil
}

// ---------------------------------------------------------------------------
// Mask derivation
// ---------------------------------------------------------------------------

// Mask exposes the repair-symbol derivation to the conformance corpus (§12.6).
//
// It is exported for that one reason, and the reason is strong. The mask never
// travels on the wire: both ends compute it from (sender, bundle_id, index), so
// an implementation that computes it differently does not fail a length check or
// a signature — it decodes to garbage, silently, with every field agreeing.
// There is nowhere else in BSMP where being wrong is this quiet, which makes it
// the one part of the format a third-party implementer most needs a frozen
// witness for.
//
// The result is indexed by source symbol: element i is true when symbol i is
// XORed into the repair symbol.
func Mask(sender identity.NodeID, bundleID uint32, index uint16, k int) []bool {
	return mask(sender, bundleID, index, k)
}

// mask returns which source symbols XOR into repair symbol `index`.
//
// Derived from (sender, bundleID, index) so it never travels on the wire. Both
// ends compute the same mask from data they already have, which is the whole
// reason repair symbols cost only their payload.
//
// # Why the mask is DENSE, not sparse
//
// §7.2 suggests a distribution "heavy on degree 2-3". That guidance follows
// from belief propagation, which peels the block starting from degree-1
// symbols and therefore wants sparse equations. This decoder is Gaussian
// elimination over GF(2) instead — correct at K <= 64, where the matrix is
// tiny and elimination succeeds whenever the equations are INDEPENDENT rather
// than only when a peeling chain happens to be available.
//
// Independence is what changes the answer. Sparse rows over GF(2) are far more
// likely to be linearly dependent, so a sparse distribution needs many extra
// symbols before reaching full rank. Measured overhead, in extra symbols
// beyond K:
//
//	distribution        K=4    K=10   K=24   K=40
//	degree 2-3 heavy    1.43   2.94   7.66   14.41
//	each symbol p=1/2   1.17   1.68   1.61   1.72
//
// Including each source symbol with probability one half is the standard
// result for random binary matrices: about 1.6 extra symbols, FLAT in K. The
// sparse distribution degrades badly as K grows, which is the opposite of what
// a mesh needs.
//
// A fixed even degree is catastrophic and worth naming: every row then has
// even parity, so the rows always sum to zero and the matrix never reaches
// full rank at all.
func mask(sender identity.NodeID, bundleID uint32, index uint16, k int) []bool {
	h := blake3.New(32, nil)
	h.Write(sender[:])
	var buf [6]byte
	binary.BigEndian.PutUint32(buf[0:4], bundleID)
	binary.BigEndian.PutUint16(buf[4:6], index)
	h.Write(buf[:])
	seed := h.Sum(nil)

	// A tiny counter-mode PRNG over the seed: deterministic, and identical on
	// every platform, which §12.1 requires.
	next := func() func() uint32 {
		var counter uint32
		var pool []byte
		return func() uint32 {
			if len(pool) < 4 {
				var cb [4]byte
				binary.BigEndian.PutUint32(cb[:], counter)
				counter++
				hh := blake3.New(32, nil)
				hh.Write(seed)
				hh.Write(cb[:])
				pool = hh.Sum(nil)
			}
			v := binary.BigEndian.Uint32(pool[:4])
			pool = pool[4:]
			return v
		}
	}()

	out := make([]bool, k)
	if k == 1 {
		out[0] = true
		return out
	}

	// Include each source symbol with probability one half, consuming bits in
	// 32-symbol batches so the draw is cheap and reproducible.
	set := 0
	for i := 0; i < k; i += 32 {
		bits := next()
		for j := 0; j < 32 && i+j < k; j++ {
			if bits&(1<<uint(j)) != 0 {
				out[i+j] = true
				set++
			}
		}
	}
	// An all-zero mask carries no information; give it one member rather than
	// wasting the symbol. This happens with probability 2^-k, so it is rare
	// above the smallest blocks but must still be handled.
	if set == 0 {
		out[int(next()%uint32(k))] = true
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
