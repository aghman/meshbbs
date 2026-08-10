package cli

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
	"github.com/aghman/meshbbs/internal/bsmp"
	"github.com/aghman/meshbbs/internal/bundle"
	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/config"
	"github.com/aghman/meshbbs/internal/gossip"
	"github.com/aghman/meshbbs/internal/governor"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/meshlink"
	"github.com/aghman/meshbbs/internal/meshtastic"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/rng"
	"github.com/aghman/meshbbs/internal/store"
)

// federation is everything needed to sync with peers over the mesh.
//
// It is assembled in one place because the pieces only make sense together: the
// governor prices what the link sends, the outbox decides what to hand the link,
// and the engine decides what the outbox is asked for. Each was built and tested
// separately; this is where they become an instance that federates.
type federation struct {
	link   *meshlink.Link
	gov    *governor.Governor
	engine *gossip.Engine
	inbox  *bsmp.Inbox
	outbox *bsmp.Outbox
	gstore *store.GossipStore
	st     *store.Store
	ctx    context.Context
	clk    clock.Clock
	log    *slog.Logger
}

// startFederation brings up the mesh side of a running BBS.
//
// The radio is connected synchronously so that a missing device, a wrong
// channel or a ham-mode violation is an error the sysop sees at startup rather
// than a silent federation outage. Everything after that is supervised.
func startFederation(ctx context.Context, e *env, key identity.NodeKey, st *store.Store, log *slog.Logger) (*federation, error) {
	cfg := e.cfg.Mesh

	gstore, err := store.NewGossipStore(ctx, st, func(err error) {
		// The engine's Store interface cannot return these, and swallowing
		// them would make "my posts are not federating" undiagnosable.
		log.Warn("record store", "err", err)
	})
	if err != nil {
		return nil, err
	}

	// The radio has to be read before the governor can be built: airtime
	// depends on the modem preset, and the duty cycle on the region. Neither is
	// something a sysop should have to copy into our config by hand.
	// The radio's own debug output, which it writes to the same wire as the
	// protocol. Discarding it was a real cost: when a node refuses a connection
	// or drops one, the reason is usually in this log and nowhere else, and
	// without it the only evidence is a bare "connection reset by peer".
	dial := dialerFor(cfg, func(s string) { log.Debug("radio", "log", s) })
	probe, err := dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the radio: %w", err)
	}
	var seed [8]byte
	if _, err := rng.NewSecret().Read(seed[:]); err != nil {
		probe.Close()
		return nil, err
	}
	info, err := meshtastic.Configure(ctx, probe, meshtastic.ConfigRequest{
		ID: binary.BigEndian.Uint32(seed[:4]) | 1,
	})
	probe.Close()
	if err != nil {
		return nil, fmt.Errorf("reading the radio's configuration: %w", err)
	}

	preset, err := presetFrom(info)
	if err != nil {
		return nil, err
	}
	quiet, err := e.cfg.QuietHourWindows()
	if err != nil {
		return nil, err
	}

	gov, err := governor.New(governor.Config{
		Preset:                  preset,
		Region:                  info.Region,
		CeilingPercent:          cfg.AirtimeCeilingPct,
		Instances:               cfg.ExpectedInstanceCount,
		FloodMultiplier:         cfg.FloodMultiplier,
		FloodMultiplierOverride: cfg.FloodMultiplierOverride,
		QuietHours:              quiet,
		Clock:                   e.clock,
		OnAlert:                 func(s string) { log.Warn("airtime governor", "alert", s) },
	})
	if err != nil {
		return nil, err
	}

	var linkSeed [8]byte
	if _, err := rng.NewSecret().Read(linkSeed[:]); err != nil {
		return nil, err
	}
	ml, err := meshlink.New(meshlink.Config{
		Key:            key,
		Dial:           dial,
		Channel:        cfg.ChannelName,
		Governor:       gov,
		Clock:          e.clock,
		Rand:           rng.NewSeeded(binary.BigEndian.Uint64(linkSeed[:])),
		HopLimit:       uint32(cfg.HopLimit),
		RxTimeout:      rxTimeout(cfg.RxTimeoutS),
		Part97Override: e.cfg.AcceptsPart97Responsibility(),
		OnEvent:        func(s string) { log.Info("mesh", "event", s) },
		OnTrace:        func(s string) { log.Debug("mesh", "trace", s) },
	})
	if err != nil {
		return nil, err
	}
	if err := ml.Start(ctx); err != nil {
		return nil, err
	}

	dict, dicts, err := federationDictionaries()
	if err != nil {
		ml.Close()
		return nil, err
	}

	// §8.3: the ham-mode gate reads through the LINK, not from config, because
	// ham mode is what the radio reports and a sysop can turn it on without
	// restarting the BBS.
	outbox, err := federationOutbox(key.ID(), ml, dict,
		gstore,
		func() bool { return ml.Part97().AllowsEncryptedDMs() }, log)
	if err != nil {
		ml.Close()
		return nil, err
	}

	var engSeed [8]byte
	if _, err := rng.NewSecret().Read(engSeed[:]); err != nil {
		ml.Close()
		return nil, err
	}
	engine := gossip.New(key.ID(), gstore, outbox, e.clock,
		rng.NewSeeded(binary.BigEndian.Uint64(engSeed[:])), gossip.DefaultConfig())

	inbox, err := bsmp.NewInbox(bsmp.InboxConfig{
		Engine:       engine,
		Dictionaries: dicts,
		Clock:        e.clock,
		OnEvent:      func(s string) { log.Info("federation", "event", s) },
	})
	if err != nil {
		ml.Close()
		return nil, err
	}

	f := &federation{
		link: ml, gov: gov, engine: engine,
		inbox: inbox, outbox: outbox, gstore: gstore, st: st, ctx: ctx,
		clk: e.clock, log: log,
	}
	// Load the caps once before the first tick, so a restart does not leave
	// every area uncapped for a whole tick interval.
	f.refreshAreaShares()

	go f.run(ctx)
	return f, nil
}

