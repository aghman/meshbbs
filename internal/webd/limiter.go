package webd

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aghman/meshbbs/internal/clock"
)

// Rate limiting for the unauthenticated endpoints (webui.md §10).
//
// # What this is actually for
//
// Not for making a 64-bit enrolment code unguessable — it already is, and no
// rate limit would rescue it if it were not. This is for two narrower things:
//
//  1. The anti-oracle argument in [D18] assumes each guess COSTS the guesser
//     something. Spending the code at ceremony start is most of that; bounding
//     the attempt rate is the rest.
//  2. Every attempt is a database lookup and a hash. Unauthenticated endpoints
//     that do real work need a ceiling, or the cheapest possible request from
//     an attacker becomes the most expensive one for the server.

// bucket is one key's token allowance.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is a token bucket per client.
//
// A full hour's allowance is available as a burst and refills over that hour,
// which is the behaviour a person expects: mistyping a code three times in a
// row is fine, and a script that keeps going runs dry.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	clock   clock.Clock

	capacity float64 // tokens
	refill   float64 // tokens per second
}

func newLimiter(clk clock.Clock, perHour int) *limiter {
	if perHour < 1 {
		perHour = 1
	}
	return &limiter{
		buckets:  map[string]*bucket{},
		clock:    clk,
		capacity: float64(perHour),
		refill:   float64(perHour) / 3600,
	}
}

// allow spends a token, reporting how long to wait if there are none.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	b, ok := l.buckets[key]
	if !ok {
		l.sweepLocked(now)
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	}

	// Refill for the elapsed time, capped at the burst size.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(l.capacity, b.tokens+elapsed*l.refill)
		b.last = now
	}

	if b.tokens < 1 {
		// How long until one token is available again.
		wait := time.Duration((1-b.tokens)/l.refill) * time.Second
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweepLocked drops buckets that have refilled completely, since a full bucket
// is indistinguishable from a client that has never been seen.
//
// Run on insertion rather than on a timer: the map only grows when a new client
// appears, so that is exactly when it is worth tidying, and a background
// goroutine here would be one more thing whose failure is silent.
func (l *limiter) sweepLocked(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	full := time.Duration(l.capacity/l.refill) * time.Second
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
}

// clientKey identifies a caller for rate-limiting purposes.
//
// # How X-Forwarded-For is handled
//
// A client sets that header, so it is worthless on its own: honouring it
// unconditionally means an attacker sends a fresh value per request and the
// limiter counts each separately — worse than no limiter, because it looks like
// protection and provides none.
//
// It is therefore consulted ONLY when the request arrived from an address the
// sysop listed in web.trusted_proxies, and then read RIGHT TO LEFT, skipping
// trusted hops. The rightmost entry is the one the nearest proxy observed and
// is the only one it could vouch for; everything to its left was supplied by
// whoever came before, up to and including the client itself. Taking the
// leftmost — the common mistake — hands an attacker the header back, since they
// choose what it starts with.
//
// With no trusted proxies configured (the default) the header is ignored
// entirely and every request is attributed to its transport address.
func clientKey(r *http.Request, trusted []*net.IPNet) string {
	remote := hostOf(r.RemoteAddr)
	if len(trusted) == 0 || !ipInAny(remote, trusted) {
		return remote
	}

	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) == 0 {
		return remote
	}

	// Flatten, since the header may appear several times as well as
	// comma-separated within one line.
	var hops []string
	for _, line := range forwarded {
		for _, h := range strings.Split(line, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}

	for i := len(hops) - 1; i >= 0; i-- {
		ip := hostOf(hops[i])
		if net.ParseIP(ip) == nil {
			// A garbage hop means the chain cannot be trusted past this point.
			break
		}
		if !ipInAny(ip, trusted) {
			return ip
		}
	}
	// Every hop was a trusted proxy, so the proxy itself is the best answer.
	return remote
}

// hostOf strips a port if there is one. Header entries usually have none;
// RemoteAddr always does.
func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return strings.Trim(addr, "[]")
}

func ipInAny(ip string, ranges []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range ranges {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// globalLimitFactor scales the per-client rate into an instance-wide ceiling.
//
// Deliberately generous. A global cap is itself a denial-of-service lever — one
// attacker can spend it and lock everyone out — so it is set high enough that
// only bulk automated guessing reaches it, and never so low that a busy evening
// on a shared network does.
//
// It still matters with trusted proxies configured, because a misconfigured or
// absent allow list silently collapses every request onto one key, and this is
// the bound that holds when that happens.
const globalLimitFactor = 20

// rateLimited reports whether a request should be refused, and writes the
// refusal if so.
func (s *Server) rateLimited(w http.ResponseWriter, r *http.Request,
	per *limiter, global *limiter, what string) bool {
	if ok, wait := per.allow(clientKey(r, s.opts.TrustedProxies)); !ok {
		s.refuse(w, r, wait, what, "client")
		return true
	}
	if ok, wait := global.allow(""); !ok {
		s.refuse(w, r, wait, what, "instance")
		return true
	}
	return false
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request,
	wait time.Duration, what, scope string) {
	s.log.Warn("rate limit reached",
		"endpoint", what, "scope", scope, "remote", r.RemoteAddr, "retry_after", wait)

	secs := int(wait.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", itoa(secs))
	// Say how long, because "too many attempts" with no number is the kind of
	// message that makes someone hammer reload.
	httpError(w, http.StatusTooManyRequests,
		"too many attempts — wait "+humaniseWait(wait)+" and try again")
}

func humaniseWait(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "a moment"
	case d < 2*time.Minute:
		return "a minute"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + " minutes"
	default:
		return "an hour"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
