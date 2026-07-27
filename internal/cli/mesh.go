package cli

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aghman/meshbbs/internal/hammode"
	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/survey"
	"github.com/spf13/cobra"
)

// newMeshCmd builds the `mesh` group.
//
// §7.8.3 makes a point of this group running WITHOUT a BBS: no database, no SSH
// server, no config beyond the device connection. Someone evaluating whether to
// host an instance can plug in a radio and get an answer before committing to
// anything, which makes these commands an adoption on-ramp as much as a
// diagnostic. `mesh survey` joins them later in Phase 3.
func newMeshCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "Talk to the Meshtastic node attached to this machine",
	}
	// e is deliberately unused: nothing in this group reads config, opens the
	// store or needs the node key. It stays in the signature because `mesh
	// survey` writes its report through the same environment as every other
	// command that produces a file.
	_ = e
	cmd.AddCommand(newMeshPortsCmd(), newMeshInfoCmd(), newMeshSurveyCmd())
	return cmd
}

func newMeshPortsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ports",
		Short: "List serial ports that might have a node attached",
		Long: `List serial ports that look like a Meshtastic node, best guess first.

Detection is a heuristic: most Meshtastic boards use generic USB-serial
bridges that any other device might also use. Treat the list as candidates
to try, not as an identification.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cands, err := meshtastic.DetectPorts()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(cands) == 0 {
				fmt.Fprintln(out, "No candidate serial ports found.")
				fmt.Fprintln(out, "If the node is on WiFi, use: meshbbs mesh info --tcp <host>")
				return nil
			}
			for _, c := range cands {
				fmt.Fprintf(out, "%s\n    %s\n", c.Port, c.Why)
			}
			return nil
		},
	}
}

func newMeshInfoCmd() *cobra.Command {
	var (
		port    string
		host    string
		baud    int
		timeout time.Duration
		verbose bool
	)

	cmd := &cobra.Command{
		Use:   "info",
		Short: "Connect to the node and print what it reports about itself",
		Long: `Connect to the attached Meshtastic node and print its configuration.

This is the first thing to run when setting up, and the first thing to run
when federation over the mesh is not working: it proves the wire, the baud
rate and the firmware API all agree before anything else is blamed.

With no flags it auto-detects a serial port. Use --tcp for a node on WiFi.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			var logSink func(string)
			if verbose {
				logSink = func(s string) { fmt.Fprintf(out, "  [node] %s\n", s) }
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			var (
				conn *meshtastic.Conn
				err  error
			)
			switch {
			case host != "":
				conn, err = meshtastic.DialTCP(ctx, meshtastic.TCPConfig{
					Host: host, OnDeviceLog: logSink,
				})
			default:
				conn, err = meshtastic.DialSerial(meshtastic.SerialConfig{
					Port: port, Baud: baud, OnDeviceLog: logSink,
				})
			}
			if err != nil {
				return err
			}
			defer conn.Close()

			fmt.Fprintf(out, "Connected: %s\n\n", conn.Name())

			// The config ID only has to be unpredictable enough to distinguish
			// our dump from a previous client's, but §12.1 says randomness comes
			// from an injected source, and there is no reason for this to be the
			// one exception.
			var b [8]byte
			if _, err := rng.NewSecret().Read(b[:]); err != nil {
				return err
			}
			id := binary.BigEndian.Uint32(b[:4]) | 1

			info, err := meshtastic.Configure(ctx, conn, meshtastic.ConfigRequest{ID: id})
			if err != nil {
				return meshInfoHint(err, conn, conn.Name())
			}

			printRadioInfo(out, info)
			return nil
		},
	}

	cmd.Flags().StringVar(&port, "port", "", "serial port (default: auto-detect)")
	cmd.Flags().StringVar(&host, "tcp", "", "connect over TCP instead, host[:port]")
	cmd.Flags().IntVar(&baud, "baud", meshtastic.DefaultBaud, "serial baud rate")
	cmd.Flags().DurationVar(&timeout, "timeout", 15*time.Second, "give up after this long")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "print the node's own log output")
	cmd.MarkFlagsMutuallyExclusive("port", "tcp")
	return cmd
}

