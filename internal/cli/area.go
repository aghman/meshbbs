package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// newAreaCmd builds the `area` group.
//
// Federating an area is the decision that puts a conversation on other people's
// radios, and until now there was no supported way to make it — the flag
// existed in the schema and in the sync engine with nothing to set it. That is
// how a sysop ends up editing the database by hand, which is exactly what a
// tool should not require.
func newAreaCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "area",
		Short: "Inspect and federate message areas",
	}
	cmd.AddCommand(newAreaListCmd(e), newAreaFederateCmd(e))
	return cmd
}

func newAreaListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List message areas and whether they federate",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			areas, err := st.ListAreas(ctx)
			if err != nil {
				return err
			}
			if len(areas) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No areas yet. They are created when the BBS first runs.")
				return nil
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "AREA\tTAG\tFEDERATED\tDESCRIPTION")
			for _, a := range areas {
				fed := "local only"
				if a.Federated {
					fed = "yes"
				}
				// The tag is what appears on the wire and in a digest, so it is
				// worth showing: it is the only way to match a log line about
				// area 79D42C56 to the area a sysop knows by name.
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.Name, a.Tag, fed, a.Description)
			}
			return w.Flush()
		},
	}
}

func newAreaFederateCmd(e *env) *cobra.Command {
	var off bool

	cmd := &cobra.Command{
		Use:   "federate <area>",
		Short: "Put an area on the mesh, or take it off",
		Long: `Put an area on the mesh, or take it off.

A federated area's posts are broadcast to every peer instance and cost shared
airtime. Areas are local-only by default, because whether a conversation
belongs on other people's radios is the sysop's call, not ours.

Taking an area off the mesh stops this node sending its posts. It does not
withdraw what peers already hold: records are signed and replicated, and there
is no unsend on a broadcast medium.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			name := args[0]
			if _, err := st.GetArea(ctx, name); err != nil {
				return fmt.Errorf("no area named %q: %w", name, err)
			}
			if err := st.SetAreaFederated(ctx, name, !off, "cli"); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if off {
				fmt.Fprintf(out, "%s is now local only.\n", name)
				fmt.Fprintf(out, "Peers keep the posts they already have — a broadcast cannot be unsent.\n")
				return nil
			}
			fmt.Fprintf(out, "%s now federates.\n", name)
			if !e.cfg.Mesh.Enabled {
				fmt.Fprintf(out, "Note: mesh.enabled is false, so nothing is transmitted yet.\n")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&off, "off", false, "take the area off the mesh instead")
	return cmd
}
