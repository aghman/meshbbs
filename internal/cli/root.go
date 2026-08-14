// Package cli builds the command tree (design §11.6).
//
// Cobra is used for command and flag structure only — NOT Viper for
// configuration. §11.3 requires an unknown config key to be a hard startup
// error, which Viper cannot express, and it would layer its own precedence
// model over the one §11.2 specifies (§4).
package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/config"
	"github.com/aghman/meshbbs/internal/door"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags.
var Version = "dev"

// env carries the resolved runtime environment down the command tree.
type env struct {
	cfg       config.Config
	cfgPath   string
	dataDir   string
	clock     clock.Clock
	openStore func(context.Context) (*store.Store, error)
}

// Execute runs the root command.
func Execute() int {
	// Before cobra sees anything: this binary doubles as its own helper for
	// applying a door's resource limits, because on Unix those can only be set
	// between fork and exec and Go offers no hook there without cgo (§4,
	// internal/door/limits.go). The helper applies the limits and execs the
	// door in place, so it leaves no process behind.
	//
	// Checked here rather than registered as a command so that it cannot be
	// reached by a user typing something plausible, and so that no command
	// parsing runs in a process that is about to become a different program.
	if door.IsHelper(os.Args) {
		if err := door.RunHelper(os.Args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}

	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error; exit non-zero without repeating it.
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	var cfgPath string
	var dataDir string

	e := &env{clock: clock.NewReal()}

	root := &cobra.Command{
		Use:   "meshbbs",
		Short: "A cross-platform BBS federated over Meshtastic LoRa mesh",
		Long: `meshbbs is a modern BBS with SSH access, forums, direct messages, file
areas and door games, federated between independent instances over a
Meshtastic LoRa mesh.

Each instance's identity is derived from its own key — self-certifying, with
no registry and no address authority. Run "meshbbs id" to see this node's ID.`,
		SilenceUsage:  true,
		SilenceErrors: false,
		// Version is wired below so `--version` works at the root.
		Version: Version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return e.load(cfgPath, dataDir)
		},
	}

	root.PersistentFlags().StringVar(&cfgPath, "config", "",
		"path to config.toml (default: <data-dir>/config.toml)")
	root.PersistentFlags().StringVar(&dataDir, "data-dir", "",
		"override the data directory")

	root.AddCommand(
		newServeCmd(e),
		newIDCmd(e),
		newInitCmd(e),
		newConfigCmd(e),
		newUserCmd(e),
		newPeerCmd(e),
		newAreaCmd(e),
		newFileCmd(e),
		newDoorCmd(e),
		newDoorExampleCmd(),
		newSneakernetCmd(e),
		newMeshCmd(e),
		newDevCmd(e),
	)
	return root
}

// load resolves configuration and prepares the lazily-opened store.
//
// Flags are applied here, after file and environment, completing the §11.2
// precedence chain: defaults -> file -> env -> flags.
func (e *env) load(cfgPath, dataDirFlag string) error {
	// Locate the config file. An explicit --config wins; otherwise look under
	// the data directory, and tolerate its absence so the binary works before
	// `meshbbs init` has run.
	if cfgPath == "" {
		probeDir := dataDirFlag
		if probeDir == "" {
			c := config.Default()
			if dataDirFlag != "" {
				c.Storage.DataDir = dataDirFlag
			}
			d, err := c.ResolvedDataDir()
			if err != nil {
				return err
			}
			probeDir = d
		}
		candidate := config.DefaultConfigPath(probeDir)
		if _, err := os.Stat(candidate); err == nil {
			cfgPath = candidate
		}
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if dataDirFlag != "" {
		cfg.Storage.DataDir = dataDirFlag
		if err := cfg.Validate(); err != nil {
			return err
		}
	}

	dir, err := cfg.ResolvedDataDir()
	if err != nil {
		return err
	}

	e.cfg, e.cfgPath, e.dataDir = cfg, cfgPath, dir
	e.openStore = func(ctx context.Context) (*store.Store, error) {
		dbPath, err := cfg.DatabasePath()
		if err != nil {
			return nil, err
		}
		return store.Open(ctx, dbPath, e.clock)
	}
	return nil
}

// keysDir returns the resolved keys directory.
func (e *env) keysDir() (string, error) { return e.cfg.KeysPath() }

// nodeKey loads this instance's identity key.
func (e *env) nodeKey() (identity.NodeKey, error) {
	dir, err := e.keysDir()
	if err != nil {
		return identity.NodeKey{}, err
	}
	return identity.LoadNodeKey(dir)
}

// requireDevelopment blocks the dev subcommands against a production datadir
// (§6.7). The check is on configuration rather than a build tag so that the
// commands exist in every binary and fail loudly rather than silently missing.
func (e *env) requireDevelopment() error {
	if e.cfg.Node.Environment != config.EnvDevelopment {
		return fmt.Errorf(
			"refusing to run a dev command against a %s datadir at %s\n"+
				"(set node.environment = \"development\" in %s if this really is a scratch instance)",
			e.cfg.Node.Environment, e.dataDir,
			cmp(e.cfgPath, config.DefaultConfigPath(e.dataDir)))
	}
	return nil
}

func cmp(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}

// configFilePath returns where the config file is, or would be.
func (e *env) configFilePath() string {
	if e.cfgPath != "" {
		return e.cfgPath
	}
	return filepath.Join(e.dataDir, "config.toml")
}
