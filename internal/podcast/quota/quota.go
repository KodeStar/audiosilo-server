// Package quota is the token buckets podcast discovery and OPML transfer
// spend from: per user (a bounded map of buckets) and server wide (one key).
// It is memory only, with an injected clock, and never evicts a bucket that
// is still refilling: a full map refuses a new key instead.
package quota

import (
	"context"
	"math"
	"sync"
	"time"
)

// Limiter is a set of token buckets, one per key.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	max     int           // most keys held at once (0: unbounded, for a fixed key set)
	idle    time.Duration // a bucket unused this long is dropped
	now     func() time.Time
	buckets map[string]*bucket
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

// New returns a limiter refilling perMinute tokens a minute up to burst per
// key, holding at most maxKeys keys (0: no bound) and dropping one idle for
// idle. now is the clock (time.Now when nil).
func New(perMinute, burst float64, maxKeys int, idle time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: perMinute / 60, burst: burst, max: maxKeys, idle: idle, now: now, buckets: map[string]*bucket{}}
}

// Take spends one token of key's bucket.
func (l *Limiter) Take(key string) Decision {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if l.max > 0 && len(l.buckets) >= l.max {
			l.sweep(now)
			if len(l.buckets) >= l.max {
				return Decision{Full: true, RetryAfter: 1}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	l.refill(b, now)
	if b.tokens >= 1-eps {
		b.tokens--
		return Decision{OK: true}
	}
	return Decision{RetryAfter: retryAfter(b.tokens, l.rate)}
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

// sweep drops the buckets unused for idle (refilled long since).
func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) >= l.idle {
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

// Release returns a slot TryAcquire or Acquire took.
func (s Slots) Release() { <-s }
