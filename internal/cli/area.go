package cli

import (
	"fmt"
	"sort"
	"strconv"
	"text/tabwriter"

	"github.com/aghman/meshbbs/internal/store"
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
	cmd.AddCommand(newAreaListCmd(e), newAreaCreateCmd(e), newAreaFederateCmd(e),
		newAreaShareCmd(e))
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
			// All three kinds share one tag namespace (migrations 0005 and
			// 0007), so they belong in one listing: a sysop deciding on a name
			// needs to see every name already spent, not some of them.
			fileAreas, err := st.ListFileAreas(ctx)
			if err != nil {
				return err
			}
			doorAreas, err := st.ListDoorAreas(ctx)
			if err != nil {
				return err
			}
			areas = append(areas, fileAreas...)
			areas = append(areas, doorAreas...)
			if len(areas) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No areas yet. They are created when the BBS first runs.")
				return nil
			}
			sort.Slice(areas, func(i, j int) bool { return areas[i].Name < areas[j].Name })

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "AREA\tKIND\tTAG\tFEDERATED\tSHARE\tDESCRIPTION")
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
				share := "-"
				if a.AirtimeShare > 0 {
					share = fmt.Sprintf("%.0f%%", a.AirtimeShare*100)
				}
				fmt.Fprintf(w, "%s\t%s\t%x\t%s\t%s\t%s\n",
					a.Name, a.Kind, a.Tag[:], fed, share, a.Description)
			}
			return w.Flush()
		},
	}
}

func newAreaCreateCmd(e *env) *cobra.Command {
	var description string
	var federated bool
	var files bool
	var kind string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a message area, a file area, or a door league",
		Long: `Create an area.

The area's wire tag is derived from its name, so two instances that create the
same name get the same tag and federate into the same conversation. That is the
whole coordination mechanism: there is no registry of area names and no
authority to ask, exactly as there is none for node IDs (§6.1.1).

It also means a typo makes a different area, silently. Check the tag against
your peers if a federated area does not seem to be reaching them.

--kind chooses what the area holds. A name can only be spent once across all
three, because they all derive the same kind of tag from it.

  message  a forum message base (§6.3). The default.
  file     files, browsable over SFTP (§6.5).
  door     an inter-BBS door league (§9.5), carrying game events between
           boards. It accepts DOOR_EVENT records and nothing else, and a
           message area will not accept those.

A federated FILE area replicates the catalog only — names, sizes and hashes.
File contents never travel over the mesh, at any size, and that is a property of
the link rather than a setting (§7.5).

A federated DOOR league spends shared airtime on game traffic, which sits at the
bottom of the priority order (§1.1). Federate one because a league exists to
play in, not to see what happens.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			if files {
				kind = string(store.KindFile)
			}
			create := st.CreateArea
			switch store.AreaKind(kind) {
			case store.KindMessage:
			case store.KindFile:
				create = st.CreateFileArea
			case store.KindDoor:
				create = st.CreateDoorArea
			default:
				return fmt.Errorf("unknown area kind %q; use message, file or door", kind)
			}
			area, err := create(ctx, args[0], description, federated)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Created %s, %s (tag %x)\n", area.Name, area.Kind.Describe(), area.Tag[:])
			if federated {
				fmt.Fprintf(out, "It federates. Peers need an area with the same name to receive it.\n")
			} else {
				fmt.Fprintf(out, "It is local only. Run \"meshbbs area federate %s\" to put it on the mesh.\n", area.Name)
			}
			switch area.Kind {
			case store.KindFile:
				fmt.Fprintf(out, "Users with the upload_files capability can put files in it over SFTP.\n")
			case store.KindDoor:
				fmt.Fprintf(out, "It carries door events only; posting into it is refused, here and from peers.\n")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "what the area is for")
	cmd.Flags().BoolVar(&federated, "federated", false, "put it on the mesh immediately")
	cmd.Flags().StringVar(&kind, "kind", string(store.KindMessage), "what the area holds: message, file or door")
	// --files predates --kind, which arrived when a third kind made a boolean
	// per kind the wrong shape. Kept working and hidden rather than removed:
	// breaking a flag to tidy a help page is a bad trade.
	cmd.Flags().BoolVar(&files, "files", false, "deprecated: use --kind file")
	_ = cmd.Flags().MarkHidden("files")
	cmd.MarkFlagsMutuallyExclusive("kind", "files")
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

func newAreaShareCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "share <area> <fraction>",
		Short: "Cap what one area may spend of this node's airtime budget",
		Long: `Cap an area's airtime, as a fraction of this instance's whole budget (§6.3).

    meshbbs area share lordleague 0.10    # at most a tenth of the budget
    meshbbs area share lordleague 0       # no cap

Without a cap an area is bounded only by its priority class, and a class
reserve is not a rate: it stops traffic spending the LAST of the bucket, and
does nothing to stop a busy area draining the first 70% of it in an hour. A
share is a rate, which is what a chatty area actually needs.

The shares of federated areas must add up to no more than 1. Local-only areas
do not count, because they spend no mesh airtime.

The figure to check afterwards is not the fraction — it is what the fraction
buys, which "meshbbs mesh status" prints in packets per day. At fifty instances
a tenth of a share is about one packet a day, and that is the number that
decides whether a league is playable.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			share, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("%q is not a fraction between 0 and 1", args[1])
			}
			ctx := cmd.Context()
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			if err := st.SetAreaShare(ctx, args[0], share, "cli"); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if share == 0 {
				fmt.Fprintf(out, "%s is no longer capped; it draws from the general pool.\n", args[0])
				return nil
			}
			fmt.Fprintf(out, "%s may use up to %.0f%% of this node's airtime budget.\n",
				args[0], share*100)
			area, err := st.GetAnyArea(ctx, args[0])
			if err == nil && !area.Federated {
				fmt.Fprintf(out, "It is local only, so the cap has nothing to limit yet.\n")
			}
			return nil
		},
	}
}
