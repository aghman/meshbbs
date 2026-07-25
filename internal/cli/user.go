package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

func newUserCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage accounts",
		Long: `Manage user accounts.

These commands work with the server stopped, which is what makes them the
recovery path when nobody can log in.

Note that new accounts do NOT get the post_federated capability: anyone may
register and use the BBS immediately, but spending the network's shared
airtime is a grant the sysop makes deliberately.`,
	}
	cmd.AddCommand(
		newUserAddCmd(e), newUserListCmd(e),
		newUserGrantCmd(e), newUserRevokeCmd(e), newUserShowCmd(e),
	)
	return cmd
}

func withStore(e *env, cmd *cobra.Command, fn func(context.Context, *store.Store) error) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	st, err := e.openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(ctx, st)
}

func newUserAddCmd(e *env) *cobra.Command {
	var displayName string
	var sysop, noLogin, passwordStdin bool
	var capabilities []string

	cmd := &cobra.Command{
		Use:   "add <nick>",
		Short: "Create an account",
		Long: `Create an account non-interactively.

This is the only account-creation path that exists before the SSH server does,
which makes it the way to populate an instance for testing.

Passwords are read from stdin with --password-stdin so they never appear in
the process list or shell history.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			nick := args[0]

			var hash string
			if passwordStdin {
				pw, err := readPassword(cmd.InOrStdin())
				if err != nil {
					return err
				}
				if pw == "" {
					return fmt.Errorf("--password-stdin was given but stdin was empty")
				}
				hash, err = auth.HashPassword(pw)
				if err != nil {
					return err
				}
			}

			caps := store.DefaultCapabilities
			if len(capabilities) > 0 {
				caps = capabilities
			}
			if sysop {
				caps = append(append([]string(nil), caps...),
					store.CapPostFederated, store.CapSendDMOffnode)
			}

			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				u, err := st.CreateUser(ctx, store.CreateUserOptions{
					Nick:         nick,
					DisplayName:  displayName,
					PasswordHash: hash,
					IsSysop:      sysop,
					CanLogin:     !noLogin,
					Capabilities: caps,
				})
				if err != nil {
					return err
				}
				if err := st.Audit(ctx, "cli", "user.add", u.Nick, ""); err != nil {
					return err
				}

				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Created %s\n", u.Nick)
				got, err := st.Capabilities(ctx, u.Nick)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "  capabilities: %s\n", strings.Join(got, ", "))
				if !contains(got, store.CapPostFederated) {
					fmt.Fprintf(out, "  note: no %s — this account can post locally but not to\n"+
						"        federated areas. Grant it with:\n"+
						"          meshbbs user grant %s %s\n",
						store.CapPostFederated, u.Nick, store.CapPostFederated)
				}
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&displayName, "display-name", "", "display name")
	cmd.Flags().BoolVar(&sysop, "sysop", false, "grant sysop status")
	cmd.Flags().BoolVar(&noLogin, "no-login", false, "create an account that cannot log in (a DM target)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from stdin")
	cmd.Flags().StringSliceVar(&capabilities, "capability", nil,
		"capability to grant (repeatable); defaults to "+strings.Join(store.DefaultCapabilities, ","))
	return cmd
}

func newUserListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List accounts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				users, err := st.ListUsers(ctx)
				if err != nil {
					return err
				}
				if len(users) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No accounts yet.")
					return nil
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "NICK\tSTATE\tSYSOP\tLISTED\tFEDERATED\tCAPABILITIES")
				for _, u := range users {
					caps, err := st.Capabilities(ctx, u.Nick)
					if err != nil {
						return err
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
						u.Nick, u.State, yn(u.IsSysop), yn(u.DirectoryListed),
						yn(contains(caps, store.CapPostFederated)), strings.Join(caps, ","))
				}
				return w.Flush()
			})
		},
	}
}

func newUserShowCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "show <nick>",
		Short: "Show one account in detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				u, err := st.GetUser(ctx, args[0])
				if err != nil {
					return err
				}
				caps, err := st.Capabilities(ctx, u.Nick)
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "nick:          %s\n", u.Nick)
				fmt.Fprintf(out, "display name:  %s\n", u.DisplayName)
				fmt.Fprintf(out, "state:         %s\n", u.State)
				fmt.Fprintf(out, "sysop:         %s\n", yn(u.IsSysop))
				fmt.Fprintf(out, "can log in:    %s\n", yn(u.CanLogin))
				fmt.Fprintf(out, "listed:        %s\n", yn(u.DirectoryListed))
				fmt.Fprintf(out, "password set:  %s\n", yn(u.PasswordHash != ""))
				fmt.Fprintf(out, "capabilities:  %s\n", strings.Join(caps, ", "))
				return nil
			})
		},
	}
}

func newUserGrantCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "grant <nick> <capability>",
		Short: "Grant a capability",
		Long: "Grant a capability.\n\nKnown capabilities: " +
			strings.Join(store.KnownCapabilities, ", "),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				if err := st.GrantCapability(ctx, args[0], args[1], "cli"); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Granted %s to %s\n", args[1], args[0])
				return nil
			})
		},
	}
}

func newUserRevokeCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <nick> <capability>",
		Short: "Revoke a capability",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				if err := st.RevokeCapability(ctx, args[0], args[1], "cli"); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s from %s\n", args[1], args[0])
				return nil
			})
		},
	}
}

func readPassword(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("read password from stdin: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

var _ = os.Stdin
