package sneakernet

import (
	"fmt"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/vv"
)

// Exporter is the half of store.GossipStore an export needs.
//
// Narrow on purpose: an export reads areas, vectors and records and writes
// nothing. Taking the concrete store would let this grow an Apply call by
// accident, and "the thing that writes a stick" should not be able to modify
// the log.
type Exporter interface {
	Areas() []record.AreaTag
	Vector(area record.AreaTag) *vv.Vector
	Records(area record.AreaTag, r vv.Range) []*record.Record
}

// Export builds a carrier.
//
// # The two legs
//
// With reply nil this is the OUTWARD leg: everything held in every federated
// area, plus the vectors saying so. That is what somebody carries to a board
// they have never exchanged with.
//
// With reply set to a carrier that arrived, it is the RETURN leg: only what the
// other side is missing, computed from the vectors they wrote down. No
// conversation happened — their vectors came in on the stick, and this is the
// answer going back on it (§6.5's "satisfied at the next bundle exchange").
//
// An area the peer does not federate is still included on the outward leg,
// because we cannot know what they carry until they tell us, and omitting it
// would mean two round trips to discover a shared area. On the return leg their
// vectors say which areas they have, and an area absent from them is treated as
// empty rather than skipped — a peer who has just created the area needs
// everything, and that is indistinguishable from a peer who has never seen it.
// ExportOptions is what a sysop chose about this exchange.
type ExportOptions struct {
	Self identity.NodeID
	Now  uint32
	// Reply is a carrier that arrived; nil for the outward leg.
	Reply *Carrier
	// Areas restricts the exchange. Empty means every federated area.
	//
	// A sysop may want to carry the tech forum to a neighbouring board and not
	// the swap board, and there is no other control for that: federation is a
	// per-area decision about the MESH, and a stick is a different medium going
	// to a different place.
	Areas []record.AreaTag
	// Blobs are the file bodies to carry, from BlobsToCarry.
	Blobs []BlobRef
}

func Export(src Exporter, dict *bundle.Dictionary, opt ExportOptions) (*Carrier, error) {
	c := &Carrier{
		Origin:    opt.Self,
		CreatedAt: opt.Now,
		Vectors:   map[record.AreaTag]*vv.Vector{},
		Blobs:     opt.Blobs,
	}

	want := map[record.AreaTag]bool{}
	for _, a := range opt.Areas {
		want[a] = true
	}

	for _, area := range src.Areas() {
		if len(want) > 0 && !want[area] {
			continue
		}
		mine := src.Vector(area)
		c.Vectors[area] = mine

		// What the other side lacks. An absent peer vector is an empty one,
		// which asks for everything.
		theirs := vv.New()
		if opt.Reply != nil {
			if v, ok := opt.Reply.Vectors[area]; ok {
				theirs = v
			}
		}
		ranges := theirs.Missing(mine)

		var batch []*record.Record
		flush := func() error {
			if len(batch) == 0 {
				return nil
			}
			packed, err := bundle.Pack(&bundle.Bundle{Area: area, Records: batch}, dict)
			if err != nil {
				return fmt.Errorf("pack a bundle for area %x: %w", area[:], err)
			}
			if len(c.Bundles) >= MaxBundles {
				return fmt.Errorf("%w: this exchange needs more than %d bundles; "+
					"run it again after importing the reply", ErrTooMany, MaxBundles)
			}
			c.Bundles = append(c.Bundles, packed)
			batch = nil
			return nil
		}

		for _, r := range ranges {
			for _, rec := range src.Records(area, r) {
				batch = append(batch, rec)
				// bundle.MaxRecords is the mesh's batch limit and is reused
				// rather than widened: a stick full of bundles the radio could
				// not have produced would be a second format in all but name.
				if len(batch) == bundle.MaxRecords {
					if err := flush(); err != nil {
						return nil, err
					}
				}
			}
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Importer is the half of store.GossipStore an import needs.
type Importer interface {
	Apply(area record.AreaTag, recs []*record.Record) (int, error)
}

// ImportResult reports what a stick actually delivered.
type ImportResult struct {
	// Bundles that unpacked. A bundle that did not is counted in Rejected and
	// does not stop the rest.
	Bundles int
	// Records added to the log, excluding ones already held.
	Records int
	// Rejected bundles, with the reason.
	Rejected []string
}

// Import applies a carrier's records to the log.
//
// # Why one bad bundle does not fail the import
//
// A stick is carried by a person and may be old, partly written, or from a
// build that speaks a format this one does not. Refusing the whole exchange
// because one bundle failed would throw away everything that was fine, and the
// person is not coming back today. Each failure is recorded and reported.
//
// Verification is not relaxed to make that work: every record still goes
// through GossipStore.Apply, which checks signatures against the roster and
// enforces area rules. What is tolerated here is a bundle that will not
// UNPACK — a framing problem, not an authenticity one.
func Import(dst Importer, dicts *bundle.DictionarySet, c *Carrier) (ImportResult, error) {
	var res ImportResult
	for i, raw := range c.Bundles {
		b, err := bundle.Unpack(raw, dicts)
		if err != nil {
			res.Rejected = append(res.Rejected,
				fmt.Sprintf("bundle %d of %d did not unpack: %v", i+1, len(c.Bundles), err))
			continue
		}
		added, err := dst.Apply(b.Area, b.Records)
		if err != nil {
			// Apply's own error is about OUR storage rather than their bytes,
			// so it stops the import: continuing would mean writing some of a
			// stick into a database that is failing.
			return res, fmt.Errorf("applying bundle %d: %w", i+1, err)
		}
		res.Bundles++
		res.Records += added
	}
	return res, nil
}

// BlobsToCarry selects which held files a carrier should take.
//
// Takes store.File — this node's OWN file rows — rather than the network-wide
// catalog, for two reasons that happen to agree. Only a local row carries the
// full BLAKE3; a catalog entry carries the truncated wire hash, which is enough
// to answer "do I hold this?" and not enough to address a blob. And a node can
// only carry bytes it actually has, so the local rows are exactly the candidate
// set.
//
// Minus what the other side already has — which, on the return leg, is whatever
// their carrier declared. There is no request queue yet (§6.5), so this is the
// blunt version: take what they do not have and let the size ceilings decide.
//
// Skipping a file that is too large is deliberate rather than an error. A stick
// carrying nine files and refusing a tenth is more useful than one that refuses
// to be written, and the sysop is told which.
func BlobsToCarry(files []store.File, held *blobstore.Store, theyHave map[blobstore.Hash]bool) ([]BlobRef, []string) {
	var refs []BlobRef
	var skipped []string
	seen := map[blobstore.Hash]bool{}

	for _, f := range files {
		if seen[f.Hash] || theyHave[f.Hash] {
			continue
		}
		size, err := held.Size(f.Hash)
		if err != nil {
			// A row whose bytes are missing from the blob store. Not an error
			// worth stopping for: it means the two got out of step, and the
			// store's own maintenance is what reconciles them.
			continue
		}
		if uint64(size) > MaxBlobBytes {
			skipped = append(skipped,
				fmt.Sprintf("%s is %d bytes, over the %d-byte carrier limit", f.Name, size, MaxBlobBytes))
			continue
		}
		if len(refs) >= MaxBlobRefs {
			skipped = append(skipped,
				fmt.Sprintf("%s and later files: a carrier holds at most %d", f.Name, MaxBlobRefs))
			break
		}
		seen[f.Hash] = true
		refs = append(refs, BlobRef{Hash: f.Hash, Size: uint64(size)})
	}
	return refs, skipped
}
