package webd

import (
	"net"
	"net/http"
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
// # Why X-Forwarded-For is ignored
//
// Because a client sets it. Trusting it means an attacker sends a different
// value on every request and the limiter counts each one separately — which is
// worse than having no limiter, since it looks like protection and provides
// none.
//
// The cost of ignoring it is real and worth stating: behind a TLS-terminating
// reverse proxy every request arrives from the proxy, so the per-client limit
// collapses into a single shared bucket. That is why globalLimitFactor exists —
// a deployment where per-IP is meaningless still has a ceiling. A proper fix is
// a trusted-proxy list, which is a config surface this does not need yet.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// globalLimitFactor scales the per-client rate into an instance-wide ceiling.
//
// Deliberately generous. A global cap is itself a denial-of-service lever — one
// attacker can spend it and lock everyone out — so it is set high enough that
// only bulk automated guessing reaches it, and never so low that a busy evening
// on a shared network does.
const globalLimitFactor = 20

// rateLimited reports whether a request should be refused, and writes the
// refusal if so.
func (s *Server) rateLimited(w http.ResponseWriter, r *http.Request,
	per *limiter, global *limiter, what string) bool {
	if ok, wait := per.allow(clientKey(r)); !ok {
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
