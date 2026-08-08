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
	cmd.AddCommand(newFileListCmd(e), newFileDescribeCmd(e))
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
