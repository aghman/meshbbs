package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// Installing a door (§9.1.1, §11.5).
//
// Door configuration lives in the database rather than config.toml, so this is
// the surface that writes it. That is not an arbitrary split: `config check`
// refuses to start a BBS whose config file does not parse (§11.3), and adding a
// door is a routine act that should never be able to take the board down.
//
// Works with the server stopped, like every other CLI command (§11.6), which is
// what makes it the recovery path when a door is what went wrong.
func newDoorCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "door",
		Short: "Install, list and remove door games",
	}
	cmd.AddCommand(
		newDoorListCmd(e),
		newDoorShowCmd(e),
		newDoorAddCmd(e),
		newDoorRemoveCmd(e),
		newDoorEnableCmd(e, true),
		newDoorEnableCmd(e, false),
		newDoorExamplesCmd(e),
	)
	return cmd
}

func newDoorListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed doors",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDoorStore(cmd, e, func(ctx context.Context, st *store.Store) error {
				doors, err := st.ListDoors(ctx)
				if err != nil {
					return err
				}
				if len(doors) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(),
						"No doors installed. Add one with `meshbbs door add`.")
					return nil
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "NAME\tAPI\tLIMIT\tSTATE\tPATH")
				for _, d := range doors {
					state := "enabled"
					if !d.Enabled {
						state = "disabled"
					}
					fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
						d.Name, d.APILevel, d.WallClock, state, d.Path)
				}
				return w.Flush()
			})
		},
	}
}

func newDoorShowCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print everything configured for one door",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDoorStore(cmd, e, func(ctx context.Context, st *store.Store) error {
				d, err := st.GetDoor(ctx, args[0])
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
				fmt.Fprintf(w, "name\t%s\n", d.Name)
				fmt.Fprintf(w, "path\t%s\n", d.Path)
				fmt.Fprintf(w, "args\t%s\n", strings.Join(d.Args, " "))
				fmt.Fprintf(w, "cwd\t%s\n", d.Cwd)
				fmt.Fprintf(w, "env_passthrough\t%s\n", strings.Join(d.EnvPassthrough, " "))
				fmt.Fprintf(w, "dropfile\t%s\n", d.DropfileType)
				fmt.Fprintf(w, "wall_clock\t%s\n", d.WallClock)
				fmt.Fprintf(w, "cpu_limit\t%s\n", limitOrNone(d.CPULimit))
				fmt.Fprintf(w, "mem_limit\t%s\n", bytesOrNone(d.MemLimit))
				fmt.Fprintf(w, "max_concurrent\t%d\n", d.MaxConcurrent)
				fmt.Fprintf(w, "node_lock\t%t\n", d.NodeLock)
				fmt.Fprintf(w, "required_capability\t%s\n", d.RequiredCapability)
				fmt.Fprintf(w, "api_level\t%d (%s)\n", d.APILevel, apiLevelName(d.APILevel))
				fmt.Fprintf(w, "announce_area\t%s\n", d.AnnounceArea)
				fmt.Fprintf(w, "league_area\t%s\n", d.LeagueArea)
				fmt.Fprintf(w, "league_per_hour\t%d\n", d.LeaguePerHour)
				fmt.Fprintf(w, "announce_per_hour\t%d\n", d.AnnouncePerHour)
				fmt.Fprintf(w, "state_quota\t%d bytes\n", d.StateQuota)
				fmt.Fprintf(w, "enabled\t%t\n", d.Enabled)
				if err := w.Flush(); err != nil {
					return err
				}

				if d.APILevel >= store.APIActAsUser {
					fmt.Fprintf(out, "\nThis door can post and send messages AS the user "+
						"playing it (§9.1.1 level 4). It cannot exceed what that user may "+
						"do themselves, every such action is written to the audit log, and "+
						"each user is told the first time it happens.\n")
				}
				warnUnsupportedLimits(out, d)
				return nil
			})
		},
	}
}

