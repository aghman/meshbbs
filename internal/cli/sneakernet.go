package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aghman/meshbbs/internal/blobstore"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/sneakernet"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// newSneakernetCmd builds the `sneakernet` group (§7.5, §6.5 fetch path 2).
//
// # Why a BBS has a USB command at all
//
// `[D8]` named two fetch paths and this is the second. It is also the only
// federation a node gets when there is no radio in range and no IP route, which
// on a mesh network is not a corner case — it is the reason the FidoNet era
// worked at all.
func newSneakernetCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sneakernet",
		Short: "Exchange records and files on removable media (§7.5)",
		Long: `Federate by carrying a file, when the mesh cannot and there is no IP route.

An exchange is TWO trips, because a hand-off has no round trip in it:

    meshbbs sneakernet export away.mbx          # on your board
    ... carry it to the other board ...
    meshbbs sneakernet import away.mbx          # on theirs
    meshbbs sneakernet export --reply-to away.mbx back.mbx
    ... carry it home ...
    meshbbs sneakernet import back.mbx          # on yours

The first carrier says what you hold. The reply contains only what you were
missing, worked out from the vectors you sent — no conversation happens, which
is what makes this work between boards that have never met.

Every carrier also asks. Files your users have queued (§6.5) ride along as a
list of content hashes, and a board answering with --files sends those and
nothing else:

    meshbbs file request utils kermit.zip --for austin
    meshbbs file requests

That is fetch path 2. The mesh never carries file bytes, at any size, so a
stick is the other way they move.`,
	}
	cmd.AddCommand(newSneakernetExportCmd(e), newSneakernetImportCmd(e))
	return cmd
}

