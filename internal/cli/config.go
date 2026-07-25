package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/aghman/meshbbs/internal/config"
	"github.com/spf13/cobra"
)

func newConfigCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate configuration",
	}
	cmd.AddCommand(newConfigCheckCmd(e), newConfigReferenceCmd(e), newConfigShowCmd(e))
	return cmd
}

// newConfigCheckCmd implements the §11.3 first-class validation command. The
// generated service units run it as a pre-start step: better to fail to start
// than to start wrong.
func newConfigCheckCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Validate the configuration and exit non-zero if it is wrong",
		Long: `Validate the configuration file.

Exits non-zero with a precise message if anything is wrong. The generated
service units run this before starting, because a BBS that fails to start is
better than one that starts with the wrong airtime budget.

Note that an unknown key is an error, not a warning: silently ignoring a
misspelled setting is how a mesh gets flooded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// PersistentPreRunE already loaded and validated; reaching here
			// means it is good.
			out := cmd.OutOrStdout()
			path := e.cfgPath
			if path == "" {
				fmt.Fprintf(out, "No config file found; using defaults.\n")
				fmt.Fprintf(out, "  would read: %s\n", e.configFilePath())
			} else {
				fmt.Fprintf(out, "Configuration OK: %s\n", path)
			}
			fmt.Fprintf(out, "  data directory: %s\n", e.dataDir)

			dbPath, err := e.cfg.DatabasePath()
			if err != nil {
				return err
			}
			keysPath, err := e.keysDir()
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "  database:       %s\n", dbPath)
			fmt.Fprintf(out, "  keys:           %s\n", keysPath)
			fmt.Fprintf(out, "  environment:    %s\n", e.cfg.Node.Environment)

			if e.cfg.Node.Environment == config.EnvDevelopment {
				fmt.Fprintf(out, "\nNote: environment is \"development\", so `meshbbs dev` commands are enabled.\n")
			}
			return nil
		},
	}
}

// newConfigReferenceCmd prints the generated key reference (§11.2). It is
// generated from struct tags so it cannot drift from the code.
func newConfigReferenceCmd(e *env) *cobra.Command {
	var markdown bool

	cmd := &cobra.Command{
		Use:   "reference",
		Short: "Print every configuration key, with defaults and documentation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			entries := config.Reference()

			if markdown {
				fmt.Fprintln(out, "# Configuration reference")
				fmt.Fprintln(out)
				fmt.Fprintln(out, "Generated from the source. Do not edit by hand.")
				fmt.Fprintln(out)
				fmt.Fprintln(out, "| Key | Type | Default | Environment variable | Description |")
				fmt.Fprintln(out, "|---|---|---|---|---|")
				for _, en := range entries {
					def := en.Default
					if def == "" {
						def = "*(empty)*"
					} else {
						def = "`" + def + "`"
					}
					fmt.Fprintf(out, "| `%s` | %s | %s | `%s` | %s |\n",
						en.Key, en.Type, def, en.Env, en.Doc)
				}
				return nil
			}

			var section string
			for _, en := range entries {
				if s := strings.SplitN(en.Key, ".", 2)[0]; s != section {
					section = s
					if doc := config.SectionDoc(section); doc != "" {
						fmt.Fprintf(out, "\n[%s]  %s\n", section, doc)
					} else {
						fmt.Fprintf(out, "\n[%s]\n", section)
					}
				}
				fmt.Fprintf(out, "\n  %s (%s)\n", en.Key, en.Type)
				if en.Default != "" {
					fmt.Fprintf(out, "    default: %s\n", en.Default)
				} else {
					fmt.Fprintf(out, "    default: (empty)\n")
				}
				fmt.Fprintf(out, "    env:     %s\n", en.Env)
				fmt.Fprintf(out, "    %s\n", wrap(en.Doc, 72, "    "))
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&markdown, "markdown", false, "emit a Markdown table for docs/config.md")
	return cmd
}

func newConfigShowCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration after defaults, file and environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# effective configuration\n")
			if e.cfgPath != "" {
				fmt.Fprintf(out, "# file: %s\n", e.cfgPath)
			} else {
				fmt.Fprintf(out, "# file: (none; defaults in use)\n")
			}
			fmt.Fprintf(out, "\n[node]\n")
			fmt.Fprintf(out, "display_name = %q\n", e.cfg.Node.DisplayName)
			fmt.Fprintf(out, "sysop_name = %q\n", e.cfg.Node.SysopName)
			fmt.Fprintf(out, "sysop_contact = %q\n", e.cfg.Node.SysopContact)
			fmt.Fprintf(out, "timezone = %q\n", e.cfg.Node.Timezone)
			fmt.Fprintf(out, "environment = %q\n", e.cfg.Node.Environment)
			fmt.Fprintf(out, "\n[log]\n")
			fmt.Fprintf(out, "level = %q\n", e.cfg.Log.Level)
			fmt.Fprintf(out, "format = %q\n", e.cfg.Log.Format)
			fmt.Fprintf(out, "file = %q\n", e.cfg.Log.File)
			fmt.Fprintf(out, "\n[storage]\n")
			fmt.Fprintf(out, "data_dir = %q\n", e.dataDir)
			fmt.Fprintf(out, "database = %q\n", e.cfg.Storage.Database)
			fmt.Fprintf(out, "keys_dir = %q\n", e.cfg.Storage.KeysDir)
			return nil
		},
	}
}

// wrap re-flows text to width, indenting continuation lines.
func wrap(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, w := range words {
		if line > 0 && line+1+len(w) > width {
			b.WriteString("\n" + indent)
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

// writeMinimalConfig writes the small starter file described in §11.2 — a
// dozen lines, not a 400-line commented dump. `config reference` is where the
// full list lives.
func writeMinimalConfig(path string, cfg config.Config) error {
	body := fmt.Sprintf(`# meshbbs configuration
#
# Only non-default settings need to appear here.
# Run "meshbbs config reference" for every available key.

[node]
display_name = %q
sysop_name = %q
environment = %q

[log]
level = %q
`, cfg.Node.DisplayName, cfg.Node.SysopName, cfg.Node.Environment, cfg.Log.Level)

	return os.WriteFile(path, []byte(body), 0o600)
}