func newDoorAddCmd(e *env) *cobra.Command {
	var (
		args_           []string
		cwd             string
		envPassthrough  []string
		dropfile        string
		wallClock       time.Duration
		cpuLimit        time.Duration
		memLimit        int64
		maxConcurrent   int
		nodeLock        bool
		requiredCap     string
		apiLevel        int
		announceArea    string
		announcePerHour int
		leagueArea      string
		leaguePerHour   int
		stateQuota      int64
	)

	cmd := &cobra.Command{
		Use:   "add <name> <executable>",
		Short: "Install a door, or update one already installed",
		Long: `Install a door.

The executable's path must be absolute: which binary runs should not be a
question answered by the server's PATH at launch time.

API level is 3 by default, which lets a door read its session, keep private
state, and post announcements as itself. Level 4 is act_as_user and has to be
asked for explicitly — it lets a door post and send messages as the person
playing it.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, path := args[0], args[1]
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			if cwd == "" {
				cwd = filepath.Dir(abs)
			}
			cwd, err = filepath.Abs(cwd)
			if err != nil {
				return err
			}

			return withDoorStore(cmd, e, func(ctx context.Context, st *store.Store) error {
				d := store.Door{
					Name: name, Path: abs, Args: args_, Cwd: cwd,
					EnvPassthrough:     envPassthrough,
					DropfileType:       dropfile,
					MaxConcurrent:      maxConcurrent,
					NodeLock:           nodeLock,
					WallClock:          wallClock,
					CPULimit:           cpuLimit,
					MemLimit:           memLimit,
					RequiredCapability: requiredCap,
					APILevel:           apiLevel,
					AnnounceArea:       announceArea,
					LeagueArea:         leagueArea,
					LeaguePerHour:      leaguePerHour,
					AnnouncePerHour:    announcePerHour,
					StateQuota:         stateQuota,
					Enabled:            true,
				}
				if err := st.PutDoor(ctx, d, cliActor); err != nil {
					return err
				}

				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Installed %s at API level %d (%s).\n",
					d.Name, d.APILevel, apiLevelName(d.APILevel))
				warnUnsupportedLimits(out, d)
				if d.APILevel >= store.APIActAsUser {
					// Loudly, once, at the moment of the decision. A grant this
					// wide should not be something a sysop discovers later in a
					// table (§9.1.1).
					fmt.Fprintf(out,
						"\n  %s can now post and send messages AS whoever plays it.\n"+
							"  It cannot do anything they could not do themselves, every\n"+
							"  action is audited, and each player is told the first time.\n"+
							"  Revoke with: meshbbs door add %s %s --api-level 3\n\n",
						d.Name, d.Name, d.Path)
				}
				return nil
			})
		},
	}

	f := cmd.Flags()
	f.StringArrayVar(&args_, "arg", nil, "argument passed to the door, repeatable")
	f.StringVar(&cwd, "cwd", "", "working directory (default: the directory holding the executable)")
	f.StringArrayVar(&envPassthrough, "env-passthrough", nil,
		"server environment variable the door may see, repeatable (it inherits nothing else)")
	f.StringVar(&dropfile, "dropfile", store.DropfileNone,
		"dropfile to write: none, door.sys, door32.sys or dorinfo1.def")
	f.DurationVar(&wallClock, "limit", time.Hour,
		"wall-clock limit; the door is killed when it runs out")
	f.DurationVar(&cpuLimit, "cpu-limit", 0,
		"processor time a door may use, 0 for no limit; not the safety net, --limit is")
	f.Int64Var(&memLimit, "mem-limit", 0,
		"memory a door may hold in bytes, 0 for no limit; cannot be enforced on macOS")
	f.IntVar(&maxConcurrent, "max-concurrent", 0, "simultaneous instances, 0 for no cap")
	f.BoolVar(&nodeLock, "node-lock", false, "allow only one instance per node number")
	f.StringVar(&requiredCap, "required-capability", "",
		"capability a user needs to run this door, on top of run_doors")
	f.IntVar(&apiLevel, "api-level", store.APIAnnounce,
		"door API level 1-4; 4 is act_as_user and must be set deliberately (§9.1.1)")
	f.StringVar(&leagueArea, "league-area", "",
		"federated door area this door reports game events to (§9.5)")
	f.IntVar(&leaguePerHour, "league-per-hour", 6,
		"how many game events this door may report an hour")
	f.StringVar(&announceArea, "announce-area", "",
		"local area this door may post announcements to; it may not be federated")
	f.IntVar(&announcePerHour, "announce-per-hour", 4, "announcements allowed per hour")
	f.Int64Var(&stateQuota, "state-quota", 65536, "bytes of saved state this door may keep")
	return cmd
}

func newDoorRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a door and the state it kept",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDoorStore(cmd, e, func(ctx context.Context, st *store.Store) error {
				if err := st.DeleteDoor(ctx, args[0], cliActor); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"Removed %s, along with every saved game and high score it kept.\n",
					args[0])
				return nil
			})
		},
	}
}

func newDoorEnableCmd(e *env, enable bool) *cobra.Command {
	verb, past := "enable", "Enabled"
	if !enable {
		verb, past = "disable", "Disabled"
	}
	return &cobra.Command{
		Use:   verb + " <name>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " a door without removing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withDoorStore(cmd, e, func(ctx context.Context, st *store.Store) error {
				d, err := st.GetDoor(ctx, args[0])
				if err != nil {
					return err
				}
				d.Enabled = enable
				if err := st.PutDoor(ctx, d, cliActor); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s.\n", past, d.Name)
				return nil
			})
		},
	}
}

// cliActor is who the audit log records for a command-line change. The CLI
// works with the server stopped and has no session, so there is no nick to
// name — saying so is better than borrowing one.
const cliActor = "cli"

func apiLevelName(level int) string {
	switch level {
	case store.APISession:
		return "session only"
	case store.APIState:
		return "session and private state"
	case store.APIAnnounce:
		return "session, state and announcements"
	case store.APIActAsUser:
		return "act_as_user — posts and messages as the player"
	}
	return "unknown"
}

// withDoorStore opens the database for one command and closes it after.
func withDoorStore(cmd *cobra.Command, e *env, fn func(context.Context, *store.Store) error) error {
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

// warnUnsupportedLimits says so when a door carries a limit this platform
// cannot apply.
//
// At install time as well as on show, because the alternative is a sysop
// discovering it when a player cannot start the game. The door is refused at
// launch rather than run without the limit (§9.4), and saying that here is the
// difference between a configuration mistake and an outage.
func warnUnsupportedLimits(out io.Writer, d store.Door) {
	bad := door.UnsupportedLimits(d.CPULimit, d.MemLimit)
	if len(bad) == 0 {
		return
	}
	fmt.Fprintf(out, "\nWARNING: %s cannot be enforced on %s, so %s will REFUSE to\n"+
		"launch until it is cleared. A limit nobody applies must not be mistaken\n"+
		"for protection.\n",
		strings.Join(bad, " and "), runtime.GOOS, d.Name)
}

// limitOrNone renders a duration limit, or says there is not one.
func limitOrNone(d time.Duration) string {
	if d <= 0 {
		return "none"
	}
	return d.String()
}

// bytesOrNone renders a byte limit, or says there is not one.
func bytesOrNone(n int64) string {
	if n <= 0 {
		return "none"
	}
	return fmt.Sprintf("%d bytes", n)
}
