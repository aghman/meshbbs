package cli

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// newFileCmd builds the `file` group.
//
// SFTP has nowhere to carry a description — a client uploads bytes and a name,
// and there is no field for anything else — so until this existed a file's
// description could only ever be the empty string it was inserted with. That
// makes the catalog less useful locally and, once FILE records are published,
// on every BBS that receives it (§6.5).
func newFileCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "file",
		Short: "Inspect and annotate the file catalog",
	}
	cmd.AddCommand(newFileListCmd(e), newFileDescribeCmd(e),
		newFileRequestCmd(e), newFileRequestsCmd(e))
	return cmd
}

// newFileRequestCmd queues a file for the next sneakernet exchange.
//
// The TUI is where a user does this — they are looking at the listing that
// says which BBS holds it — so this exists for the sysop, who is the one
// carrying the stick and is entitled to put something on it without logging in
// to their own board over SSH.
func newFileRequestCmd(e *env) *cobra.Command {
	var nick string
	cmd := &cobra.Command{
		Use:   "request <area> <name>",
		Short: "Queue a file held by another BBS for the next exchange (§6.5)",
		Long: `Ask for a file this BBS does not hold.

The mesh never carries file bytes, at any size, so a file announced by another
board is a listing here and nothing more. This records a request. It rides every
sneakernet carrier written from now on as a content hash, and when a board that
holds the bytes answers one, the file is filed in this area under this name and
whoever asked is told the next time they log in.

What it does not do is promise a date. A hand-off happens when somebody walks,
and nothing here knows when that is.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			area, name := args[0], args[1]
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				entries, err := st.ListAreaContents(ctx, area)
				if err != nil {
					return err
				}
				var want store.CatalogEntry
				var found bool
				for _, entry := range entries {
					if entry.Name != name {
						continue
					}
					// A name can appear twice — two boards, two files, one
					// name — and the one worth requesting is the one this node
					// does not have.
					if !entry.Held {
						want, found = entry, true
						break
					}
					want, found = entry, true
				}
				if !found {
					return fmt.Errorf("%s holds no listing for %s", area, name)
				}

				req, err := st.RequestFile(ctx, area, want.Name, want.Hash, want.Origin, nick)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"Queued %s/%s for %s.\nIt rides the next carrier: "+
						"meshbbs sneakernet export away.mbx\n",
					req.Area, req.Name, req.Nick)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&nick, "for", "sysop",
		"the account to notify when it lands")
	return cmd
}

// newFileRequestsCmd shows the queue.
func newFileRequestsCmd(e *env) *cobra.Command {
	var nick string
	cmd := &cobra.Command{
		Use:   "requests",
		Short: "Show what is queued for the next sneakernet exchange",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				reqs, err := st.ListFileRequests(ctx, nick)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				if len(reqs) == 0 {
					fmt.Fprintln(out, "Nothing is on request.")
					return nil
				}

				w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "AREA\tNAME\tFOR\tHASH\tSTATE")
				for _, r := range reqs {
					state := "waiting"
					switch {
					case r.Filed():
						state = "arrived"
					case r.Arrived():
						state = r.Note
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%x…\t%s\n",
						r.Area, r.Name, r.Nick, r.Hash[:6], state)
				}
				if err := w.Flush(); err != nil {
					return err
				}
				fmt.Fprintf(out, "\nWaiting requests ride every carrier "+
					"`meshbbs sneakernet export` writes.\n")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&nick, "for", "", "show only this account's requests")
	return cmd
}

func newFileListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list <area>",
		Short: "List the files an area holds",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				files, err := st.ListFiles(ctx, args[0])
				if err != nil {
					return err
				}
				if len(files) == 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "%s holds no files yet.\n", args[0])
					return nil
				}

				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "NAME\tSIZE\tFROM\tDESCRIPTION")
				for _, f := range files {
					fmt.Fprintf(w, "%s\t%d\t%s\t%s\n",
						f.Name, f.Size, f.Uploader, f.Description)
				}
				return w.Flush()
			})
		},
	}
}

func newFileDescribeCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "describe <area> <name> <text>",
		Short: "Set the description shown beside a file",
		Long: fmt.Sprintf(`Set the description shown beside a file in the catalog.

An SFTP client has no way to send one, so a description is set here or in the
TUI (press "d" on a file), never at upload time.

Pass an empty string to clear it:

    meshbbs file describe uploads notes.txt ""

The limit is %d bytes, which is what a FILE record can carry (§6.5). It is
short on purpose: at roughly ten originated packets per node per day, a
description that doubles the size of a catalog entry is one fewer file this
node can announce at all.

In a federated area this announces the change: the description travels inside
the FILE record, so a new one is written and replicated. Peers keep what they
already hold until it reaches them — records are signed and replicated, and
there is no unsend on a broadcast medium.`, record.MaxFileDescLen),
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			area, name, text := args[0], args[1], args[2]
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				// No MayDescribe check. Running this binary means shell access
				// to the machine holding the database and the node key, which
				// is strictly more authority than any capability grants — a
				// permission check here would be theatre, and the audit log
				// already records who ran it.
				//
				// Through the service so a federated area gets a new FILE
				// record: the description travels in one, so editing it here
				// and stopping would leave every peer holding the old text.
				key, err := e.nodeKey()
				if err != nil {
					return err
				}
				svc := bbs.New(st, key, e.clock)
				if err := svc.DescribeFile(ctx, area, name, text, "cli"); err != nil {
					return err
				}

				out := cmd.OutOrStdout()
				f, err := st.GetFile(ctx, area, name)
				if err != nil {
					return err
				}
				if f.Description == "" {
					fmt.Fprintf(out, "Cleared the description of %s in %s.\n", f.Name, area)
					return nil
				}
				fmt.Fprintf(out, "%s in %s is now described as:\n  %s\n", f.Name, area, f.Description)
				return nil
			})
		},
	}
}
