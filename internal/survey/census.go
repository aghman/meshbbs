package survey

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Census is what the survey learned by listening, as opposed to by measuring.
//
// §7.8.3 asks for this alongside R because R alone does not tell a sysop
// whether their node is well placed. Two nodes with the same R are in very
// different situations if one has eleven direct neighbours and the other has
// one.
type Census struct {
	// Packets heard in total, after firmware deduplication.
	Packets int
	// Neighbours are the radios heard from, most recently first.
	Neighbours []Neighbour
	// DirectCount is how many were heard with no hops.
	DirectCount int
	// RelayedCount is how many arrived via at least one hop.
	RelayedCount int
	// UnknownHops counts packets from firmware too old to report hop_start.
	UnknownHops int
	// ByPortnum counts what the traffic on this mesh actually is, which is
	// useful context for a sysop about to add BBS traffic to it.
	ByPortnum map[string]int
	// Busiest is the highest packets-per-minute seen, a hint at burstiness that
	// a mean hides.
	PeakPerMinute int
}

// Neighbour is one radio we heard.
type Neighbour struct {
	Radio    uint32
	Packets  int
	BestSNR  float32
	WorstSNR float32
	// MinHops is the fewest hops any packet from it took. Zero means direct.
	MinHops  int
	KnownHop bool
	LastAt   time.Time
}

type census struct {
	mu      sync.Mutex
	packets int
	byRadio map[uint32]*Neighbour
	byPort  map[string]int
	perMin  map[int64]int
	direct  int
	relayed int
	unknown int
}

func newCensus() *census {
	return &census{
		byRadio: map[uint32]*Neighbour{},
		byPort:  map[string]int{},
		perMin:  map[int64]int{},
	}
}

func (c *census) add(h Heard) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.packets++
	c.byPort[h.Portnum]++
	c.perMin[h.At.Unix()/60]++

	hops, known := h.Hops()
	switch {
	case !known:
		c.unknown++
	case hops == 0:
		c.direct++
	default:
		c.relayed++
	}

	n := c.byRadio[h.From]
	if n == nil {
		n = &Neighbour{Radio: h.From, BestSNR: h.SNR, WorstSNR: h.SNR, MinHops: 99}
		c.byRadio[h.From] = n
	}
	n.Packets++
	n.LastAt = h.At
	if h.SNR > n.BestSNR {
		n.BestSNR = h.SNR
	}
	if h.SNR < n.WorstSNR {
		n.WorstSNR = h.SNR
	}
	if known && hops < n.MinHops {
		n.MinHops, n.KnownHop = hops, true
	}
}

func (c *census) summarise() Census {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := Census{
		Packets:      c.packets,
		DirectCount:  c.direct,
		RelayedCount: c.relayed,
		UnknownHops:  c.unknown,
		ByPortnum:    map[string]int{},
	}
	for k, v := range c.byPort {
		out.ByPortnum[k] = v
	}
	for _, n := range c.byRadio {
		out.Neighbours = append(out.Neighbours, *n)
	}
	// Sorted by traffic then radio number: a report that reorders itself
	// between runs is a report nobody can diff.
	sort.Slice(out.Neighbours, func(i, j int) bool {
		if out.Neighbours[i].Packets != out.Neighbours[j].Packets {
			return out.Neighbours[i].Packets > out.Neighbours[j].Packets
		}
		return out.Neighbours[i].Radio < out.Neighbours[j].Radio
	})
	for _, v := range c.perMin {
		if v > out.PeakPerMinute {
			out.PeakPerMinute = v
		}
	}
	return out
}

// collect drains the census feed until the returned function is called.
func collect(ctx context.Context, feed <-chan Heard, c *census) func() {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case h, ok := <-feed:
				if !ok {
					return
				}
				c.add(h)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
