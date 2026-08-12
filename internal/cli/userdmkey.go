package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/aghman/meshbbs/internal/keyring"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// newUserDMKeyCmd adopts a public key whose private half the user keeps
// (§8.2 tier 3).
//
// # Why the sysop runs this rather than the user
//
// It is the one step of tier 3 that changes the server, and it is a claim about
// who somebody is: "mail for this nick should be sealed to this key". A user
// who could set it themselves over SSH could set it for the account they are
// logged into — which is fine — but the interesting case is an account whose
// password has leaked, where an attacker setting their own key would redirect
// every future message and lock the owner out of their own mail with no
// notification anywhere.
//
// Requiring a sysop is not a strong control, and it is not pretending to be
// one. It is a speed bump on the one operation whose failure is silent, in a
// system whose trust model is already sysop-to-sysop (§9.5, `[D5]`).
func newUserDMKeyCmd(e *env) *cobra.Command {
	var replace bool

	cmd := &cobra.Command{
		Use:   "dm-key <nick> <public-key>",
		Short: "Adopt a user's own DM public key (tier 3, §8.2)",
		Long: `Record a DM public key whose private half the user holds.

The user generates the pair on their own machine:

    meshbbs-key init

and hands you the public half it prints. From then on this BBS seals their mail
to that key and cannot open it — which is the point. They read it with:

    meshbbs-key open

This clears any wrapped private key the server was holding for them. If the
account already has a DIFFERENT key, messages already delivered are sealed to
the old one and nobody can re-seal them — the server never had the private half.
The command refuses unless --replace is given.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			pub, err := keyring.ParsePublicKey(args[1])
			if err != nil {
				return fmt.Errorf("%q is not a DM public key: %w\n\n"+
					"It should be the line \"meshbbs-key init\" printed, "+
					"not the contents of the key file", args[1], err)
			}
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				err := st.SetClientHeldDMKey(ctx, args[0], pub, replace)
				if errors.Is(err, store.ErrWouldStrandExistingMail) {
					return fmt.Errorf("%w\n\nRe-run with --replace to proceed anyway", err)
				}
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "%s now holds their own DM key.\n", args[0])
				fmt.Fprintf(out, "This BBS seals their mail to %s and cannot read it.\n", pub)
				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&replace, "replace", false,
		"replace an existing key, making mail already sealed to it unreadable")
	return cmd
}