func newSneakernetExportCmd(e *env) *cobra.Command {
	var replyTo string
	var areas []string
	var withFiles bool

	cmd := &cobra.Command{
		Use:   "export <file>",
		Short: "Write a carrier for another board",
		Long: `Write a carrier file.

Without --reply-to this is the outward leg: everything in every federated area.
With it, only what the other board is missing, from the vectors on their carrier.

--area restricts what travels. Federation is a per-area decision about the MESH;
a stick goes somewhere else, to someone specific, so it gets its own choice.

--files carries file bodies as well as records. This is the one path §7.5 allows
bytes on — the mesh never carries them at any size — and it is off by default
because a carrier with files on it is a different size of object entirely.

With --reply-to, --files answers the requests on THEIR carrier and carries
nothing else. Without one it is the blunt version: everything they do not have,
which is all an opening carrier can do, since nobody has asked yet.

Whatever your own users have queued rides on every carrier either way. Asking
is 16 bytes and needs no blob store.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := e.nodeKey()
			if err != nil {
				return err
			}
			dict, _, err := federationDictionaries()
			if err != nil {
				return err
			}

			var reply *sneakernet.Carrier
			if replyTo != "" {
				// Read WITHOUT a blob store: an export only needs the vectors,
				// and pulling somebody's files in as a side effect of answering
				// them would be a surprise.
				f, err := os.Open(replyTo)
				if err != nil {
					return err
				}
				reply, err = sneakernet.ReadManifest(f)
				f.Close()
				if err != nil {
					return fmt.Errorf("read %s: %w", replyTo, err)
				}
			}

			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				gs, err := store.NewGossipStore(ctx, st, nil)
				if err != nil {
					return err
				}
				tags, err := resolveAreas(ctx, st, areas)
				if err != nil {
					return err
				}

				// Our own queue rides every carrier, both legs, whether or not
				// this one carries files: asking costs 16 bytes on a medium
				// with no airtime budget, and a carrier that answers somebody
				// while forgetting to ask is a wasted trip (§6.5).
				wants, err := st.OpenFileRequestHashes(ctx, sneakernet.MaxRequests)
				if err != nil {
					return err
				}

				opt := sneakernet.ExportOptions{
					Self:     key.ID(),
					Now:      uint32(e.clock.Now().Unix()),
					Reply:    reply,
					Areas:    tags,
					Requests: wants,
				}

				var blobs *blobstore.Store
				var plan sneakernet.BlobPlan
				out := cmd.OutOrStdout()
				if withFiles {
					blobs, err = openBlobs(e)
					if err != nil {
						return err
					}
					plan, err = chooseBlobs(ctx, st, blobs, reply)
					if err != nil {
						return err
					}
					opt.Blobs = plan.Refs
					for _, s := range plan.Skipped {
						fmt.Fprintf(out, "skipping %s\n", s)
					}
				} else if reply != nil && len(reply.Requests) > 0 {
					// The one case where doing nothing is worth a line. They
					// asked, this carrier will not answer, and the person who
					// asked finds out a week later on somebody else's desk.
					fmt.Fprintf(out, "%s asked for %d file(s) and this carrier holds none — "+
						"re-run with --files to answer.\n",
						reply.Origin.Short(), len(reply.Requests))
				}

				c, err := sneakernet.Export(gs, dict, opt)
				if err != nil {
					return err
				}

				// Written to a temporary name and renamed, because the usual
				// destination is a removable drive and a half-written carrier
				// that looks complete is worse than one that is obviously not
				// there.
				tmp := args[0] + ".partial"
				f, err := os.Create(tmp)
				if err != nil {
					return err
				}
				err = sneakernet.Write(f, c, blobOpener(blobs))
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					os.Remove(tmp)
					return err
				}
				if err := os.Rename(tmp, args[0]); err != nil {
					return err
				}

				info, _ := os.Stat(args[0])
				fmt.Fprintf(out, "Wrote %s", args[0])
				if info != nil {
					fmt.Fprintf(out, " (%s)", humanBytes(info.Size()))
				}
				fmt.Fprintf(out, "\n  %d area(s), %d bundle(s), %d file(s)\n",
					len(c.Vectors), len(c.Bundles), len(c.Blobs))
				if len(c.Requests) > 0 {
					fmt.Fprintf(out, "  asking for %d file(s) queued here (§6.5)\n", len(c.Requests))
				}
				for _, h := range plan.Unanswered {
					fmt.Fprintf(out, "  cannot answer their request for %s — "+
						"not held in any federated file area here\n", describeHash(h))
				}
				if reply == nil {
					fmt.Fprintf(out, "\nThis is the outward leg. On the other board:\n"+
						"  meshbbs sneakernet import %s\n"+
						"  meshbbs sneakernet export --reply-to %s back.mbx\n",
						filepath.Base(args[0]), filepath.Base(args[0]))
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&replyTo, "reply-to", "",
		"a carrier that arrived; send only what its writer is missing")
	cmd.Flags().StringArrayVar(&areas, "area", nil,
		"restrict to this area, repeatable (default: every federated area)")
	cmd.Flags().BoolVar(&withFiles, "files", false,
		"carry file bodies too — the one path §7.5 allows bytes on")
	return cmd
}

func newSneakernetImportCmd(e *env) *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Apply a carrier from another board",
		Long: `Read a carrier and apply what it holds.

Records are verified exactly as they are off the air: signatures against the
roster, area rules, sequence conflicts. A stick is not a way around any of that,
and one whose records do not verify simply adds nothing.

Files are stored under their own content hash, and a body that does not match
what the carrier declared is refused rather than kept. A body somebody here had
queued with ` + "`meshbbs file request`" + ` becomes a catalog entry in the area they
asked for it in, and they are told the next time they log in (§6.5).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, dicts, err := federationDictionaries()
			if err != nil {
				return err
			}
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				gs, err := store.NewGossipStore(ctx, st, nil)
				if err != nil {
					return err
				}
				f, err := os.Open(args[0])
				if err != nil {
					return err
				}
				defer f.Close()

				out := cmd.OutOrStdout()

				// A dry run reads the MANIFEST only. Passing a nil blob store to
				// Read would refuse a carrier that declares files, which is
				// exactly the carrier somebody most wants to look at before
				// committing to it.
				var c *sneakernet.Carrier
				if dryRun {
					c, err = sneakernet.ReadManifest(f)
				} else {
					var blobs *blobstore.Store
					if blobs, err = openBlobs(e); err != nil {
						return err
					}
					c, err = sneakernet.Read(f, blobs)
				}
				if err != nil {
					return fmt.Errorf("read %s: %w", args[0], err)
				}
				fmt.Fprintf(out, "From %s, written %s\n",
					c.Origin.Short(), sinceWhen(e.clock.Now(), int64(c.CreatedAt)))

				if dryRun {
					fmt.Fprintf(out, "  %d area(s), %d bundle(s), %d file(s)\n",
						len(c.Vectors), len(c.Bundles), len(c.Blobs))
					if len(c.Requests) > 0 {
						fmt.Fprintf(out, "  asking for %d file(s)\n", len(c.Requests))
					}
					fmt.Fprintf(out, "\nNothing was applied (--dry-run). Files were not stored either.\n")
					return nil
				}

				res, err := sneakernet.Import(gs, dicts, c)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "  %d record(s) added from %d bundle(s)\n", res.Records, res.Bundles)
				if len(c.Blobs) > 0 {
					fmt.Fprintf(out, "  %d file(s) stored\n", len(c.Blobs))
				}
				for _, why := range res.Rejected {
					fmt.Fprintf(out, "  skipped: %s\n", why)
				}

				answered, err := closeRequests(ctx, st, c, out)
				if err != nil {
					return err
				}

				if len(c.Requests) > 0 {
					// Said after the arrivals, because this is the next trip's
					// work rather than this one's result.
					fmt.Fprintf(out, "\n%s is asking for %d file(s). To answer:\n"+
						"  meshbbs sneakernet export --reply-to %s --files back.mbx\n",
						c.Origin.Short(), len(c.Requests), filepath.Base(args[0]))
				}
				if res.Records == 0 && len(res.Rejected) == 0 && answered == 0 {
					// Not a failure and worth saying plainly, because "nothing
					// happened" and "it did not work" look identical otherwise.
					fmt.Fprintf(out, "\nEverything on this carrier was already held.\n")
				}
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"say what the carrier holds without applying it")
	return cmd
}