// federationOutbox builds the outbox a running node federates through.
//
// Separate from startFederation, which otherwise assembles everything inline,
// because this is the one piece whose configuration is a POLICY decision rather
// than a wiring detail: the classifier below is what makes §7.6's priority
// order and §8.3's Part 97 refusal real on air. A test can build the genuine
// article over a fake link and check that they are, which is exactly what was
// missing when the classifier was left unset.
func federationOutbox(self identity.NodeID, sender bsmp.Sender, dict *bundle.Dictionary,
	kinds areaKinds,
	allowEncryptedDMs func() bool, log *slog.Logger) (*bsmp.Outbox, error) {
	return bsmp.NewOutbox(bsmp.Config{
		Self:              self,
		Link:              sender,
		Dictionary:        dict,
		Classify:          classifierFor(kinds),
		AllowEncryptedDMs: allowEncryptedDMs,
		OnRefusedDM: func(area record.AreaTag) {
			log.Warn("mail held back", "area", area,
				"reason", "this node transmits under an amateur licence (FCC Part 97)")
		},
	})
}

// classifyArea maps an area tag to its governor priority class (§7.6).
//
// This is the production policy, and it has to be set explicitly: the outbox
// defaults to treating everything as forum traffic, which is a safe middle for
// a test but wrong for a running node in two ways. The roster loses the
// priority that keeps a congested mesh converging — starving digests and NODE
// records does not save airtime, it wastes it, because peers that cannot
// reconcile ask again. And nothing is ever classified ClassDM, which silently
// disables §8.3's Part 97 refusal: the gate is written and tested, but a
// default classifier means it can never fire on air.
//
// Mail does not federate yet (record.DMArea), so today the DM arm is a policy
// that is in place before the traffic is, rather than dead code — it is what
// makes the ham-mode block correct the moment mail does go on the wire.
// areaKinds is what the classifier needs to know about an area beyond its tag.
//
// An interface rather than one lookup function per kind, because there are
// three kinds now and the next one would be a fourth parameter threaded through
// federationOutbox to every caller. *store.GossipStore satisfies it already:
// both methods read the cache it refreshes whenever the area list changes, so
// an area created while the node is running gets the right price without a
// restart.
type areaKinds interface {
	IsFileArea(record.AreaTag) bool
	IsDoorArea(record.AreaTag) bool
}

// classifierFor builds the production classifier over an area-kind lookup.
//
// An area tag is a hash of a name and says nothing about what the area holds,
// so neither the file-catalog arm nor the door arm can be a pure function of
// the tag — both have to ask something that tracks the areas.
//
// A nil lookup means "message areas only", which is what a caller with no store
// wants. It under-prices catalog and door traffic and never misprices anything
// else: forum is the safe middle, and the two arms below it are throughput
// questions where the roster and mail arms are correctness ones.
func classifierFor(kinds areaKinds) bsmp.Classifier {
	return func(area record.AreaTag) governor.Class {
		switch area {
		case store.RosterArea:
			return governor.ClassControl
		case record.DMArea:
			return governor.ClassDM
		}
		if kinds != nil {
			// §7.5: a file area's records are catalog entries, and §7.6 puts
			// them near the bottom of the priority order — below forum posts,
			// because a file listing that arrives an hour late costs nobody
			// anything, and under backpressure classes drop from the bottom.
			if kinds.IsFileArea(area) {
				return governor.ClassFileCatalog
			}
			// And a door league is below even that. See ClassDoorEvent: a
			// stale catalog entry is still true, a stale battle report is not.
			if kinds.IsDoorArea(area) {
				return governor.ClassDoorEvent
			}
		}
		// Forums, and the user directory with them.
		return governor.ClassForum
	}
}

