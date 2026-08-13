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
is what makes this work between boards that have never met.`,
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
because a carrier with files on it is a different size of object entirely.`,
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

				opt := sneakernet.ExportOptions{
					Self:  key.ID(),
					Now:   uint32(e.clock.Now().Unix()),
					Reply: reply,
					Areas: tags,
				}

				var blobs *blobstore.Store
				out := cmd.OutOrStdout()
				if withFiles {
					blobs, err = openBlobs(e)
					if err != nil {
						return err
					}
					refs, skipped, err := chooseBlobs(ctx, st, blobs, reply)
					if err != nil {
						return err
					}
					opt.Blobs = refs
					for _, s := range skipped {
						fmt.Fprintf(out, "skipping %s\n", s)
					}
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
what the carrier declared is refused rather than kept.`,
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
				if res.Records == 0 && len(res.Rejected) == 0 {
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
func chooseBlobs(ctx context.Context, st *store.Store, bs *blobstore.Store, reply *sneakernet.Carrier) ([]sneakernet.BlobRef, []string, error) {
	areas, err := st.ListFileAreas(ctx)
	if err != nil {
		return nil, nil, err
	}
	var files []store.File
	for _, a := range areas {
		if !a.Federated {
			continue
		}
		in, err := st.ListFiles(ctx, a.Name)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, in...)
	}

	theyHave := map[blobstore.Hash]bool{}
	if reply != nil {
		for _, ref := range reply.Blobs {
			theyHave[ref.Hash] = true
		}
	}
	refs, skipped := sneakernet.BlobsToCarry(files, bs, theyHave)
	return refs, skipped, nil
}

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
