package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newIDCmd(e *env) *cobra.Command {
	var short, words, compact bool

	cmd := &cobra.Command{
		Use:   "id",
		Short: "Print this node's ID",
		Long: `Print this node's ID in both renderings.

The ID is derived from the node key — it is not configurable and not
registered anywhere. Both forms below encode the same 64 bits:

  base32   for typing, config files and allowlists
  words    for reading aloud, over voice radio or the phone

Show the full base32 form (not the abbreviated one) whenever someone is
making a security decision, such as adding this node to their allowlist.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := e.nodeKey()
			if err != nil {
				return err
			}
			id := key.ID()

			switch {
			case short:
				fmt.Fprintln(cmd.OutOrStdout(), id.Short())
			case compact:
				fmt.Fprintln(cmd.OutOrStdout(), id.Compact())
			case words:
				fmt.Fprintln(cmd.OutOrStdout(), id.Words())
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "Node ID\n")
				fmt.Fprintf(cmd.OutOrStdout(), "  base32  %s\n", id.String())
				fmt.Fprintf(cmd.OutOrStdout(), "  words   %s\n", id.Words())
				fmt.Fprintf(cmd.OutOrStdout(), "  short   %s\n", id.Short())
				if name := e.cfg.Node.DisplayName; name != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "\nDisplay name: %s\n", name)
					fmt.Fprintf(cmd.OutOrStdout(),
						"(self-declared and not authoritative — peers choose their own local alias)\n")
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&short, "short", false, "print the 8-character abbreviated form")
	cmd.Flags().BoolVar(&compact, "compact", false, "print the ungrouped 13-character base32 form")
	cmd.Flags().BoolVar(&words, "words", false, "print only the six-word form")
	cmd.MarkFlagsMutuallyExclusive("short", "compact", "words")
	return cmd
}
