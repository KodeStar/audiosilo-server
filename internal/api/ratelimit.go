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
	lastSweep   time.Time
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

// Acquire atomically records an attempt and reports whether it was permitted
// (i.e. the key was not already locked out). Unlike a separate Allowed()+Fail()
// it cannot race, so every admitted attempt counts toward the cap — suitable for
// gating account-creating endpoints (demo sessions) where partial failures and
// concurrent requests must both be metered.
func (l *limiter) Acquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	e := l.entries[key]
	if e == nil || now.Sub(e.last) > l.window {
		e = &lockEntry{}
		l.entries[key] = e
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false // already locked out
	}
	e.failures++
	e.last = now
	if e.failures >= l.maxFailures {
		e.until = now.Add(l.window)
	}
	return true
}

// Fail records a failed attempt and locks the key once over the threshold.
func (l *limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
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

// sweep drops stale, unlocked entries so a flood of distinct keys can't grow the
// map without bound. It runs at most once per window (caller holds the lock).
func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for k, e := range l.entries {
		if now.Before(e.until) {
			continue // still locked out — keep
		}
		if now.Sub(e.last) > l.window {
			delete(l.entries, k)
		}
	}
}

// ipRateLimiter is a per-IP token-bucket limiter for overall request rate.
type ipRateLimiter struct {
	mu        sync.Mutex
	rate      float64 // tokens per second
	burst     float64 // bucket capacity
	buckets   map[string]*bucket
	idleTTL   time.Duration // drop buckets idle longer than this (they've refilled to full)
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newIPRateLimiter(ratePerSec, burst float64) *ipRateLimiter {
	// A bucket idle long enough to refill to full is equivalent to a fresh one,
	// so it can be evicted. Use the refill time (with margin), floored at 1 min.
	idle := time.Minute
	if ratePerSec > 0 {
		if refill := time.Duration(burst / ratePerSec * 2 * float64(time.Second)); refill > idle {
			idle = refill
		}
	}
	return &ipRateLimiter{
		rate:    ratePerSec,
		burst:   burst,
		buckets: map[string]*bucket{},
		idleTTL: idle,
		now:     time.Now,
	}
}

// sweep drops buckets idle long enough to have refilled to full, bounding memory
// under a flood of distinct IPs. Runs at most once per idleTTL (caller holds lock).
func (r *ipRateLimiter) sweep(now time.Time) {
	if now.Sub(r.lastSweep) < r.idleTTL {
		return
	}
	r.lastSweep = now
	for k, b := range r.buckets {
		if now.Sub(b.last) > r.idleTTL {
			delete(r.buckets, k)
		}
	}
}

// Allow consumes one token for ip, refilling based on elapsed time.
func (r *ipRateLimiter) Allow(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.sweep(now)
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