// tickInterval is how often the engine gets a chance to act.
//
// It is not the digest interval — the engine's own scheduler decides when to
// speak, and on a fifty-node mesh that works out at hours (§7.3). This is only
// how often it is asked.
const tickInterval = 5 * time.Second

// telemetryInterval is how often the governor is told what the channel looks
// like. The radio refreshes its own metrics far more slowly than this, so
// asking more often would only sample the same reading again.
const telemetryInterval = time.Minute

// statusInterval is how often federation activity is summarised to the log.
//
// This exists because a mesh federation is silent by design — §7.3's digest
// beat is half an hour at two peers and hours at fifty — so "nothing in the log"
// is indistinguishable from "nothing working". Eighteen hours of a two-node test
// produced no output at all and no convergence, and there was no way to tell
// which stage had stalled. A periodic line makes the difference visible without
// waiting for a failure to announce itself.
const statusInterval = 5 * time.Minute

// run pumps datagrams into the engine and ticks it.
//
// The timers come from the injected clock rather than time.NewTicker, per
// §12.1. That constraint is usually about the simulator being replayable, but
// it earns its keep here too: a test can drive this loop through a day of
// federation without waiting for one.
// refreshAreaShares reloads the per-area caps into the governor (§6.3).
//
// A read failure leaves the previous shares in place rather than clearing them.
// Clearing would UNCAP every area, so a transient database error would turn a
// budget the sysop set into no budget at all — the one direction this must
// never fail in.
func (f *federation) refreshAreaShares() {
	shares, err := f.st.AreaShares(f.ctx)
	if err != nil {
		f.log.Warn("refreshing area airtime shares", "err", err)
		return
	}
	raw := make(map[[4]byte]float64, len(shares))
	for tag, share := range shares {
		raw[tag] = share
	}
	f.gov.SetAreaShares(raw)
}

func (f *federation) run(ctx context.Context) {
	tick := f.clk.After(tickInterval)
	telemetry := f.clk.After(telemetryInterval)
	status := f.clk.After(statusInterval)

	for {
		select {
		case <-ctx.Done():
			return

		case dg, ok := <-f.link.Recv():
			if !ok {
				return
			}
			if err := f.inbox.Deliver(dg); err != nil {
				// An error here is OUR state — storage or verification — not a
				// peer's malformed frame, which the inbox counts instead.
				f.log.Error("applying records", "from", dg.From.Short(), "err", err)
			}

		case <-tick:
			tick = f.clk.After(tickInterval)
			// Areas can be created while the BBS runs, and a sysop who marks
			// one federated should not have to restart to see it sync.
			if err := f.gstore.Refresh(); err != nil {
				f.log.Warn("refreshing federated areas", "err", err)
			}
			// Shares travel with the area list for the same reason: §6.3 puts
			// airtime_share on the area row precisely so a sysop can re-tune a
			// noisy league without a restart, and a cap that needed one is a
			// cap nobody would reach for while the mesh was actually busy.
			f.refreshAreaShares()
			if err := f.engine.Tick(); err != nil {
				f.log.Error("sync engine", "err", err)
			}

		case <-telemetry:
			telemetry = f.clk.After(telemetryInterval)
			if info := f.link.Radio(); info != nil && info.Metrics != nil {
				f.gov.Observe(float64(info.Metrics.ChannelUtilization),
					float64(info.Metrics.AirUtilTx))
			}

		case <-status:
			status = f.clk.After(statusInterval)
			f.logStatus()
		}
	}
}

