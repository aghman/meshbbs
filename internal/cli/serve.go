package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/logging"
	"github.com/aghman/meshbbs/internal/sshd"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/theme"
	"github.com/spf13/cobra"
)

func newServeCmd(e *env) *cobra.Command {
	var port int

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the BBS",
		Long: `Run the BBS.

Serves SSH on the configured port. Users connect with:

    ssh new@your-host -p 2222      register a new account
    ssh yournick@your-host -p 2222 log in
    ssh guest@your-host -p 2222    browse read-only, if guests are enabled`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()

			log, closer, err := logging.New(e.cfg.Log)
			if err != nil {
				return err
			}
			if closer != nil {
				defer closer.Close()
			}

			key, err := e.nodeKey()
			if err != nil {
				return err
			}
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			// §6.2.1 rule 3: check for a sequence regression at startup, before
			// anything can issue a new record. A restore from backup that
			// silently reused sequence numbers is unrecoverable once peers have
			// accepted the reissued ones.
			self := key.ID()
			repaired, err := st.CheckSeqIntegrity(ctx, self[:])
			if err != nil {
				return err
			}
			if repaired {
				log.Warn("sequence high-water mark was behind the stored log and has been repaired; " +
					"incarnation counter incremented. This usually means a restore from backup.")
			}

			svc := bbs.New(st, key, e.clock)
			if err := svc.SeedDefaultAreas(ctx); err != nil {
				return err
			}

			themeDir, err := e.cfg.ThemePath()
			if err != nil {
				return err
			}
			themes, err := theme.Load(themeDir)
			if err != nil {
				return err
			}
			if !themes.Has(e.cfg.Theme.Default) {
				return fmt.Errorf("theme.default is %q, which is not among the available themes: %v",
					e.cfg.Theme.Default, themes.Names())
			}

			loc, err := e.cfg.Location()
			if err != nil {
				return err
			}

			keysDir, err := e.keysDir()
			if err != nil {
				return err
			}
			filesDir, err := e.cfg.FilesPath()
			if err != nil {
				return err
			}
			if port == 0 {
				port = e.cfg.SSH.Port
			}

			srv, err := sshd.NewServer(svc, st, sshd.Options{
				Bind:         e.cfg.SSH.Bind,
				Port:         port,
				KeysDir:      keysDir,
				FilesDir:     filesDir,
				GuestEnabled: e.cfg.Users.GuestEnabled,
				OpenSignup:   e.cfg.Users.RegistrationMode == "open",
				Themes:       themes,
				DefaultTheme: e.cfg.Theme.Default,
				Clock:        e.clock,
				Location:     loc,
				Logger:       log,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s is up.\n\n", e.cfg.Node.DisplayName)
			fmt.Fprintf(out, "  node id   %s\n", key.ID().String())
			fmt.Fprintf(out, "  ssh       %s:%d\n", e.cfg.SSH.Bind, port)
			fmt.Fprintf(out, "  themes    %v\n", themes.Names())
			fmt.Fprintf(out, "  guests    %v\n", e.cfg.Users.GuestEnabled)
			fmt.Fprintf(out, "  register  %s\n", e.cfg.Users.RegistrationMode)
			fmt.Fprintf(out, "  files     %s (sftp)\n\n", filesDir)
			fmt.Fprintf(out, "Connect with:  ssh new@localhost -p %d\n", port)
			fmt.Fprintf(out, "Ctrl+C to stop.\n\n")

			// Telnet, when enabled, runs alongside SSH. [D12] made it
			// off-by-default with a loud warning; the warning is emitted by the
			// telnet server itself at every start.
			if e.cfg.Telnet.Enabled {
				tel := sshd.NewTelnetServer(svc, st, sshd.TelnetOptions{
					Bind:      e.cfg.Telnet.Bind,
					Port:      e.cfg.Telnet.Port,
					GuestOnly: e.cfg.Telnet.GuestOnly,
					Themes:    themes,
					Theme:     e.cfg.Theme.Default,
					Chat:      srv.Chat(),
					Presence:  srv.Presence(),
					Location:  loc,
					Logger:    log,
				})
				fmt.Fprintf(out, "  telnet    %s:%d (PLAINTEXT, guest-only)\n\n",
					e.cfg.Telnet.Bind, e.cfg.Telnet.Port)
				go func() {
					if err := tel.ListenAndServe(ctx); err != nil {
						log.Error("telnet server stopped", "err", err)
					}
				}()
			}

			if err := srv.ListenAndServe(ctx); err != nil {
				return err
			}
			log.Info("shut down cleanly")
			return nil
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "override the configured SSH port")
	return cmd
}

var _ = store.ErrNotFound
