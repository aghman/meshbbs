package fountain

import (
	"fmt"
	"math"

	"github.com/aghman/meshbbs/internal/identity"
)

// Encoder produces symbols for one source block.
type Encoder struct {
	sender   identity.NodeID
	bundleID uint32
	k        int
	symSize  int
	source   [][]byte
	// origLen is the pre-padding length, carried so the decoder can trim.
	origLen int
}

// NewEncoder splits a payload into K symbols of at most symSize bytes.
//
// The final symbol is zero-padded; the original length travels in the block
// header so the decoder trims exactly. Padding rather than a short final symbol
// keeps every XOR the same width, which is what makes the decoder simple.
func NewEncoder(sender identity.NodeID, bundleID uint32, payload []byte, symSize int) (*Encoder, error) {
	if symSize <= 0 || symSize > MaxSymbolSize {
		return nil, ErrSymbolSize
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: empty payload", ErrBadK)
	}

	k := (len(payload) + symSize - 1) / symSize
	if k > MaxK {
		return nil, fmt.Errorf("%w: payload needs %d symbols, limit %d — split the bundle",
			ErrBadK, k, MaxK)
	}

	source := make([][]byte, k)
	for i := 0; i < k; i++ {
		chunk := make([]byte, symSize)
		copy(chunk, payload[i*symSize:min((i+1)*symSize, len(payload))])
		source[i] = chunk
	}

	return &Encoder{
		sender: sender, bundleID: bundleID, k: k,
		symSize: symSize, source: source, origLen: len(payload),
	}, nil
}

// K is the number of source symbols.
func (e *Encoder) K() int { return e.k }

// OrigLen is the unpadded payload length.
func (e *Encoder) OrigLen() int { return e.origLen }

// Symbol produces symbol i.
//
// Indices below K are the systematic originals — a receiver with no loss uses
// exactly these and pays no coding overhead at all. Indices at or above K are
// repair symbols, XOR combinations whose membership both ends derive from
// (sender, bundleID, index).
func (e *Encoder) Symbol(index uint16) Symbol {
	if int(index) < e.k {
		return Symbol{
			BundleID: e.bundleID, Index: index, K: uint8(e.k),
			Data: append([]byte(nil), e.source[index]...),
		}
	}

	combined := make([]byte, e.symSize)
	for i, member := range mask(e.sender, e.bundleID, index, e.k) {
		if !member {
			continue
		}
		for b := range combined {
			combined[b] ^= e.source[i][b]
		}
	}
	return Symbol{BundleID: e.bundleID, Index: index, K: uint8(e.k), Data: combined}
}

// Transmission returns the systematic symbols followed by `repair` repair
// symbols.
//
// §7.2 makes the repair count a governor decision rather than a protocol
// constant: send K systematic symbols plus ceil(alpha*K)+1 repair, where alpha
// tracks observed loss. RepairCount computes that.
func (e *Encoder) Transmission(repair int) []Symbol {
	out := make([]Symbol, 0, e.k+repair)
	for i := 0; i < e.k; i++ {
		out = append(out, e.Symbol(uint16(i)))
	}
	for i := 0; i < repair; i++ {
		out = append(out, e.Symbol(uint16(e.k+i)))
	}
	return out
}

// Epsilon is the codec's own overhead: the symbols a decoder needs beyond K
// because some arrivals turn out to be linearly dependent on what it already
// holds. Two things about it were got wrong, in opposite directions, and both
// cost real airtime.
//
// # It is a distribution, not a mean
//
// Epsilon was first set to 1.6, the measured mean. That is the wrong statistic:
// the overhead is long-tailed (p90 = 4, p95 = 5, p99 = 7), so sizing to the
// mean leaves roughly half of all receivers short of it, on top of whatever the
// binomial term already gave away. Measured failure rates were 1.9% to 6.2%
// against a 2% target, in 28 of 30 cells.
//
// # It is flat in K only for K >= 5
//
// The plateau was measured at K in {5,10,15,20,40} and holds there. Below that
// it is badly false, and the error runs the expensive way: at K=1 the codec has
// NO overhead at all — every symbol is a copy of the single source symbol, so
// any one that arrives decodes the block — yet a flat 3.4 makes RepairCount
// send SIXTEEN symbols for a one-symbol payload at 50% loss, where six suffice.
//
// That is a 3x airtime bill on exactly the bundles §7.2 says dominate a BBS:
// "most DMs and most small post batches" are K=1. The simulator surfaced it as
// a fifty-node federation sitting at 5.4% of the channel, just over the §1.1
// budget, with nearly all of it spent on tiny bundles.
//
// # Where these numbers come from
//
// Fitted, not derived: for each K, sweep the loss rate, run thousands of
// transmissions per cell, and find the smallest repair count that holds decode
// failure under 2%. These values clear every measured cell — the K >= 5 plateau
// with 14 symbols of total waste across thirty cells, the small-K ramp with at
// most two per cell.
//
// TestRepairCountHoldsTheFailureRate is the regression test. If the degree
// distribution in mask() changes, epsilon changes with it and every value here
// must be refitted; that test will say so rather than letting it slide.
var epsilonByK = [...]float64{
	0: epsilonPlateau, // unused; K is at least 1
	1: 0.0,            // provably zero: any single symbol decodes a one-symbol block
	2: 0.9,
	3: 2.0,
	4: 2.5,
}