// closeRequests turns arrived bodies into catalog rows and closes the requests
// they answer (§6.5 fetch path 2), reporting how many were answered.
//
// # Why an unrequested body is worth a line
//
// A carrier written with a blunt `--files` may hold content nobody here asked
// for. Its bytes are in the blob store — content-addressed, verified — and
// there is no catalog row for them, because there is no area to file them in
// and no name to file them under: a body arrives with a hash and a length, and
// the name lives in the FILE record, which may or may not be on the same stick.
//
// That is a defensible thing for the format to do and a terrible thing to do
// silently, so the sysop is told the count. A request is what turns a body into
// a file, and this is the line that says so.
func closeRequests(ctx context.Context, st *store.Store, c *sneakernet.Carrier, out io.Writer) (int, error) {
	var answered, unasked int
	for _, ref := range c.Blobs {
		reqs, err := st.SatisfyFileRequests(ctx, ref.Hash, int64(ref.Size))
		if err != nil {
			return answered, err
		}
		if len(reqs) == 0 {
			unasked++
			continue
		}
		answered += len(reqs)
		for _, r := range reqs {
			if r.Filed() {
				fmt.Fprintf(out, "  answered: %s/%s for %s\n", r.Area, r.Name, r.Nick)
				continue
			}
			fmt.Fprintf(out, "  %s/%s for %s: %s\n", r.Area, r.Name, r.Nick, r.Note)
		}
	}
	if unasked > 0 {
		fmt.Fprintf(out, "  %d file(s) nobody here had asked for: held as content, "+
			"with no catalog entry to reach them by\n", unasked)
	}
	return answered, nil
}

func openBlobs(e *env) (*blobstore.Store, error) {
	dir, err := e.cfg.FilesPath()
	if err != nil {
		return nil, err
	}
	return blobstore.Open(filepath.Join(dir, "blobs"))
}

func blobOpener(bs *blobstore.Store) func(blobstore.Hash) (io.ReadCloser, error) {
	if bs == nil {
		return nil
	}
	return func(h blobstore.Hash) (io.ReadCloser, error) { return bs.Open(h) }
}

// resolveAreas turns --area names into tags, refusing one that does not exist.
//
// Refusing rather than ignoring: a sysop who mistypes an area name and gets a
// carrier without it would not find out until the other board did not receive
// it, which is a week later and on somebody else's desk.
func resolveAreas(ctx context.Context, st *store.Store, names []string) ([]record.AreaTag, error) {
	var out []record.AreaTag
	for _, n := range names {
		a, err := st.GetAnyArea(ctx, n)
		if err != nil {
			return nil, fmt.Errorf("--area %s: %w", n, err)
		}
		if !a.Federated {
			return nil, fmt.Errorf("--area %s is local only; federate it first, "+
				"or leave it off the carrier deliberately", n)
		}
		out = append(out, a.Tag)
	}
	return out, nil
}

// chooseBlobs picks the file bodies to carry.
//
// Candidates come from FEDERATED file areas only, requests included. A
// local-only area is a sysop saying this content does not leave the building,
// and somebody else's request does not overrule that — an unanswerable request
// is reported back rather than quietly satisfied out of a private area.
func chooseBlobs(ctx context.Context, st *store.Store, bs *blobstore.Store, reply *sneakernet.Carrier) (sneakernet.BlobPlan, error) {
	areas, err := st.ListFileAreas(ctx)
	if err != nil {
		return sneakernet.BlobPlan{}, err
	}
	var files []store.File
	for _, a := range areas {
		if !a.Federated {
			continue
		}
		in, err := st.ListFiles(ctx, a.Name)
		if err != nil {
			return sneakernet.BlobPlan{}, err
		}
		files = append(files, in...)
	}

	theyHave := map[blobstore.Hash]bool{}
	var wanted []sneakernet.WireHash
	if reply != nil {
		for _, ref := range reply.Blobs {
			theyHave[ref.Hash] = true
		}
		wanted = reply.Requests
	}
	return sneakernet.BlobsToCarry(files, bs, theyHave, wanted), nil
}

// describeHash names a hash the way a sysop can match it against a listing.
//
// The full 16 bytes are unreadable out loud and the short form is what every
// other ID in this codebase renders as, so this is the same bargain `[D9]`
// makes everywhere else: enough to identify, short enough to say.
func describeHash(h sneakernet.WireHash) string { return fmt.Sprintf("%x…", h[:6]) }

// humanBytes renders a size the way a sysop deciding whether it fits would want.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
