package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/meshlink"
	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/survey"
	"github.com/spf13/cobra"
)

func newMeshSurveyCmd() *cobra.Command {
	var (
		port      string
		host      string
		baud      int
		channel   string
		baseline  time.Duration
		load      time.Duration
		sample    time.Duration
		hops      string
		duty      float64
		payload   int
		instances int
		out       string
		confirm   bool
	)

	cmd := &cobra.Command{
		Use:   "survey",
		Short: "Measure R, the flood multiplier, on your mesh",
		Long: `Measure R — how many times the mesh rebroadcasts each packet you send.

R is the one number in the design that cannot be derived or simulated: it is a
property of your mesh's RF topology, seen from your node's position. Every
airtime budget scales linearly with it, and the shipped default of 4 is a
guess.

The survey listens with the radio silent to establish a baseline, then
transmits a small, paced load at each hop limit and attributes the rise in
channel utilization to rebroadcasts of its own packets.

THIS COMMAND TRANSMITS. It is polite about it — it refuses to start on a busy
channel, aborts if the channel gets busier, and defaults to about 1% duty —
but it does put packets on a shared band for as long as you tell it to. Pass
--yes once you have read what it plans to do.

Your node must also report its metrics often enough to sample. A stock node
updates them every 30 minutes, which is far too slow; set Telemetry → device
metrics update interval to 60s in the Meshtastic app first.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			o := cmd.OutOrStdout()

			hopList, err := parseHops(hops)
			if err != nil {
				return err
			}

			// A load packet larger than the radio will actually send does not
			// fail loudly — it fails in the one way that corrupts the result.
			//
			// Above meshlink.MaxPayload the firmware never puts the frame on the
			// air (see the mtuReserve comment: eight consecutive 233-byte
			// broadcasts, none of which arrived, nothing reported anywhere).
			// But air_util_tx still rises, because the radio charges us for the
			// attempt. So the survey would measure its own transmit time going
			// up, see no answering rise in channel utilization, and conclude
			// R ≈ 1 — with a tight confidence interval, because the samples
			// really are that consistent. A confidently wrong number is worse
			// than a refusal, and worse than a wide one.
			if payload < 1 || payload > meshlink.MaxPayload {
				return fmt.Errorf(
					"--bytes must be between 1 and %d: above that the firmware silently drops the frame "+
						"while still charging it to airtime, and the survey would measure R as 1",
					meshlink.MaxPayload)
			}

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			var conn *meshtastic.Conn
			if host != "" {
				conn, err = meshtastic.DialTCP(ctx, meshtastic.TCPConfig{Host: host})
			} else {
				conn, err = meshtastic.DialSerial(meshtastic.SerialConfig{Port: port, Baud: baud})
			}
			if err != nil {
				return err
			}
			defer conn.Close()

			// Seeded from the crypto source rather than the clock: §12.1 keeps
			// time.Now() out of domain code, and a survey has no reason to be
			// the exception.
			var seed [8]byte
			if _, err := rng.NewSecret().Read(seed[:]); err != nil {
				return err
			}
			radio, err := survey.Attach(ctx, conn, survey.RadioConfig{
				Channel: channel,
				Refresh: sample,
				Rand:    rng.NewSeeded(binary.BigEndian.Uint64(seed[:])),
			})
			if err != nil {
				return err
			}
			go radio.Run(ctx)

			cfg := survey.Config{
				Baseline:     baseline,
				Load:         load,
				Sample:       sample,
				HopLimits:    hopList,
				TargetDuty:   duty,
				PayloadBytes: payload,
				OnProgress:   func(s string) { fmt.Fprintf(o, "  %s\n", s) },
			}

			plan := planFor(radio.Preset(), cfg)
			fmt.Fprintf(o, "%s\n", plan)
			if !confirm {
				return errors.New("not transmitting: re-run with --yes once the plan above looks right")
			}

			rep, err := survey.Run(ctx, radio, cfg)
			if err != nil {
				// A refusal is a result, not a crash: it tells the sysop
				// something true about their mesh or their node.
				return err
			}
			rep.Region = radio.Region()

			fmt.Fprintln(o)
			rep.Write(o, instances)

			if out != "" {
				f, err := os.Create(out)
				if err != nil {
					return err
				}
				defer f.Close()
				rep.Write(f, instances)
				fmt.Fprintf(o, "\nReport written to %s\n", out)
				fmt.Fprintf(o, "Sharing it is how the project replaces the guessed default of 4\n")
				fmt.Fprintf(o, "with a distribution grounded in real deployments (§7.8.3).\n")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&port, "port", "", "serial port (default: auto-detect)")
	cmd.Flags().StringVar(&host, "tcp", "", "connect over TCP instead, host[:port]")
	cmd.Flags().IntVar(&baud, "baud", meshtastic.DefaultBaud, "serial baud rate")
	cmd.Flags().StringVar(&channel, "channel", "", "channel to transmit the load on (default: primary)")
	cmd.Flags().DurationVar(&baseline, "baseline", 30*time.Minute, "how long to listen before transmitting")
	cmd.Flags().DurationVar(&load, "load", 30*time.Minute, "how long to transmit at each hop limit")
	cmd.Flags().DurationVar(&sample, "sample", time.Minute, "how often to read the node's metrics")
	cmd.Flags().StringVar(&hops, "hops", "1,3,5", "hop limits to sweep")
	cmd.Flags().Float64Var(&duty, "duty", 1.0, "percentage of airtime to use during load")
	cmd.Flags().IntVar(&payload, "bytes", 200, fmt.Sprintf("size of each load packet (1-%d)", meshlink.MaxPayload))
	cmd.Flags().IntVar(&instances, "instances", survey.DefaultInstanceCount, "instances to divide the budget between")
	cmd.Flags().StringVar(&out, "out", "", "write the shareable report here")
	cmd.Flags().BoolVar(&confirm, "yes", false, "confirm that this may transmit")
	cmd.MarkFlagsMutuallyExclusive("port", "tcp")
	return cmd
}

// planFor states what the survey will put on the air, before it does.
//
// A sysop should not have to run the thing to find out that it wanted three
// hours and 400 packets. The airtime figure is the honest one — local time
// multiplied by nothing, since R is what we are here to find out.
func planFor(p airtime.Preset, cfg survey.Config) string {
	hopCount := len(cfg.HopLimits)
	if hopCount == 0 {
		hopCount = 3
	}
	// Two baselines: one before the load phases and one after, so ambient drift
	// over the run is visible rather than folded into R.
	total := 2*cfg.Baseline + time.Duration(hopCount)*cfg.Load

	one := p.Packet(cfg.PayloadBytes)
	perPhase := 0
	if cfg.TargetDuty > 0 {
		every := time.Duration(float64(one) * 100 / cfg.TargetDuty)
		perPhase = int(cfg.Load / every)
	}
	packets := perPhase * hopCount
	onAir := time.Duration(packets) * one

	var sb strings.Builder
	fmt.Fprintf(&sb, "Survey plan\n")
	fmt.Fprintf(&sb, "  radio          %s\n", p)
	fmt.Fprintf(&sb, "  baseline       %s, silent — twice, before and after\n", cfg.Baseline)
	fmt.Fprintf(&sb, "  load           %s at each of hop %v\n", cfg.Load, cfg.HopLimits)
	fmt.Fprintf(&sb, "  total time     %s\n", total)
	fmt.Fprintf(&sb, "  transmits      about %d packets of %d bytes\n", packets, cfg.PayloadBytes)
	fmt.Fprintf(&sb, "  our airtime    about %s (before rebroadcasts)\n", onAir.Round(time.Second))
	return sb.String()
}

func parseHops(s string) ([]uint32, error) {
	var out []uint32
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 7 {
			return nil, fmt.Errorf("hop limit %q is not in the Meshtastic range 0-7", part)
		}
		out = append(out, uint32(n))
	}
	if len(out) == 0 {
		return nil, errors.New("no hop limits given")
	}
	return out, nil
}