// epsilonPlateau applies from K=5 up, where the overhead stops growing with K.
const epsilonPlateau = 3.4

// failureTarget is the per-receiver decode failure rate everything here is
// sized against. §7.2 absorbs the residue with want-repair rather than
// over-provisioning every transmission for every peer.
const failureTarget = 0.02

// zScore is how many standard deviations of binomial loss to cover.
//
// It came DOWN from 2.0 as epsilon went up, which is the useful lesson here.
// The margin was never missing, it was charged to the wrong term — and widening
// the binomial interval to cover what was really codec overhead costs far more
// airtime, because that term scales with N while epsilon does not.
const zScore = 1.8

func epsilonFor(k int) float64 {
	if k > 0 && k < len(epsilonByK) {
		return epsilonByK[k]
	}
	return epsilonPlateau
}

// RepairCount returns how many repair symbols to send for a given loss rate.
//
// §7.2 suggests ceil(alpha*K) + 1. That under-provisions, for two reasons the
// formula does not account for:
//
//  1. The repair symbols are lost at the same rate as everything else. Sending
//     ceil(pK) extra replaces the systematic symbols expected to be lost, but
//     a p fraction of those extras is lost too.
//  2. Loss is random, so half of the receivers do worse than the mean. Sizing
//     to the mean means about half the network fails to decode.
//
// Measured: at K=10 and 20% loss the design's formula sends 13 symbols and only
// 5 of 12 receivers decoded. What is actually needed is the smallest N where the
// number received stays above K + epsilon even z standard deviations below the
// mean:
//
//	N(1-p) - z*sqrt(N*p*(1-p)) >= K + epsilon
//
// Solved by search rather than algebra, because N appears on both sides and the
// search is over at most a few dozen values. See epsilonByK for where the
// constants come from.
func RepairCount(k int, lossRate float64) int {
	if lossRate <= 0 {
		// A clean link decodes from the systematic prefix alone. One spare
		// still earns its place: at low K a single loss is a large fraction of
		// the block, and one symbol is cheap next to a repair round trip that
		// costs minutes.
		return 1
	}
	if lossRate > 0.9 {
		lossRate = 0.9
	}

	// K=1 is exact, not approximated.
	//
	// §7.2 item 5 already calls it a special case — "a single-packet bundle
	// needs no coding, just optional blind repeats" — and most DMs and small
	// post batches land here, so it is the case worth getting exactly right
	// rather than within a symbol. Every symbol of a one-symbol block decodes
	// it, so the question is only how many copies make it improbable that all
	// are lost: the smallest n with p^n <= the failure target.
	//
	// The Gaussian machinery below is a normal approximation to a binomial and
	// overshoots at tiny n, which at K=1 means paying an extra full packet on
	// the most common bundle on the mesh.
	if k == 1 {
		n := int(math.Ceil(math.Log(failureTarget) / math.Log(lossRate)))
		if n < 2 {
			n = 2 // never send a lone copy over a lossy link
		}
		return n - 1
	}

	target := float64(k) + epsilonFor(k)

	// N cannot exceed a few multiples of K even at extreme loss; cap the
	// search so a nonsense loss rate cannot produce an unbounded transmission.
	maxN := k*12 + 16
	for n := k + 1; n <= maxN; n++ {
		mean := float64(n) * (1 - lossRate)
		sd := math.Sqrt(float64(n) * lossRate * (1 - lossRate))
		if mean-zScore*sd >= target {
			return n - k
		}
	}
	return maxN - k
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

// Decoder reassembles a source block from whatever symbols arrive.
//
// State is keyed by (sender, bundleID) at the caller; a Decoder handles one
// block. Symbols may arrive in any order, duplicated, or not at all.
type Decoder struct {
	sender   identity.NodeID
	bundleID uint32
	k        int
	symSize  int
	origLen  int

	// rows holds the equations gathered so far: each is a mask over the source
	// symbols plus the XOR of their values.
	rows []equation
	seen map[uint16]bool

	solved [][]byte
	done   bool
}

type equation struct {
	mask []bool
	data []byte
}

// NewDecoder builds a decoder for a block.
func NewDecoder(sender identity.NodeID, bundleID uint32, k, symSize, origLen int) (*Decoder, error) {
	if k <= 0 || k > MaxK {
		return nil, fmt.Errorf("%w: K=%d", ErrBadK, k)
	}
	if symSize <= 0 || symSize > MaxSymbolSize {
		return nil, ErrSymbolSize
	}
	if origLen <= 0 || origLen > k*symSize {
		return nil, fmt.Errorf("declared payload length %d is impossible for K=%d symSize=%d",
			origLen, k, symSize)
	}
	return &Decoder{
		sender: sender, bundleID: bundleID, k: k,
		symSize: symSize, origLen: origLen,
		seen: map[uint16]bool{},
	}, nil
}

// Add feeds a symbol in. It reports whether the block is now decodable.
//
// Duplicates are ignored, which matters on a flooding mesh where the same
// symbol arrives by several paths.
func (d *Decoder) Add(s Symbol) (bool, error) {
	if d.done {
		return true, nil
	}
	if s.BundleID != d.bundleID || int(s.K) != d.k {
		return false, ErrMismatched
	}
	if len(s.Data) != d.symSize {
		return false, fmt.Errorf("%w: symbol is %d bytes, block uses %d",
			ErrSymbolSize, len(s.Data), d.symSize)
	}
	if d.seen[s.Index] {
		return d.done, nil
	}
	d.seen[s.Index] = true

	var m []bool
	if int(s.Index) < d.k {
		m = make([]bool, d.k)
		m[s.Index] = true
	} else {
		m = mask(d.sender, d.bundleID, s.Index, d.k)
	}
	d.rows = append(d.rows, equation{mask: m, data: append([]byte(nil), s.Data...)})

	// Only attempt a solve once there are at least K equations: fewer cannot
	// possibly determine K unknowns, and trying wastes work on every arrival.
	if len(d.rows) >= d.k {
		d.trySolve()
	}
	return d.done, nil
}

// Done reports whether the block has been recovered.
func (d *Decoder) Done() bool { return d.done }

// Received is how many distinct symbols have arrived.
func (d *Decoder) Received() int { return len(d.seen) }

// Payload returns the reassembled payload.
func (d *Decoder) Payload() ([]byte, error) {
	if !d.done {
		return nil, ErrNotDecoded
	}
	out := make([]byte, 0, d.k*d.symSize)
	for _, s := range d.solved {
		out = append(out, s...)
	}
	return out[:d.origLen], nil
}

// trySolve runs Gaussian elimination over GF(2).
//
// Belief propagation would be faster asymptotically, but at K ≤ 64 this is a
// 64x64 bit matrix and elimination takes microseconds — and unlike belief
// propagation it succeeds whenever the equations are independent, rather than
// only when a degree-1 symbol happens to be available to start the cascade.
// For small blocks that difference is the whole ballgame: a peeling decoder
// stalls on exactly the small-K cases this code exists to serve.
func (d *Decoder) trySolve() {
	n := d.k
	// Work on copies: a failed solve must leave the gathered rows intact for
	// the next arrival.
	masks := make([][]bool, len(d.rows))
	data := make([][]byte, len(d.rows))
	for i, r := range d.rows {
		masks[i] = append([]bool(nil), r.mask...)
		data[i] = append([]byte(nil), r.data...)
	}

	// Forward elimination.
	pivotRow := make([]int, n)
	for i := range pivotRow {
		pivotRow[i] = -1
	}
	row := 0
	for col := 0; col < n && row < len(masks); col++ {
		// Find a row with a 1 in this column.
		sel := -1
		for r := row; r < len(masks); r++ {
			if masks[r][col] {
				sel = r
				break
			}
		}
		if sel == -1 {
			continue // this column is not yet determined
		}
		masks[row], masks[sel] = masks[sel], masks[row]
		data[row], data[sel] = data[sel], data[row]

		for r := 0; r < len(masks); r++ {
			if r != row && masks[r][col] {
				xorInto(masks[r], masks[row])
				xorBytes(data[r], data[row])
			}
		}
		pivotRow[col] = row
		row++
	}

	// Every column must have a pivot, or the block is underdetermined.
	solved := make([][]byte, n)
	for col := 0; col < n; col++ {
		if pivotRow[col] == -1 {
			return
		}
		solved[col] = data[pivotRow[col]]
	}
	d.solved = solved
	d.done = true
}

func xorInto(dst, src []bool) {
	for i := range dst {
		dst[i] = dst[i] != src[i]
	}
}

func xorBytes(dst, src []byte) {
	for i := range dst {
		dst[i] ^= src[i]
	}
}
