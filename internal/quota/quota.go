// Package quota provides per-key token buckets and concurrency limits.
package quota

import (
	"context"
	"math"
	"sync"
	"time"
)

// Limiter is a set of token buckets, one per key.
type Limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64
	max       int           // most keys held at once (0: unbounded)
	idle      time.Duration // a bucket unused this long is dropped
	now       func() time.Time
	buckets   map[string]*bucket
	periodic  bool // unbounded API key sets sweep on a timer, bounded maps sweep when full
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Decision is what Take answered.
type Decision struct {
	OK bool
	// RetryAfter is the whole seconds until a token is back (>= 1) when refused
	// for an empty bucket.
	RetryAfter int
	// Full is a refusal because the map holds max active buckets: the caller
	// answers busy, not rate limited.
	Full bool
}

// PerMinute returns a limiter refilling perMinute tokens a minute up to burst per
// key, holding at most maxKeys keys (0: no bound) and dropping one idle for
// idle. now is the clock (time.Now when nil).
func PerMinute(perMinute, burst float64, maxKeys int, idle time.Duration, now func() time.Time) *Limiter {
	return newLimiter(perMinute/60, burst, maxKeys, idle, now)
}

const maxIdleTTL = 24 * time.Hour

// PerSecond returns a request limiter with one bucket per address or credential.
// An idle bucket is swept after two refills (including its possible debt),
// clamped to a minute through a day even for very small configured rates.
func PerSecond(rate, burst float64, now func() time.Time) *Limiter {
	idle := time.Minute
	if rate > 0 {
		secs := min(2*burst/rate, maxIdleTTL.Seconds())
		idle = max(idle, time.Duration(secs*float64(time.Second)))
	}
	l := newLimiter(rate, burst, 0, idle, now)
	l.periodic = true
	return l
}

func newLimiter(rate, burst float64, maxKeys int, idle time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: burst, max: maxKeys, idle: idle, now: now, buckets: map[string]*bucket{}}
}

// bucket returns the refilled bucket under the lock, or nil at the key cap.
func (l *Limiter) bucket(key string) *bucket {
	now := l.now()
	if l.periodic && now.Sub(l.lastSweep) >= l.idle {
		l.sweep(now)
		l.lastSweep = now
	}
	b := l.buckets[key]
	if b == nil {
		if l.max > 0 && len(l.buckets) >= l.max {
			l.sweep(now)
			if len(l.buckets) >= l.max {
				return nil
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	l.refill(b, now)
	return b
}

// Take spends a podcast token, allowing for rounding in per-minute refills.
func (l *Limiter) Take(key string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key)
	if b == nil {
		return Decision{Full: true, RetryAfter: 1}
	}
	if b.spend(1 - eps) {
		return Decision{OK: true}
	}
	return Decision{RetryAfter: retryAfter(b.tokens, l.rate)}
}

// Allow spends one whole token from an API request bucket.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key)
	return b != nil && b.spend(1)
}

func (b *bucket) spend(threshold float64) bool {
	if b.tokens < threshold {
		return false
	}
	b.tokens--
	return true
}

// Ready checks a bucket without reserving a token.
func (l *Limiter) Ready(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key)
	return b != nil && b.tokens >= 1
}

// Wait is the delay until the next token. Bound it before converting to
// nanoseconds, since a configured refill can take longer than a Duration holds.
func (l *Limiter) Wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(key)
	if b == nil {
		return time.Second
	}
	if b.tokens >= 1 || l.rate <= 0 {
		return 0
	}
	secs := min((1-b.tokens)/l.rate, maxIdleTTL.Seconds())
	return time.Duration(secs * float64(time.Second))
}

// Charge pays for work already done after Ready, including concurrent work
// admitted before the first charge. Debt is capped at one burst.
func (l *Limiter) Charge(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.bucket(key); b != nil {
		b.tokens = max(b.tokens-1, -l.burst)
	}
}

// Refund gives back a token Take spent from key's bucket for a request that
// never ran (it waited for a slot and gave up), up to the burst.
func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.buckets[key]; b != nil {
		l.refill(b, l.now())
		b.tokens = math.Min(b.tokens+1, l.burst)
	}
}

// refill adds b's tokens since it was last read, up to the burst.
func (l *Limiter) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(b.tokens+elapsed*l.rate, l.burst)
	}
	b.last = now
}

// eps absorbs float residue in refills (5 + 1 seconds of 1/6 token each).
const eps = 1e-9

// retryAfter is the whole seconds (>= 1) until tokens reaches one at rate,
// ignoring float residue (5.9999999 s is 6, not 7).
func retryAfter(tokens, rate float64) int {
	return int(math.Max(math.Ceil((1-tokens)/rate-eps), 1))
}

// Capped maps release a slot at the idle boundary. The periodic API sweep
// keeps a bucket through the boundary, including a very slow configured refill.
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if age := now.Sub(b.last); age > l.idle || (!l.periodic && age == l.idle) {
			delete(l.buckets, k)
		}
	}
}

// Slots is a counting semaphore: TryAcquire takes a slot or reports that all
// are busy, Acquire waits for one.
type Slots chan struct{}

// NewSlots returns n slots.
func NewSlots(n int) Slots { return make(Slots, n) }

// TryAcquire takes a slot when one is free.
func (s Slots) TryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

// Acquire waits for a slot until ctx ends, and reports whether it took one.
func (s Slots) Acquire(ctx context.Context) bool {
	if s.TryAcquire() {
		return true
	}
	select {
	case s <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// Busy reports whether every slot is taken right now (a hint: one may free
// or fill a moment later, so TryAcquire still decides).
func (s Slots) Busy() bool { return len(s) == cap(s) }

// Release returns a slot TryAcquire or Acquire took.
func (s Slots) Release() { <-s }
