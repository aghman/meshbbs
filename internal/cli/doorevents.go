package cli

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aghman/meshbbs/internal/store"
	"github.com/spf13/cobra"
)

// newDoorEventsCmd shows what the inter-BBS leagues on this node are carrying.
//
// # Why a sysop needs this at all
//
// A league is the one kind of area whose contents nothing on this BBS may be
// able to read. A forum's posts are shown in the forum; a file area's catalog is
// browsable; a league's records are interpreted by a DOOR, and a node can
// perfectly well carry a league for which no door is installed — that is not a
// misconfiguration, it is how a multi-hop league reaches boards that cannot hear
// each other.
//
// But it is not free. A record this node holds is a record it will serve to
// peers that ask, so carrying somebody else's league spends this node's airtime
// on a game nobody here plays. That is a legitimate thing to choose and an
// illegitimate thing to do by accident, so it needs somewhere to be visible.
func newDoorEventsCmd(e *env) *cobra.Command {
	var game string
	var limit int

	cmd := &cobra.Command{
		Use:   "events [league]",
		Short: "Show what the door leagues on this node are carrying",
		Long: `Show inter-BBS door league traffic (§9.5).

Without arguments this summarises every league: what has arrived, what is
waiting to go out, and which doors are pointed at it. Name a league to list its
events.

A league with no door installed is not an error. Records still arrive, are still
stored, and are still served to peers that ask for them — which is how a league
reaches boards that cannot hear each other directly. It does mean this node
spends airtime on a game nobody here plays, so it is worth knowing about.

Nothing joins a league automatically. A league exists here because a sysop
created the area and federated it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			st, err := e.openStore(ctx)
			if err != nil {
				return err
			}
			defer st.Close()

			if len(args) == 1 {
				return listLeagueEvents(ctx, cmd, st, e.clock.Now(), args[0], game, limit)
			}
			return summariseLeagues(ctx, cmd, st)
		},
	}

	cmd.Flags().StringVar(&game, "game", "", "only show events for this game")
	cmd.Flags().IntVar(&limit, "limit", 50, "how many events to show")
	return cmd
}

func summariseLeagues(ctx context.Context, cmd *cobra.Command, st *store.Store) error {
	leagues, err := st.Leagues(ctx)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(leagues) == 0 {
		fmt.Fprintln(out, "No door leagues. Create one with \"meshbbs area create <name> --kind door\".")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LEAGUE\tFEDERATED\tGAMES\tRECEIVED\tWAITING\tDOORS")
	unplayed := 0
	for _, l := range leagues {
		fed := "local only"
		if l.Federated {
			fed = "yes"
		}
		games := strings.Join(l.Games, ", ")
		if games == "" {
			games = "-"
		}
		doors := strings.Join(l.Doors, ", ")
		if doors == "" {
			doors = "(none installed)"
			unplayed++
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d in %d records\t%d\t%s\n",
			l.Area, fed, games, l.Events, l.Received, l.Waiting, doors)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Said once, under the table, rather than as a warning per row: it is a
	// choice a sysop may have made deliberately, and a scold on every line
	// would be read as an error.
	if unplayed > 0 {
		fmt.Fprintf(out, "\n%d league(s) here have no door installed. Their records still\n", unplayed)
		fmt.Fprintf(out, "arrive and are still served to peers that ask, which is what carries a\n")
		fmt.Fprintf(out, "league across boards that cannot hear each other — and which spends this\n")
		fmt.Fprintf(out, "node's airtime on a game nobody here plays. Un-federate the area to stop.\n")
	}
	return nil
}

func listLeagueEvents(ctx context.Context, cmd *cobra.Command, st *store.Store, now time.Time, area, game string, limit int) error {
	a, err := st.GetDoorArea(ctx, area)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()

	events, _, truncated, err := st.DoorEventsSince(ctx, a.Tag, game, 0, limit)
	if err != nil {
		return err
	}
	if truncated {
		fmt.Fprintln(out, "Some earlier events have been pruned by retention.")
	}
	if len(events) == 0 {
		fmt.Fprintf(out, "Nothing has arrived on %s yet.\n", a.Name)
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "WHEN\tFROM\tEVENT")
		for _, ev := range events {
			fmt.Fprintf(w, "%s\t%s\t%s\n",
				sinceWhen(now, ev.At), ev.Origin.Short(), renderDoorEvent(ev))
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	queued, err := st.QueuedDoorEvents(ctx, a.Name, game)
	if err != nil {
		return err
	}
	if len(queued) > 0 {
		fmt.Fprintf(out, "\n%d event(s) waiting to go out. They are batched rather than sent one\n", len(queued))
		fmt.Fprintf(out, "at a time, because a record's signature costs more than the news in it.\n")
	}
	return nil
}

// renderDoorEvent is the one-line form. The payload is never printed: it is
// door-defined bytes that were deliberately not checked for control characters,
// and this output goes to a terminal.
func renderDoorEvent(ev store.DeliveredDoorEvent) string {
	var sb strings.Builder
	sb.WriteString(ev.Actor)
	if ev.Target != "" {
		sb.WriteString(" → ")
		sb.WriteString(ev.Target)
		if !ev.TargetNode.IsZero() {
			sb.WriteString("@" + ev.TargetNode.Short())
		}
	}
	fmt.Fprintf(&sb, " · kind %d", ev.Kind)
	if n := len(ev.Payload); n > 0 {
		fmt.Fprintf(&sb, " · %d bytes", n)
	}
	return sb.String()
}

// sinceWhen renders an advisory timestamp as an age, against an injected now.
//
// An age rather than a date because §6.2.1 makes a record's ts advisory: it was
// written by another node's clock, and rendering it as a precise local time
// implies an accuracy the protocol does not claim.
//
// `now` is passed rather than read from the wall clock because §12.1 keeps
// time.Now() out of domain code — and this is not pedantry here: a record's ts
// comes from ANOTHER node, so "1h ago" is already the difference between two
// clocks that were never synchronised. Sourcing our half of that subtraction
// from the injected clock is what makes the rendering reproducible in a test.
func sinceWhen(now time.Time, ts int64) string {
	d := now.Sub(time.Unix(ts, 0))
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
