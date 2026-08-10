package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// newDevVectorCmd prints this node's per-area sync state.
//
// # Why this exists
//
// Convergence is defined as two nodes holding the same version vector for every
// federated area (§7.3). Until now there was no way to read one from outside the
// process: the five-minute `federation status` line reports COUNTERS — digests
// sent, symbols decoded, records added — and a counter says what happened, not
// what state the node ended in. Proving a sync worked therefore meant two
// hand-typed SQL queries, one per host, and two queries agreeing is not the same
// as two nodes agreeing. They can agree for the wrong reason: a query counting
// rows counts the DM records the gossip store deliberately refuses to replicate,
// and a query that filters them has just reimplemented typeFilterSQL's job
// slightly differently.
//
// So this asks GossipStore.Vector — the same function the wire uses. What it
// prints is what a digest would carry, which makes `diff <(a) <(b)` an actual
// proof rather than a suggestive coincidence. The contiguous-high-water rule is
// part of that: a node holding 1, 2 and 4 reports 2, and two nodes that disagree
// about whether the gap at 3 exists show it here.
//
// Safe to run against a live `serve`: the database is WAL, so this takes a read
// snapshot without blocking the federation loop.
func newDevVectorCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "vector",
		Short: "Print the per-area version vector, as a peer would see it",
		Long: `Print this node's version vector for every federated area.

Convergence between two instances means these match. Capture one from each side
and diff them:

    meshbbs --data-dir ~/bench/a dev vector > a.txt
    meshbbs --data-dir ~/bench/b dev vector > b.txt
    diff a.txt b.txt

The count and hash are exactly what a digest puts on the air, read through the
same code path, so a match here is the same claim the protocol makes rather than
a separate one that happens to agree.

Sequence numbers are CONTIGUOUS high-water marks, not maxima: a node holding
records 1, 2 and 4 reports 2, because claiming 4 would tell peers it has 3 and
anti-entropy would never ask for it again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				names, err := areaNames(ctx, st)
				if err != nil {
					return err
				}

				// A read error is fatal here rather than logged-and-empty. The
				// engine treats a failed vector read as something to retry, which
				// is right for a scheduler and wrong for a diagnostic: an area
				// printed empty because its query failed looks exactly like an
				// area that is empty, and this command exists to be trusted.
				var readErr error
				gs, err := store.NewGossipStore(ctx, st, func(e error) {
					if readErr == nil {
						readErr = e
					}
				})
				if err != nil {
					return err
				}

				out := cmd.OutOrStdout()
				areas := gs.Areas()
				for _, tag := range areas {
					name, ok := names[tag]
					if !ok {
						// A federated area of a kind this build does not list.
						// Print the tag rather than a blank: an unreadable line
						// is better than a line that looks like a missing area.
						name = "(unknown)"
					}
					v := gs.Vector(tag)
					h := v.Hash()
					fmt.Fprintf(out, "%s tag=%s count=%d hash=%s\n",
						name, hex.EncodeToString(tag[:]), v.Count(), hex.EncodeToString(h[:]))
					// Origins() is sorted, so this output is stable across runs
					// and across hosts — which is what makes it diffable.
					for _, origin := range v.Origins() {
						fmt.Fprintf(out, "  origin=%s seq=%d\n", origin.Compact(), v.Get(origin))
					}
				}
				if readErr != nil {
					return fmt.Errorf("reading version vectors: %w", readErr)
				}
				if len(areas) == 1 {
					// Only the roster. Worth saying, because "nothing federates
					// yet" and "the sync is broken" produce identical diffs.
					fmt.Fprintf(out, "\nNo federated areas beyond the roster; `meshbbs area federate <name>` adds one.\n")
				}
				return nil
			})
		},
	}
}

// areaNames maps area tags back to the names a sysop typed.
//
// A tag is a hash of a name (§6.3) and cannot be reversed, so the only way to
// label the output is to look up every area this node knows and index by tag.
// Both kinds are listed: a file area federates its catalog exactly as a message
// area federates its posts, and a diff that says "a4f21b0c" where the other side
// says "files" is a diff nobody can read.
func areaNames(ctx context.Context, st *store.Store) (map[record.AreaTag]string, error) {
	names := map[record.AreaTag]string{store.RosterArea: "roster"}
	msg, err := st.ListAreas(ctx)
	if err != nil {
		return nil, err
	}
	files, err := st.ListFileAreas(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range append(msg, files...) {
		names[a.Tag] = a.Name
	}
	return names, nil
}
