package survey

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/aghman/meshbbs/internal/airtime"
)

// MeshCeilingPercent is the share of the channel the whole BBS network should
// consume (§7.6). It is a ceiling on the federation, not on one node.
const MeshCeilingPercent = 5.0

// DefaultInstanceCount is the network size the design plans for `[D2]`.
const DefaultInstanceCount = 50

// Budget converts an R estimate into what a sysop actually needs to know.
//
// §7.8.3 is emphatic that the number alone is not the deliverable: "at R = 5.2
// and a 5% mesh ceiling shared 50 ways, your instance can originate about 8
// full packets/day, roughly 20 short posts". A sysop cannot act on "R is 5.2".
// They can act on "eight posts a day".
type Budget struct {
	R              float64
	Instances      int
	CeilingPercent float64
	// ChannelSecondsPerDay is this instance's share of the commons.
	ChannelSecondsPerDay float64
	FullPackets          float64
	ShortPosts           float64
}

// DeriveBudget computes one instance's allowance at a measured R.
func DeriveBudget(p airtime.Preset, r float64, instances int, ceiling float64) Budget {
	if instances < 1 {
		instances = 1
	}
	if r < 1 {
		r = 1
	}
	perDay := 86400 * (ceiling / 100) / float64(instances)

	full := p.Cost(233, r)
	short := p.Cost(100, r)

	return Budget{
		R:                    r,
		Instances:            instances,
		CeilingPercent:       ceiling,
		ChannelSecondsPerDay: perDay,
		FullPackets:          perDay / full.Seconds(),
		ShortPosts:           perDay / short.Seconds(),
	}
}

// Write renders the report as the shareable file §7.8.3 asks for.
//
// Plain text on purpose: the point is that sysops send these to each other and
// the project aggregates them into a distribution that eventually replaces the
// guessed default of 4. That works if the format is readable in a terminal, an
// email and a forum post.
func (r *Report) Write(w io.Writer, instances int) {
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }

	p("meshbbs mesh survey\n")
	p("===================\n\n")
	p("Started   %s\n", r.Started.UTC().Format(time.RFC3339))
	p("Ended     %s\n", r.Ended.UTC().Format(time.RFC3339))
	p("Duration  %s\n", r.Ended.Sub(r.Started).Round(time.Second))
	p("Radio     %s", r.Preset)
	if r.Region != "" {
		p(", region %s", r.Region)
	}
	p("\n\n")

	p("Baseline (transmitting nothing)\n")
	p("  channel busy   %.2f%% (sd %.2f, %d fresh samples, %d stale reads)\n",
		r.Baseline.MeanChannel(), r.Baseline.SDChannel(),
		len(r.Baseline.Samples), r.Baseline.Stale)
	p("  duration       %s\n", r.Baseline.Ended.Sub(r.Baseline.Started).Round(time.Second))
	if drift, ok := r.Drift(); ok {
		// Printed next to the opening baseline rather than at the end, because
		// it is the same measurement and a reader comparing the two should not
		// have to scroll. The sign is the useful part: positive means the mesh
		// got busier while we were transmitting, and R is over-stated.
		raw, _ := r.DriftRaw()
		p("  closing        %.2f%% (sd %.2f, %d fresh samples)\n",
			r.BaselineEnd.MeanChannel(), r.BaselineEnd.SDChannel(), len(r.BaselineEnd.Samples))
		p("  ambient drift  %+.2f to %+.2f points over the run\n", drift, raw)
		p("                 (the range is our own transmit average still decaying;\n")
		p("                  a wide one means the closing baseline started too soon)\n")
	} else {
		p("  closing        not measured — drift over the run is unknown\n")
	}
	p("\n")

	p("Hop limit sweep\n")
	p("  %-5s %-10s %-10s %-16s %s\n", "hop", "sent", "our tx", "R", "note")
	for i, e := range r.Estimates {
		var sent int
		if i < len(r.Loads) {
			sent = r.Loads[i].Sent
		}
		rng := fmt.Sprintf("%.1f (%.1f-%.1f)", e.R, e.Low, e.High)
		if !e.Confident {
			rng = "≤ " + fmt.Sprintf("%.1f", e.High)
		}
		p("  %-5d %-10d %-10.3f %-16s %s\n", e.HopLimit, sent, e.OurTransmit, rng, e.Explain)
	}
	p("\n")

	best, ok := r.Best()
	if ok {
		b := DeriveBudget(r.Preset, best.R, instances, MeshCeilingPercent)
		p("What this means for your instance\n")
		p("  R ≈ %.1f at hop limit %d\n", best.R, best.HopLimit)
		p("  At a %.0f%% mesh-wide ceiling shared %d ways, this node's share is\n",
			b.CeilingPercent, b.Instances)
		p("  %.0f seconds of channel time per day — about %.0f full packets,\n",
			b.ChannelSecondsPerDay, b.FullPackets)
		p("  or roughly %.0f short forum posts.\n\n", b.ShortPosts)
		if len(r.Estimates) > 1 {
			p("  Hop limit is the lever: %s\n\n", hopAdvice(r))
		}
	} else {
		p("What this means for your instance\n")
		p("  No usable R was measured. The load did not rise above the channel's\n")
		p("  own noise, so any number here would be invented. Re-run when the mesh\n")
		p("  is quieter, or with a longer load phase.\n\n")
	}

	p("What this node heard\n")
	p("  packets        %d (%d direct, %d relayed, %d hop count unknown)\n",
		r.Census.Packets, r.Census.DirectCount, r.Census.RelayedCount, r.Census.UnknownHops)
	p("  peak           %d packets in one minute\n", r.Census.PeakPerMinute)
	p("  neighbours     %d radios heard\n", len(r.Census.Neighbours))
	for _, n := range r.Census.Neighbours {
		hops := "?"
		if n.KnownHop {
			hops = fmt.Sprint(n.MinHops)
		}
		p("    0x%08x  %4d pkts  snr %.1f..%.1f  min hops %s\n",
			n.Radio, n.Packets, n.WorstSNR, n.BestSNR, hops)
	}
	if len(r.Census.ByPortnum) > 0 {
		p("  traffic        ")
		var parts []string
		for k, v := range r.Census.ByPortnum {
			parts = append(parts, fmt.Sprintf("%s=%d", k, v))
		}
		// Sorted so two runs of the same mesh produce diffable reports.
		sortStrings(parts)
		p("%s\n", strings.Join(parts, " "))
	}
	p("\n")

	if len(r.Notes) > 0 {
		p("Notes\n")
		for _, n := range r.Notes {
			p("  - %s\n", n)
		}
		p("\n")
	}

	p("R is a property of THIS node's position in THIS mesh at THIS moment.\n")
	p("A node at the edge sees a different R than one in the middle, and that is\n")
	p("the correct thing to measure: it is your node's traffic being budgeted.\n")
}

