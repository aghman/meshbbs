package cli

import (
	"fmt"
	"sort"
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
		Short: "Inspect and federate message and file areas",
	}
	cmd.AddCommand(newAreaListCmd(e), newAreaCreateCmd(e), newAreaFederateCmd(e))
	return cmd
}

func newAreaListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List areas and whether they federate",
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
			// Both kinds share one tag namespace (migration 0005), so they
			// belong in one listing: a sysop deciding on a name needs to see
			// every name already spent, not half of them.
			fileAreas, err := st.ListFileAreas(ctx)
			if err != nil {
				return err
			}
			areas = append(areas, fileAreas...)
			if len(areas) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No areas yet. They are created when the BBS first runs.")
				return nil
			}
			sort.Slice(areas, func(i, j int) bool { return areas[i].Name < areas[j].Name })

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "AREA\tKIND\tTAG\tFEDERATED\tDESCRIPTION")
			for _, a := range areas {
				fed := "local only"
				if a.Federated {
					fed = "yes"
				}
				// The tag is what appears on the wire and in a digest, so it is
				// worth showing: it is the only way to match a log line about
				// area 79D42C56 to the area a sysop knows by name.
				// Hex, not the raw bytes: a tag is four arbitrary bytes of a
				// hash, and printing them as text produces mojibake in the one
				// column whose whole purpose is matching a log line to a name.
				fmt.Fprintf(w, "%s\t%s\t%x\t%s\t%s\n", a.Name, a.Kind, a.Tag[:], fed, a.Description)
			}
			return w.Flush()
		},
	}
}

func newAreaCreateCmd(e *env) *cobra.Command {
	var description string
	var federated bool
	var files bool

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a message area, or a file area with --files",
		Long: `Create an area.

The area's wire tag is derived from its name, so two instances that create the
same name get the same tag and federate into the same conversation. That is the
whole coordination mechanism: there is no registry of area names and no
authority to ask, exactly as there is none for node IDs (§6.1.1).

It also means a typo makes a different area, silently. Check the tag against
your peers if a federated area does not seem to be reaching them.

With --files the area holds files rather than messages (§6.5), and appears as a
directory over SFTP. A name can only be spent once across both kinds, because
both derive the same kind of tag from it.

A federated FILE area replicates the catalog only — names, sizes and hashes.
File contents never travel over the mesh, at any size, and that is a property of
the link rather than a setting (§7.5).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			create := st.CreateArea
			kind := "message area"
			if files {
				create = st.CreateFileArea
				kind = "file area"
			}
			area, err := create(ctx, args[0], description, federated)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created %s, a %s (tag %x)\n", area.Name, kind, area.Tag[:])
			if federated {
				fmt.Fprintf(out, "It federates. Peers need an area with the same name to receive it.\n")
			} else {
				fmt.Fprintf(out, "It is local only. Run \"meshbbs area federate %s\" to put it on the mesh.\n", area.Name)
			}
			if files {
				fmt.Fprintf(out, "Users with the upload_files capability can put files in it over SFTP.\n")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "what the area is for")
	cmd.Flags().BoolVar(&federated, "federated", false, "put it on the mesh immediately")
	cmd.Flags().BoolVar(&files, "files", false, "make a file area rather than a message area")
	return cmd
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

			// Either kind: a file area's catalog federates too (§6.5), and it
			// is the same flag on the same row that decides.
			name := args[0]
			if _, err := st.GetAnyArea(ctx, name); err != nil {
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