func printRadioInfo(out io.Writer, info *meshtastic.RadioInfo) {
	fmt.Fprintf(out, "Radio\n")
	fmt.Fprintf(out, "  node number     %d (0x%08x)\n", info.NodeNum, info.NodeNum)
	fmt.Fprintf(out, "  hardware        %s\n", info.HardwareModel)
	fmt.Fprintf(out, "  firmware        %s\n", info.FirmwareVersion)

	fmt.Fprintf(out, "\nLoRa\n")
	fmt.Fprintf(out, "  region          %s\n", info.Region)
	if info.UsePreset {
		fmt.Fprintf(out, "  modem preset    %s\n", info.ModemPreset)
	} else {
		fmt.Fprintf(out, "  modem           custom: bw=%d sf=%d cr=%d\n",
			info.Bandwidth, info.SpreadFactor, info.CodingRate)
	}
	// Hop limit is a direct multiplier on the airtime cost of everything the
	// BBS sends (§1.1), so it is shown here rather than buried in a debug dump.
	fmt.Fprintf(out, "  hop limit       %d\n", info.HopLimit)
	if !info.TxEnabled {
		fmt.Fprintf(out, "  transmit        DISABLED — this node can hear but not speak\n")
	}
	// Ham mode changes what this instance may legally transmit (§8.3), so it
	// belongs next to the radio settings rather than in a log a sysop reads
	// after something has already gone out.
	if info.IsLicensed {
		fmt.Fprintf(out, "  ham mode        ON — licensed operator, FCC Part 97 applies\n")
	}

	// A radio always reports all eight slots, and on a stock device seven of
	// them are disabled. Listing them in full buries the one line that matters
	// under seven that do not — so the unused slots collapse to their numbers,
	// which is the form a sysop actually needs: §7.1 wants a dedicated `bbsnet`
	// channel, and the question being asked here is "which slot is free?".
	fmt.Fprintf(out, "\nChannels\n")
	var free []string
	active := 0
	for _, ch := range info.Channels {
		if ch.Role == "DISABLED" {
			free = append(free, fmt.Sprint(ch.Index))
			continue
		}
		active++
		name := ch.Name
		if name == "" {
			// An empty name means the preset's own default ("LongFast" and so
			// on), not an unnamed channel.
			name = "(preset default)"
		}
		enc := "no encryption"
		if ch.Encrypted {
			enc = "encrypted"
		}
		fmt.Fprintf(out, "  %d  %-18s %-10s %s\n", ch.Index, name, ch.Role, enc)
	}
	if active == 0 {
		fmt.Fprintf(out, "  (none configured)\n")
	}
	if len(free) > 0 {
		fmt.Fprintf(out, "  unused slots: %s\n", strings.Join(free, ", "))
	}
	// The telemetry interval is not trivia: §7.8.1 measures R from two numbers
	// the node only refreshes on this cadence, so at the 30-minute default a
	// survey cannot sample often enough to measure anything.
	fmt.Fprintf(out, "\nTelemetry\n")
	// A stock node reports 2147483647 here — INT32_MAX as an "unset, use the
	// firmware default" sentinel. Rendering that literally gives "596523h",
	// which is not a number anyone should have to interpret.
	if d, ok := survey.TelemetryCadence(info.TelemetryIntervalSecs); ok {
		fmt.Fprintf(out, "  metrics update  every %s\n", d)
	} else {
		fmt.Fprintf(out, "  metrics update  firmware default (%s)\n", survey.DefaultTelemetryInterval)
	}
	if m := info.Metrics; m != nil {
		fmt.Fprintf(out, "  channel busy    %.2f%%\n", m.ChannelUtilization)
		fmt.Fprintf(out, "  our transmit    %.4f%%\n", m.AirUtilTx)
		fmt.Fprintf(out, "  uptime          %s\n",
			(time.Duration(m.UptimeSecs) * time.Second).String())
	}

	if info.IsLicensed {
		fmt.Fprintf(out, "\n%s\n", hammode.Policy{Licensed: true}.Banner())
	}

	fmt.Fprintf(out, "\nRegion and preset determine airtime; hop limit multiplies it.\n")
}

// meshInfoHint turns the two failures that actually happen into advice.
//
// Both were found by pointing this command at a real radio, and they present
// almost identically — the command hangs and then reports a timeout — while
// having nothing to do with each other. The frame and skipped-byte counters are
// what tell them apart.
func meshInfoHint(err error, conn *meshtastic.Conn, where string) error {
	switch {
	case conn.Skipped() > 256 && conn.Frames() == 0:
		// Bytes, but never a valid header. A wrong baud rate, or a port that
		// belongs to something else entirely.
		return fmt.Errorf("%w\n"+
			"(%d bytes arrived but not one valid frame: wrong baud rate, or %s "+
			"is not a Meshtastic device)", err, conn.Skipped(), where)
	case conn.Frames() == 0:
		// Silence. Over TCP this is what connecting to the web UI's port looks
		// like: the connection succeeds, the server waits for a request it
		// understands, and neither side ever speaks.
		return fmt.Errorf("%w\n"+
			"(connected to %s but it sent nothing: check this is the Meshtastic "+
			"API port %d, not the web interface)", err, where, meshtastic.DefaultTCPPort)
	}
	return err
}
