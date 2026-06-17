package api

import (
	"sync"
	"time"
)

// limiter implements a simple per-key failure lockout: after maxFailures within
// the window a key (IP) is blocked until the window elapses since its last
// failure. Used to throttle brute force on login and auth-code redemption.
type limiter struct {
	mu          sync.Mutex
	maxFailures int
	window      time.Duration
	entries     map[string]*lockEntry
	now         func() time.Time
}

type lockEntry struct {
	failures int
	until    time.Time
	last     time.Time
}

func newLimiter(maxFailures int, window time.Duration) *limiter {
	return &limiter{
		maxFailures: maxFailures,
		window:      window,
		entries:     map[string]*lockEntry{},
		now:         time.Now,
	}
}

// Allowed reports whether key is currently permitted to attempt.
func (l *limiter) Allowed(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true
	}
	now := l.now()
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	// Reset stale entries.
	if now.Sub(e.last) > l.window {
		delete(l.entries, key)
	}
	return true
}

// Fail records a failed attempt and locks the key once over the threshold.
func (l *limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil || now.Sub(e.last) > l.window {
		e = &lockEntry{}
		l.entries[key] = e
	}
	e.failures++
	e.last = now
	if e.failures >= l.maxFailures {
		e.until = now.Add(l.window)
	}
}

// Reset clears a key's failures (called on a successful attempt).
func (l *limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// ipRateLimiter is a per-IP token-bucket limiter for overall request rate.
type ipRateLimiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64 // bucket capacity
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(ratePerSec, burst float64) *ipRateLimiter {
	return &ipRateLimiter{
		rate:    ratePerSec,
		burst:   burst,
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// Allow consumes one token for ip, refilling based on elapsed time.
func (r *ipRateLimiter) Allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	b := r.buckets[ip]
	if b == nil {
		r.buckets[ip] = &bucket{tokens: r.burst - 1, last: now}
		return true
	}
	b.tokens += now.Sub(b.last).Seconds() * r.rate
	if b.tokens > r.burst {
		b.tokens = r.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
