package cli

import (
	"context"
	"fmt"

	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

func newDevCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Development helpers (refused against a production datadir)",
		Long: `Development helpers.

These commands are compiled into every binary but refuse to run unless
node.environment is "development" — a missing command is confusing, whereas a
command that explains why it will not run is not.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// The parent's PersistentPreRunE loads config; cobra runs only the
			// closest one, so re-run it here before the environment check.
			if p := cmd.Root().PersistentPreRunE; p != nil {
				if err := p(cmd, args); err != nil {
					return err
				}
			}
			return e.requireDevelopment()
		},
	}
	cmd.AddCommand(newDevSeedCmd(e))
	cmd.AddCommand(newDevDoorRunCmd(e))
	return cmd
}

func newDevSeedCmd(e *env) *cobra.Command {
	var seed uint64
	var users, posts int

	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Populate this instance with deterministic test data",
		Long: `Populate the database with deterministic test data.

Determinism is the point. The same --seed always produces the same users,
posts and record IDs, on any platform. That is what makes the simulation
harness useful: several instances seeded from known seeds produce a known,
reproducible divergence to reconcile, which is how anti-entropy gets tested at
all.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := e.nodeKey()
			if err != nil {
				return err
			}

			src := rng.NewSeeded(seed)
			userSrc := src.Derive("users")
			postSrc := src.Derive("posts")

			return withStore(e, cmd, func(ctx context.Context, st *store.Store) error {
				out := cmd.OutOrStdout()

				created := 0
				for i := 0; i < users; i++ {
					nick := seedNick(userSrc, i)
					_, err := st.CreateUser(ctx, store.CreateUserOptions{
						Nick:        nick,
						DisplayName: nick,
						CanLogin:    true,
					})
					if err != nil {
						// A repeat run over the same database hits taken nicks;
						// that is fine and should not abort the seed.
						continue
					}
					created++
				}
				fmt.Fprintf(out, "Created %d users\n", created)

				areas := []string{"general", "tech", "swap"}
				for i := 0; i < posts; i++ {
					// The area is chosen FIRST, because sequences are allocated
					// per area (migration 0003) and a number drawn from the
					// wrong one would leave a gap that never closes.
					area := areas[postSrc.IntN(len(areas))]
					seqNum, err := st.NextSeq(ctx, record.AreaTagFor(area))
					if err != nil {
						return err
					}
					body := fmt.Sprintf("seeded post %d in %s", i, area)
					r, err := record.New(key, record.Record{
						Seq:  seqNum,
						TS:   uint32(e.clock.Now().Unix()),
						Type: record.TypePost,
						Area: record.AreaTagFor(area),
						Body: []byte(body),
					})
					if err != nil {
						return err
					}
					if err := st.PutRecord(ctx, r); err != nil {
						return err
					}
				}
				fmt.Fprintf(out, "Created %d posts\n", posts)

				total, err := st.CountRecords(ctx)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "Records now in log: %d\n", total)
				return nil
			})
		},
	}

	cmd.Flags().Uint64Var(&seed, "seed", 1, "PRNG seed; the same seed reproduces the same data exactly")
	cmd.Flags().IntVar(&users, "users", 10, "number of users to create")
	cmd.Flags().IntVar(&posts, "posts", 50, "number of posts to create")
	return cmd
}

// seedNick builds a deterministic, valid nick (§6.7 rules: starts with a
// letter, 2-16 characters).
func seedNick(src rng.Source, i int) string {
	syllables := []string{
		"ada", "bel", "cor", "dex", "eli", "fen", "gus", "hal",
		"ivy", "jem", "kit", "lor", "mox", "nia", "orl", "pip",
	}
	a := syllables[src.IntN(len(syllables))]
	b := syllables[src.IntN(len(syllables))]
	return fmt.Sprintf("%s%s%d", a, b, i)
}

var _ = context.Background
