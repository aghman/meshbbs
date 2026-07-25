package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aghman/meshbbs/internal/auth"
	"github.com/aghman/meshbbs/internal/config"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

func newInitCmd(e *env) *cobra.Command {
	var displayName, sysopName, sysopNick, sysopContact string
	var development, sysopPasswordStdin bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a new instance: generate the node key, database and config",
		Long: `Initialise a new BBS instance.

Generates the node identity key, creates the database, writes a minimal
config file, and records this node's own NODE record.

There is no address to choose, request or register: the node ID falls out of
the key that is generated here. That also means the key is irreplaceable —
back up the keys directory, because a lost node key cannot be recovered and
the instance would have to re-establish with its peers as a new node.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			out := cmd.OutOrStdout()

			cfg := e.cfg
			if displayName != "" {
				cfg.Node.DisplayName = displayName
			}
			if sysopName != "" {
				cfg.Node.SysopName = sysopName
			}
			if sysopContact != "" {
				cfg.Node.SysopContact = sysopContact
			}
			if development {
				cfg.Node.Environment = config.EnvDevelopment
			}
			if err := cfg.Validate(); err != nil {
				return err
			}

			// Validate EVERY input before writing anything. init is not
			// re-runnable — SaveNodeKey refuses to overwrite an existing key —
			// so a late failure would strand the operator with a half-built
			// instance they cannot init again and probably should not delete
			// by hand.
			var sysopHash string
			if sysopNick != "" {
				if err := store.ValidateNick(sysopNick); err != nil {
					return fmt.Errorf("--sysop-nick: %w", err)
				}
				if sysopPasswordStdin {
					pw, err := readPassword(cmd.InOrStdin())
					if err != nil {
						return err
					}
					if pw == "" {
						return fmt.Errorf("--sysop-password-stdin was given but stdin was empty")
					}
					sysopHash, err = auth.HashPassword(pw)
					if err != nil {
						return err
					}
				}
			}

			if err := os.MkdirAll(e.dataDir, 0o700); err != nil {
				return fmt.Errorf("create data directory: %w", err)
			}

			keysDir, err := cfg.KeysPath()
			if err != nil {
				return err
			}

			// Generating the key first means a re-run against an initialised
			// datadir stops here, rather than after writing a config that
			// disagrees with the existing identity.
			key, err := identity.GenerateNodeKey(rng.NewSecret())
			if err != nil {
				return err
			}
			if err := identity.SaveNodeKey(keysDir, key); err != nil {
				return err
			}
			fmt.Fprintf(out, "Generated node key in %s\n", keysDir)

			cfgPath := config.DefaultConfigPath(e.dataDir)
			if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
				if err := writeMinimalConfig(cfgPath, cfg); err != nil {
					return fmt.Errorf("write config: %w", err)
				}
				fmt.Fprintf(out, "Wrote %s\n", cfgPath)
			} else {
				fmt.Fprintf(out, "Kept existing %s\n", cfgPath)
			}

			dbPath, err := cfg.DatabasePath()
			if err != nil {
				return err
			}
			st, err := store.Open(ctx, dbPath, e.clock)
			if err != nil {
				return err
			}
			defer st.Close()
			fmt.Fprintf(out, "Created database %s\n", dbPath)

			// Publish our own NODE record so the roster has an entry for us
			// from the very first boot, and so seq 1 is spent on something
			// meaningful.
			seq, err := st.NextSeq(ctx)
			if err != nil {
				return err
			}
			nodeRec, err := record.NewNodeRecord(key, seq, uint32(e.clock.Now().Unix()),
				cfg.Node.DisplayName, cfg.Node.SysopContact, 0)
			if err != nil {
				return err
			}
			if err := st.PutRecord(ctx, nodeRec); err != nil {
				return err
			}
			if err := st.PutNode(ctx, store.Node{
				ID:           key.ID(),
				PublicKey:    key.Public,
				DisplayName:  cfg.Node.DisplayName,
				SysopContact: cfg.Node.SysopContact,
				IsSelf:       true,
			}); err != nil {
				return err
			}

			if sysopNick != "" {
				if _, err := st.CreateUser(ctx, store.CreateUserOptions{
					Nick:         sysopNick,
					DisplayName:  cfg.Node.SysopName,
					PasswordHash: sysopHash,
					IsSysop:      true,
					CanLogin:     true,
					// The sysop gets federated posting; everyone else must be
					// granted it explicitly ([N7]).
					Capabilities: append(append([]string(nil), store.DefaultCapabilities...),
						store.CapPostFederated, store.CapSendDMOffnode),
				}); err != nil {
					return fmt.Errorf("create sysop account: %w", err)
				}
				fmt.Fprintf(out, "Created sysop account %q\n", sysopNick)
				if sysopHash == "" {
					// An account with neither a password nor an enrolled key
					// cannot log in at all, which is a confusing state to hand
					// a new sysop. Say so, with the fix.
					fmt.Fprintf(out, "\n  WARNING: %s has no password and no SSH key, so it cannot log in yet.\n", sysopNick)
					fmt.Fprintf(out, "  Set one with:  meshbbs user passwd %s\n", sysopNick)
					fmt.Fprintf(out, "  Or register over SSH and grant sysop:  ssh new@localhost\n")
				}
			}

			id := key.ID()
			fmt.Fprintf(out, "\nThis node's ID:\n")
			fmt.Fprintf(out, "  base32  %s\n", id.String())
			fmt.Fprintf(out, "  words   %s\n", id.Words())
			fmt.Fprintf(out, "\nBack up %s — a lost node key cannot be recovered.\n",
				filepath.Join(keysDir, identity.NodeKeyFile))
			return nil
		},
	}

	cmd.Flags().StringVar(&displayName, "display-name", "", "self-declared node label published in the NODE record")
	cmd.Flags().StringVar(&sysopName, "sysop-name", "", "sysop's name, for display")
	cmd.Flags().StringVar(&sysopContact, "sysop-contact", "", "sysop contact address published in the NODE record")
	cmd.Flags().StringVar(&sysopNick, "sysop-nick", "", "create a sysop account with this nick")
	cmd.Flags().BoolVar(&sysopPasswordStdin, "sysop-password-stdin", false,
		"read the sysop's password from stdin (without this the account cannot log in until one is set)")
	cmd.Flags().BoolVar(&development, "development", false,
		"mark this as a development instance, enabling the dev subcommands")
	return cmd
}