// Best returns the estimate at the node's own hop limit if that was surveyed,
// otherwise the most confident one.
func (r *Report) Best() (REstimate, bool) {
	var best REstimate
	var found bool
	for _, e := range r.Estimates {
		if !e.Confident {
			continue
		}
		// Prefer the most conservative confident estimate: under-stating R
		// means over-spending the commons, which is the error that matters.
		if !found || e.R > best.R {
			best, found = e, true
		}
	}
	return best, found
}

func hopAdvice(r *Report) string {
	var lo, hi REstimate
	for _, e := range r.Estimates {
		if !e.Confident {
			continue
		}
		if lo.HopLimit == 0 || e.HopLimit < lo.HopLimit {
			lo = e
		}
		if e.HopLimit > hi.HopLimit {
			hi = e
		}
	}
	if lo.HopLimit == 0 || hi.HopLimit == lo.HopLimit {
		return "not enough confident points to draw the curve"
	}
	return fmt.Sprintf("hop %d costs R≈%.1f, hop %d costs R≈%.1f — %.1fx more airtime for every packet",
		lo.HopLimit, lo.R, hi.HopLimit, hi.R, hi.R/lo.R)
}

// notes records what a reader of the report needs to distrust.
func notes(r *Report) []string {
	var out []string
	if r.Baseline.Stale > len(r.Baseline.Samples) {
		out = append(out, fmt.Sprintf(
			"more stale reads (%d) than fresh samples (%d): the node updates its metrics slowly, so these means rest on few points",
			r.Baseline.Stale, len(r.Baseline.Samples)))
	}
	if r.Census.Packets == 0 {
		out = append(out, "no packets were heard at all — if this mesh has other nodes, this one may not be hearing them")
	}
	if len(r.Census.Neighbours) == 1 {
		out = append(out, "only one other radio was heard: R measured against a two-node mesh is close to meaningless, since there is nobody to rebroadcast")
	}
	for _, e := range r.Estimates {
		if !e.Confident {
			out = append(out, fmt.Sprintf("hop %d produced no usable measurement (%s)", e.HopLimit, e.Explain))
		}
	}
	out = append(out, driftNotes(r)...)
	return out
}

// driftBias is the share of a load phase's measured rise that ambient drift may
// account for before the estimate stops being trustworthy.
//
// A quarter, because the reported interval is already roughly ±20% on a good
// run: a bias of the same order does not widen the answer, it moves it, and an
// interval that excludes the truth is worse than a wide one.
const driftBias = 0.25

// driftNotes reports ambient drift against what each hop actually measured.
//
// Stated per hop rather than once, because the same absolute drift matters
// completely differently at hop 1 and hop 5: a 0.3-point drift against hop 5's
// 4-point rise is noise, and against hop 1's 0.8-point rise it is a quarter of
// the answer.
func driftNotes(r *Report) []string {
	drift, ok := r.Drift()
	if !ok {
		return []string{"no closing baseline: ambient drift over the run is unknown, so R may be biased in either direction by however much this mesh got busier or quieter"}
	}

	var out []string
	for _, e := range r.Estimates {
		if !e.Confident || e.DeltaBusy <= 0 {
			continue
		}
		share := math.Abs(drift) / e.DeltaBusy
		if share < driftBias {
			continue
		}
		direction := "over-stated"
		if drift < 0 {
			direction = "under-stated"
		}
		out = append(out, fmt.Sprintf(
			"the mesh got %s over this run (%+.2f points) and hop %d's rise was only %.2f: "+
				"as much as %.0f%% of that hop's R may be drift rather than rebroadcast, so treat R≈%.1f as %s",
			busier(drift), drift, e.HopLimit, e.DeltaBusy, share*100, e.R, direction))
	}
	return out
}

func busier(drift float64) string {
	if drift < 0 {
		return "quieter"
	}
	return "busier"
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
