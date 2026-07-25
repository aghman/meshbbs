package cli

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

func newPeerCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peer",
		Short: "Manage the node roster and local aliases",
		Long: `Manage known nodes and the local alias table.

Aliases are this instance's private naming for other nodes, exactly like Host
entries in an SSH config. They are owned by the sysop, resolved locally when a
message is composed, and never travel on the wire — so two BBSes may disagree
about what "pnw" means and neither is wrong. That is why no registry exists.`,
	}
	cmd.AddCommand(newPeerListCmd(e), newPeerAliasCmd(e), newPeerUnaliasCmd(e), newPeerResolveCmd(e))
	return cmd
}

func newPeerListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List known nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				nodes, err := st.ListNodes(ctx)
				if err != nil {
					return err
				}
				aliases, err := st.ListAliases(ctx)
				if err != nil {
					return err
				}
				byNode := map[identity.NodeID][]string{}
				for _, a := range aliases {
					byNode[a.NodeID] = append(byNode[a.NodeID], a.Alias)
				}

				if len(nodes) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No known nodes.")
					return nil
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "NODE ID\tALIASES\tDISPLAY NAME\tSELF")
				for _, n := range nodes {
					names := "-"
					if a := byNode[n.ID]; len(a) > 0 {
						names = joinComma(a)
					}
					display := n.DisplayName
					if display == "" {
						display = "-"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.ID.String(), names, display, yn(n.IsSelf))
				}
				if err := w.Flush(); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nDisplay names are self-declared and not authoritative — always check the ID.\n")
				return nil
			})
		},
	}
}

func newPeerAliasCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "alias <name> <node-id>",
		Short: "Bind a local alias to a node ID",
		Long: `Bind a local alias to a node ID.

The node ID may be given in either rendering — 13 base32 characters (grouped
or not) or six words. Both encode the same 64 bits.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias, ref := args[0], args[1]
			id, err := identity.ParseNodeID(ref)
			if err != nil {
				return fmt.Errorf("second argument must be a node ID: %w", err)
			}
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				// Warn rather than refuse when retargeting: the sysop may
				// legitimately be following a node's succession, but silently
				// redirecting where mail goes would be worse than noisy.
				if existing, err := st.ResolveAlias(ctx, alias); err == nil && existing != id {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"warning: alias %q currently points at %s; retargeting it to %s\n",
						alias, existing.String(), id.String())
				}
				if err := st.SetAlias(ctx, alias, id); err != nil {
					return err
				}
				if err := st.Audit(ctx, "cli", "alias.set", alias, id.Compact()); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s\n", alias, id.String())
				return nil
			})
		},
	}
}

func newPeerUnaliasCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "unalias <name>",
		Short: "Remove a local alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				if err := st.RemoveAlias(ctx, args[0]); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("no alias named %q", args[0])
					}
					return err
				}
				if err := st.Audit(ctx, "cli", "alias.remove", args[0], ""); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed alias %s\n", args[0])
				return nil
			})
		},
	}
}

func newPeerResolveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <alias-or-id>",
		Short: "Resolve an alias or node ID and show both renderings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				id, err := st.ResolveNodeRef(ctx, args[0])
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "base32  %s\n", id.String())
				fmt.Fprintf(out, "words   %s\n", id.Words())
				return nil
			})
		},
	}
}

func joinComma(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