// logStatus summarises what federation has done lately.
//
// Every counter here answers a question a sysop actually asks when posts are
// not arriving, in the order the traffic flows: are we speaking (digests,
// symbols), is anyone answering (heard, requests), is anything landing
// (bundles, records), and if not, who refused (budget, quota, unattributed).
//
// The three rx refusals are split because they point at different layers, and
// a run that stalled for hours is the reason they are here at all: every
// counter on that line read zero or healthy while the radios quietly discarded
// half the protocol. rx_undecryptable in particular is not a federation
// problem — it means the radio could not decrypt what arrived, so the place to
// look is the radios' keys, not anything above them.
func (f *federation) logStatus() {
	eng := f.engine.Stats()
	out := f.outbox.Stats()
	in := f.inbox.Stats()
	lnk := f.link.Stats()
	gov := f.gov.Stats()
	budget := f.gov.Budget()

	f.log.Info("federation status",
		"peers", f.engine.PeerCount(),
		"areas", len(f.gstore.Areas()),
		"digest_areas", len(f.engine.Digest().Areas),
		"next_digest_in", f.engine.NextDue().Sub(f.clk.Now()).Round(time.Second),
		"bound", lnk.PeersKnown,
		"digests_sent", eng.DigestsSent,
		"digests_heard", eng.DigestsHeard,
		"digests_suppressed", eng.DigestsSuppressed,
		"vector_reqs", eng.VectorReqsSent,
		"range_reqs", eng.RangeReqsSent,
		"records_pushed", eng.RecordsPushed,
		"symbols_sent", out.SymbolsSent,
		"bundles_decoded", in.Decoded,
		"records_added", in.RecordsAdded,
		"loss_estimate", fmt.Sprintf("%.0f%%", f.engine.LossEstimate()*100),
		"bundles_observed", eng.BundlesObserved,
		"bundles_missed", eng.BundlesMissed,
		"rx_symbols", in.Symbols,
		"rx_duplicates", in.Duplicates,
		"rx_evicted", in.Evicted,
		"rx_rejected", in.Rejected,
		"rx_corrupt", in.Corrupt,
		"rx_undecryptable", lnk.Undecryptable,
		"rx_wrong_channel", lnk.WrongChannel,
		"since_rx", lnk.SinceRx.Round(time.Second),
		"rx_stalls", lnk.RxStalls,
		"serial_skipped", lnk.SerialSkipped,
		"serial_frames", lnk.SerialFrames,
		"unattributed", lnk.Unattributed,
		"tx_refused_budget", gov.RefusedBudget,
		"tx_refused_busy", gov.RefusedBusy,
		"tx_refused_quiet", gov.RefusedQuiet,
		"budget_available", budget.Available.Round(time.Second),
		"backpressure", budget.Backpressure,
	)
}

// Close shuts the mesh side down.
func (f *federation) Close() error { return f.link.Close() }

// Summary is what `serve` prints at startup, in the terms §7.6 requires: a
// sysop cannot act on "0.1% of channel time", but can act on "11 packets a day".
func (f *federation) Summary() string { return f.gov.Explain() }

// rxTimeout converts the configured seconds into a link setting.
//
// Zero means the sysop turned the watchdog off, and must reach the link as a
// negative rather than a zero: zero is what an unset Config field looks like,
// and the link fills those in with its default. Disabling has to be sayable.
func rxTimeout(secs int) time.Duration {
	if secs == 0 {
		return -1
	}
	return time.Duration(secs) * time.Second
}

// dialerFor builds the radio dialer described by config.
func dialerFor(cfg config.Mesh, deviceLog func(string)) meshlink.Dialer {
	return func(ctx context.Context) (*meshtastic.Conn, error) {
		switch cfg.Mode {
		case "tcp":
			return meshtastic.DialTCP(ctx, meshtastic.TCPConfig{
				Host: cfg.TCPHost, OnDeviceLog: deviceLog,
			})
		case "serial":
			return meshtastic.DialSerial(meshtastic.SerialConfig{
				Port: cfg.SerialDevice, Baud: cfg.SerialBaud, OnDeviceLog: deviceLog,
			})
		default:
			// auto: a cable if there is one, otherwise the network. Serial
			// first because §7.1 recommends it — it does not depend on the
			// node's WiFi staying up.
			conn, err := meshtastic.DialSerial(meshtastic.SerialConfig{
				Port: cfg.SerialDevice, Baud: cfg.SerialBaud, OnDeviceLog: deviceLog,
			})
			if err == nil {
				return conn, nil
			}
			if cfg.TCPHost == "" {
				return nil, fmt.Errorf("no serial radio found and no mesh.tcp_host configured: %w", err)
			}
			return meshtastic.DialTCP(ctx, meshtastic.TCPConfig{
				Host: cfg.TCPHost, OnDeviceLog: deviceLog,
			})
		}
	}
}

// presetFrom turns the radio's report into airtime parameters.
func presetFrom(info *meshtastic.RadioInfo) (airtime.Preset, error) {
	if !info.UsePreset {
		// A sysop who hand-set the modem gets their own numbers, not a preset's.
		return airtime.Custom(int(info.Bandwidth)*1000, int(info.SpreadFactor), int(info.CodingRate))
	}
	return airtime.PresetByName(info.ModemPreset)
}

// federationDictionaries builds the compression dictionary set (§7.4).
//
// The dictionary is a wire-format constant: two nodes with different
// dictionaries cannot read each other's bundles, so this is deliberately not
// configurable and will be frozen with the rest of the format in Phase 6.
func federationDictionaries() (*bundle.Dictionary, *bundle.DictionarySet, error) {
	d, err := bundle.NewRawDictionary(0, []byte(
		"subject: re: from wrote posted meshbbs node area post reply thread "+
			"http:// https:// the and that with have this for you are not "))
	if err != nil {
		return nil, nil, err
	}
	set, err := bundle.NewDictionarySet(d)
	if err != nil {
		return nil, nil, err
	}
	return d, set, nil
}
