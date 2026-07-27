package cli

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/rng"
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
	cmd.AddCommand(newMeshPortsCmd(), newMeshInfoCmd())
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
				return meshInfoHint(err, conn)
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

	fmt.Fprintf(out, "\nChannels\n")
	if len(info.Channels) == 0 {
		fmt.Fprintf(out, "  (none reported)\n")
	}
	for _, ch := range info.Channels {
		name := ch.Name
		if name == "" {
			name = "(default)"
		}
		enc := "no encryption"
		if ch.Encrypted {
			enc = "encrypted"
		}
		fmt.Fprintf(out, "  %d  %-16s %-10s %s\n", ch.Index, name, ch.Role, enc)
	}
	fmt.Fprintf(out, "\nRegion and preset determine airtime; hop limit multiplies it.\n")
}

// meshInfoHint turns the most common failure into an actionable message.
//
// A stream that produces bytes but no frames is nearly always a baud rate
// mismatch or a port belonging to something else entirely, and the skipped-byte
// counter is what distinguishes that from a node which simply is not talking.
func meshInfoHint(err error, conn *meshtastic.Conn) error {
	if n := conn.Skipped(); n > 256 {
		return fmt.Errorf("%w\n"+
			"(%d bytes arrived but no valid frames: wrong baud rate, or this port "+
			"belongs to another device)", err, n)
	}
	return err
}
