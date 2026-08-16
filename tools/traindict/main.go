// Command traindict builds dictionary 1 from internal/dictcorpus (design §7.4).
//
// # What zstd --train does that this has to do by hand
//
// klauspost/compress exposes zstd.BuildDict, which is not the same thing as
// `zstd --train`. It builds the entropy tables — the literal Huffman table and
// the offset, match-length and literal-length FSE tables — from sample content,
// and it takes the dictionary CONTENT verbatim from History. It does not run
// COVER or fastcover, the substring-selection algorithms that decide WHICH bytes
// belong in a dictionary. So the selection is implemented here.
//
// Using the reference `zstd` binary instead was rejected for the reason §4 gives
// about cgo: a build-time dependency on a C tool nobody has installed is how a
// reproducible artifact stops being reproducible. The algorithm below is
// deliberately simple, deterministic, and about eighty lines.
//
// # Entropy tables are most of the win
//
// Dictionary 0 is a RAW dictionary — content with no entropy tables at all
// (bundle.NewRawDictionary), which is why it buys about 1% over plain zstd. Even
// with identical content, dictionary 1 would beat it simply by carrying tables
// trained on real samples.
//
// # It refuses to overwrite
//
// A dictionary is a wire-format constant. §7.4 says old dictionaries stay
// supported, and v0.21's conformance work made "supersede, never edit" a rule
// rather than a preference: rewriting a dictionary in place leaves every peer on
// an older build unable to read anything, under an ID that claims agreement. So
// retraining produces dictionary 2 and a second file, and this command will not
// clobber the first.
//
// # It is reproducible, and it cannot bootstrap itself
//
// Selection is deterministic — candidates are ordered by score and then by text,
// never left in map order — so retraining the same corpus yields a byte-identical
// artifact. That is the property that makes a committed binary blob auditable
// rather than something that merely appeared one day.
//
// The thing it cannot do is rebuild from nothing. This command imports
// internal/bundle, which embeds dict1.zdict, so deleting that file stops this
// command compiling and the error names go:embed rather than the cause. This is
// not a workflow anyone should need: a retrain ships under a NEW id and a new
// file, leaving dict1.zdict untouched, and the refusal below enforces that. If
// the artifact has been deleted anyway, restore it with git and train to a
// different -out.
//
// Run: go run ./tools/traindict
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/dictcorpus"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/klauspost/compress/zstd"
)

// contentBudget is how many bytes of dictionary CONTENT to select.
//
// 16 KiB, and the sweep this command prints is why. Measured against a 113 KiB
// training corpus and a 46 KiB holdout it has never seen:
//
//	budget    dict bytes   train   holdout   gap
//	 4 KiB          4223   1.313     1.230   0.082
//	 8 KiB          8316   1.492     1.280   0.211
//	16 KiB         16508   1.550     1.298   0.252
//	32 KiB         32888   1.979     1.381   0.598
//	64 KiB         65655   3.193     1.407   1.786
//
// Holdout is still climbing at 32 KiB, so the case for stopping at 16 is not
// that more stops helping. It is that the GAP more than doubles there, and a
// 33 KiB dictionary built from a 113 KiB corpus is carrying a third of its own
// training data. Past that point the holdout figure stops being trustworthy in
// its own right: this holdout shares a register with the training half by
// construction, and the more the dictionary leans on memorised fragments the
// more that shared register flatters it. At 64 KiB the dictionary triples on
// data it has seen and gains 0.026 on data it has not, which is the shape of
// memorisation and not of compression.
//
// The measurement that would justify a larger dictionary is a larger and more
// varied CORPUS, not a larger budget over this one — §7.4 wants real archives
// and particularly FTN echomail. Until that exists, 16 KiB is also the frugal
// answer: a decoder keeps the whole dictionary resident, and on a Pi Zero
// running the board, the SSH server and the radio, this is not where to spend
// memory generously.
const contentBudget = 16 << 10

