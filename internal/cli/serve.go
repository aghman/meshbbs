package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aghman/meshbbs/internal/bbs"
	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/logging"
	"github.com/aghman/meshbbs/internal/sshd"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/aghman/meshbbs/internal/theme"
	"github.com/aghman/meshbbs/internal/webd"
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

			// Doors get a manager and a way into the BBS (§9.1.1). One manager
			// for the instance, because the limits it enforces — how many
			// copies of a door may run, which nodes hold a lock, how often a
			// door may announce — are statements about the whole board and not
			// about one session.
			doors := door.New(e.clock, log)
			doors.SetHost(svc.Doors())

			srv, err := sshd.NewServer(svc, st, sshd.Options{
				Bind:         e.cfg.SSH.Bind,
				Port:         port,
				KeysDir:      keysDir,
				FilesDir:     filesDir,
				GuestEnabled: e.cfg.Users.GuestEnabled,
				OpenSignup:   e.cfg.Users.RegistrationMode == "open",
				Themes:       themes,
				DefaultTheme: e.cfg.Theme.Default,
				WebEnabled:   e.cfg.Web.Enabled,
				WebURL:       e.cfg.Web.Origin,
				SessionLimit: sessionLimit(e.cfg.Users.SessionTimeLimitMins),
				Doors:        doors,
				BBSName:      e.cfg.Node.DisplayName,
				SysopName:    e.cfg.Node.SysopName,
				Clock:        e.clock,
				Location:     loc,
				Logger:       log,
			})
			if err != nil {
				return err
			}

			// The mesh comes up before the SSH server accepts anyone, so a
			// misconfigured radio, an absent channel or a ham-mode violation
			// stops startup instead of leaving an instance that looks healthy
			// and silently federates nothing.
			var fed *federation
			if e.cfg.Mesh.Enabled {
				fed, err = startFederation(ctx, e, key, st, svc, log)
				if err != nil {
					return fmt.Errorf("mesh: %w", err)
				}
				defer fed.Close()

				// New posts in federated areas go out now rather than waiting
				// for the next anti-entropy beat (§7.3's push path).
				bbs.OnPublishError = func(err error) {
					log.Error("publishing to the mesh", "err", err)
				}
				svc.SetPublisher(fed.engine)
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s is up.\n\n", e.cfg.Node.DisplayName)
			fmt.Fprintf(out, "  node id   %s\n", key.ID().String())
			fmt.Fprintf(out, "  ssh       %s:%d\n", e.cfg.SSH.Bind, port)
			fmt.Fprintf(out, "  themes    %v\n", themes.Names())
			fmt.Fprintf(out, "  guests    %v\n", e.cfg.Users.GuestEnabled)
			fmt.Fprintf(out, "  register  %s\n", e.cfg.Users.RegistrationMode)
			fmt.Fprintf(out, "  files     %s (sftp)\n", filesDir)
			if fed != nil {
				radio := fed.link.Radio()
				fmt.Fprintf(out, "  mesh      %s, channel %q\n",
					fed.link.Name(), e.cfg.Mesh.ChannelName)
				if radio != nil {
					fmt.Fprintf(out, "  radio     %s %s, hop limit %d\n",
						radio.HardwareModel, radio.ModemPreset, radio.HopLimit)
				}
				// §7.6 requires the derived budget in human terms, at startup
				// and on the status screen — not buried in a config file.
				fmt.Fprintf(out, "  airtime   %s\n", fed.Summary())
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "Connect with:  ssh new@localhost -p %d\n", port)
			fmt.Fprintf(out, "Ctrl+C to stop.\n\n")

			// Telnet, when enabled, runs alongside SSH. [D12] made it
			// off-by-default with a loud warning; the warning is emitted by the
			// telnet server itself at every start.
			if e.cfg.Telnet.Enabled {
				tel := sshd.NewTelnetServer(svc, st, sshd.TelnetOptions{
					Bind:         e.cfg.Telnet.Bind,
					Port:         e.cfg.Telnet.Port,
					GuestOnly:    e.cfg.Telnet.GuestOnly,
					MaxSessions:  e.cfg.Telnet.MaxSessions,
					Themes:       themes,
					Theme:        e.cfg.Theme.Default,
					Chat:         srv.Chat(),
					Presence:     srv.Presence(),
					Location:     loc,
					SessionLimit: sessionLimit(e.cfg.Users.SessionTimeLimitMins),
					Logger:       log,
				})
				fmt.Fprintf(out, "  telnet    %s:%d (PLAINTEXT, guest-only)\n\n",
					e.cfg.Telnet.Bind, e.cfg.Telnet.Port)
				go func() {
					if err := tel.ListenAndServe(ctx); err != nil {
						log.Error("telnet server stopped", "err", err)
					}
				}()
			}

			// The web front end runs alongside SSH too. Its settings are
			// validated at startup (§11.3) precisely because every one of them
			// fails at run time in a way the browser cannot explain.
			if e.cfg.Web.Enabled {
				web, err := webd.NewServer(svc, st, webd.Options{
					Bind:                    e.cfg.Web.Bind,
					Port:                    e.cfg.Web.Port,
					Origin:                  e.cfg.Web.Origin,
					TLSCert:                 e.cfg.Web.TLSCert,
					TLSKey:                  e.cfg.Web.TLSKey,
					DisplayName:             e.cfg.Node.DisplayName,
					MaxSessions:             e.cfg.Web.MaxSessions,
					MaxSessionsPerUser:      e.cfg.Web.MaxSessionsPerUser,
					IdleTimeoutMins:         e.cfg.Web.IdleTimeoutMins,
					UnlockedIdleTimeoutMins: e.cfg.Web.UnlockedIdleTimeoutMins,
					SessionTTLHours:         e.cfg.Web.SessionTTLHours,
					SessionLimit:            sessionLimit(e.cfg.Users.SessionTimeLimitMins),
					// Doors are not playable in a browser ([D16]); the list
					// says so and points at the terminal that can run them.
					SSHHost:              sshHostFor(e.cfg.SSH.Bind),
					SSHPort:              port,
					EnrolAttemptsPerHour: e.cfg.Web.EnrolAttemptsPerHour,
					AuthAttemptsPerHour:  e.cfg.Web.AuthAttemptsPerHour,
					TrustedProxies:       e.cfg.TrustedProxyRanges(),
					// Shared with SSH and telnet, not duplicated: a browser user
					// gets a node number, shows up in who's-online, and joins the
					// same chat as everyone else.
					Presence:  srv.Presence().TUI(),
					Chat:      srv.Chat(),
					Themes:    themes,
					ThemeName: e.cfg.Theme.Default,
					Clock:     e.clock,
					Location:  loc,
					Logger:    log,
				})
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "  web       %s\n", e.cfg.Web.Origin)
				fmt.Fprintf(out, "            press P at the SSH menu for a passkey enrolment code\n\n")
				go func() {
					if err := web.ListenAndServe(ctx); err != nil {
						log.Error("web server stopped", "err", err)
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

// sessionLimit turns the configured minutes into a duration, treating anything
// non-positive as no limit at all.
//
// One helper rather than the conversion written three times, because three
// front ends share one limit and a board where the browser times you out and
// SSH does not is a bug report nobody can reproduce.
func sessionLimit(mins int) time.Duration {
	if mins <= 0 {
		return 0
	}
	return time.Duration(mins) * time.Minute
}

// sshHostFor turns the SSH listener's bind address into something a caller can
// type.
//
// A wildcard bind says nothing about how to reach the board, so it produces no
// host at all and the browser prints a placeholder. That is better than
// printing 0.0.0.0, which looks like an answer and is not one.
func sshHostFor(bind string) string {
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		return ""
	}
	return bind
}