// Candidate substring lengths, longest first.
//
// Longest-first matters: a long phrase is selected before its own sub-phrases,
// which are then rejected as already contained. That is what keeps the budget
// from filling with a hundred variations of the same sentence.
var candidateLengths = []int{64, 48, 32, 24, 16, 12, 8}

// minOccurrences is how many times a fragment must appear to be worth carrying.
// Twice is the real threshold — a fragment used once saves nothing.
const minOccurrences = 2

// referenceOverhead approximates what a zstd match costs to encode. Subtracting
// it stops short fragments scoring on length alone.
const referenceOverhead = 3

func main() {
	out := flag.String("out", "internal/bundle/dict1.zdict", "where to write the dictionary")
	id := flag.Uint("id", 1, "dictionary ID")
	sweep := flag.Bool("sweep", true, "report the holdout ratio at several content budgets")
	force := flag.Bool("force", false, "overwrite an existing dictionary (see the package comment; you almost certainly want a new ID instead)")
	flag.Parse()

	if err := run(*out, uint8(*id), *sweep, *force); err != nil {
		fmt.Fprintf(os.Stderr, "traindict: %v\n", err)
		os.Exit(1)
	}
}

func run(outPath string, id uint8, sweep, force bool) error {
	train, err := dictcorpus.Train()
	if err != nil {
		return err
	}
	holdout, err := dictcorpus.Holdout()
	if err != nil {
		return err
	}

	trainSamples, err := sampleBodies(train)
	if err != nil {
		return err
	}
	holdoutSamples, err := sampleBodies(holdout)
	if err != nil {
		return err
	}
	fmt.Printf("train:   %d bundles, %d bytes\n", len(trainSamples), totalLen(trainSamples))
	fmt.Printf("holdout: %d bundles, %d bytes\n\n", len(holdoutSamples), totalLen(holdoutSamples))

	if sweep {
		// Both halves, because the gap between them is the number that decides
		// the budget. A dictionary that keeps improving on training data while
		// holdout flattens has started memorising the corpus, and every byte
		// after that point is memory spent on samples no peer will ever send.
		baseNone, err := bundle.NewRawDictionary(9, nil)
		if err != nil {
			return err
		}
		basis, err := ratioWith(baseNone, holdoutSamples)
		baseNone.Close()
		if err != nil {
			return err
		}
		fmt.Printf("plain zstd on holdout: %.3fx\n", basis)
		fmt.Println("content budget sweep:")
		fmt.Printf("  %-12s %-10s %-9s %-9s %s\n", "budget", "dict bytes", "train", "holdout", "gap")
		for _, budget := range []int{4 << 10, 8 << 10, 16 << 10, 32 << 10, 64 << 10} {
			raw, err := build(id, trainSamples, budget)
			if err != nil {
				return err
			}
			tr, err := measure(id, raw, trainSamples)
			if err != nil {
				return err
			}
			hr, err := measure(id, raw, holdoutSamples)
			if err != nil {
				return err
			}
			fmt.Printf("  %5d KiB    %-10d %-9.3f %-9.3f %.3f\n",
				budget>>10, len(raw), tr, hr, tr-hr)
		}
		fmt.Println()
	}

	raw, err := build(id, trainSamples, contentBudget)
	if err != nil {
		return err
	}

	if !force {
		if _, err := os.Stat(outPath); err == nil {
			return fmt.Errorf("%s already exists.\n"+
				"A dictionary is a wire-format constant and §7.4 keeps old ones supported, so a\n"+
				"retrain ships as a NEW id and a new file rather than replacing this one. Editing it\n"+
				"in place would leave every peer on an older build unable to decode anything, under a\n"+
				"dictionary ID that claims they agree.\n"+
				"  new dictionary:  go run ./tools/traindict -id 2 -out internal/bundle/dict2.zdict\n"+
				"  really replace:  -force", outPath)
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	if err := os.WriteFile(outPath, raw, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n\n", outPath, len(raw))

	return report(id, raw, train, holdout)
}

// ---------------------------------------------------------------------------
// Samples
// ---------------------------------------------------------------------------

// sampleBodies turns a corpus half into the byte strings that actually get
// compressed: encoded bundle bodies.
//
// Training on bundle bodies rather than on raw post text is the whole reason the
// numbers mean anything. A bundle body is a third signature by weight, and those
// bytes are incompressible; a dictionary trained on prose alone would be tuned
// for a payload that never travels by itself.
func sampleBodies(s *dictcorpus.Set) ([][]byte, error) {
	var out [][]byte
	add := func(area string, recs []*record.Record) error {
		if len(recs) == 0 {
			return nil
		}
		b := &bundle.Bundle{
			Area:    record.AreaTagFor(area),
			BaseTS:  1700000000,
			Records: recs,
		}
		body, err := bundle.EncodeBody(b)
		if err != nil {
			return err
		}
		out = append(out, body)
		return nil
	}

	// Batch sizes a mesh actually produces. §1.1's budget is about ten packets a
	// day, so a bundle of one is the common case and a bundle of ten is a busy
	// day — not an arbitrary chunking.
	for _, size := range []int{1, 1, 2, 3, 5, 10} {
		for i := 0; i+size <= len(s.Posts); i += size {
			if err := add("general", s.Posts[i:i+size]); err != nil {
				return nil, err
			}
		}
	}
	for _, size := range []int{1, 3, 6} {
		for i := 0; i+size <= len(s.Files); i += size {
			if err := add("files/uploads", s.Files[i:i+size]); err != nil {
				return nil, err
			}
		}
	}
	for i := range s.Doors {
		if err := add("league/x", s.Doors[i:i+1]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func totalLen(samples [][]byte) int {
	n := 0
	for _, s := range samples {
		n += len(s)
	}
	return n
}

// ---------------------------------------------------------------------------
// Content selection — the part BuildDict does not do
// ---------------------------------------------------------------------------

type candidate struct {
	text  string
	score int
}

// selectContent picks the dictionary content: the fragments worth referencing.
//
// Greedy by estimated bytes saved. Deterministic throughout — candidates are
// sorted by score and then by text, never left in map order, because the output
// is a committed artifact and §6.2.1 rule 2 is about exactly this class of
// accident.
//
// Signatures select themselves out without a special case: 64 random bytes never
// appear twice, so no fragment of one ever reaches minOccurrences.
func selectContent(samples [][]byte, budget int) []byte {
	var cands []candidate
	seen := map[string]bool{}

	for _, l := range candidateLengths {
		counts := map[string]int{}
		for _, s := range samples {
			for i := 0; i+l <= len(s); i++ {
				counts[string(s[i:i+l])]++
			}
		}
		for text, n := range counts {
			if n < minOccurrences || seen[text] {
				continue
			}
			seen[text] = true
			cands = append(cands, candidate{text: text, score: (n - 1) * (l - referenceOverhead)})
		}
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if len(cands[i].text) != len(cands[j].text) {
			return len(cands[i].text) > len(cands[j].text)
		}
		return cands[i].text < cands[j].text
	})

	// Accept in descending value, skipping anything already covered. Collected
	// in this order and REVERSED at the end: zstd encodes nearer offsets more
	// cheaply, so the most valuable content belongs at the end of the dictionary,
	// closest to the data being compressed.
	var picked []string
	acc := make([]byte, 0, budget)
	for _, c := range cands {
		if len(acc)+len(c.text) > budget {
			continue
		}
		if bytes.Contains(acc, []byte(c.text)) {
			continue
		}
		acc = append(acc, c.text...)
		picked = append(picked, c.text)
		if len(acc) >= budget {
			break
		}
	}

	out := make([]byte, 0, len(acc))
	for i := len(picked) - 1; i >= 0; i-- {
		out = append(out, picked[i]...)
	}
	return out
}

func build(id uint8, samples [][]byte, budget int) ([]byte, error) {
	content := selectContent(samples, budget)
	if len(content) < 8 {
		return nil, fmt.Errorf("selected only %d bytes of content; the corpus has no repeated material", len(content))
	}
	return zstd.BuildDict(zstd.BuildDictOptions{
		ID:       uint32(id),
		Contents: samples,
		History:  content,
		Level:    zstd.SpeedBestCompression,
	})
}

// ---------------------------------------------------------------------------
// Measurement
// ---------------------------------------------------------------------------

func measure(id uint8, raw []byte, samples [][]byte) (float64, error) {
	d, err := bundle.NewDictionary(id, raw)
	if err != nil {
		return 0, err
	}
	defer d.Close()
	return ratioWith(d, samples)
}

// ratioWith is the aggregate ratio over samples: total in over total out, not a
// mean of per-sample ratios. Those are different numbers and the second one
// flatters small bundles, which are the majority of the corpus by count and a
// minority of it by bytes.
func ratioWith(d *bundle.Dictionary, samples [][]byte) (float64, error) {
	in, out := 0, 0
	for _, s := range samples {
		packed, err := d.Compress(s)
		if err != nil {
			return 0, err
		}
		in += len(s)
		out += len(packed)
	}
	return float64(in) / float64(out), nil
}

// report prints the comparison §12.7 asks to be gated on, per record kind.
func report(id uint8, raw []byte, train, holdout *dictcorpus.Set) error {
	none, err := bundle.NewRawDictionary(9, nil)
	if err != nil {
		return err
	}
	defer none.Close()
	d0, err := bundle.Dictionary0()
	if err != nil {
		return err
	}
	defer d0.Close()
	d1, err := bundle.NewDictionary(id, raw)
	if err != nil {
		return err
	}
	defer d1.Close()

	kinds := []struct {
		name string
		set  *dictcorpus.Set
		pick func(*dictcorpus.Set) []*record.Record
		area string
	}{
		{"posts", holdout, func(s *dictcorpus.Set) []*record.Record { return s.Posts }, "general"},
		{"files", holdout, func(s *dictcorpus.Set) []*record.Record { return s.Files }, "files/uploads"},
		{"doors", holdout, func(s *dictcorpus.Set) []*record.Record { return s.Doors }, "league/x"},
	}

	fmt.Println("holdout ratios by bundle shape (never trained on):")
	fmt.Printf("  %-8s %-6s %8s %8s %8s %8s\n", "kind", "n", "body", "zstd", "dict0", "dict1")
	for _, k := range kinds {
		recs := k.pick(k.set)
		for _, n := range []int{1, 5} {
			if n > len(recs) {
				continue
			}
			b := &bundle.Bundle{Area: record.AreaTagFor(k.area), BaseTS: 1700000000, Records: recs[:n]}
			body, err := bundle.EncodeBody(b)
			if err != nil {
				return err
			}
			cn, _ := none.Compress(body)
			c0, _ := d0.Compress(body)
			c1, _ := d1.Compress(body)
			fmt.Printf("  %-8s %-6d %8d %8d %8d %8d   dict1 %.2fx vs plain %.2fx\n",
				k.name, n, len(body), len(cn), len(c0), len(c1),
				float64(len(body))/float64(len(c1)), float64(len(body))/float64(len(cn)))
		}
	}

	ts, err := sampleBodies(train)
	if err != nil {
		return err
	}
	hs, err := sampleBodies(holdout)
	if err != nil {
		return err
	}
	tr, err := measure(id, raw, ts)
	if err != nil {
		return err
	}
	hr, err := measure(id, raw, hs)
	if err != nil {
		return err
	}
	fmt.Printf("\noverall: train %.3fx, holdout %.3fx", tr, hr)
	if tr > hr*1.5 {
		fmt.Printf("   <- train is far ahead of holdout; the dictionary has memorised rather than generalised")
	}
	fmt.Println()
	return nil
}
