package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/quota"
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

// allowAttempt reports whether l admits key's attempt (Allowed); when it doesn't
// it writes the shared 429 (tooManyAttempts) and reports false, so every
// credential handler gates with one line.
func allowAttempt(w http.ResponseWriter, l *limiter, key string) bool {
	if l.Allowed(key) {
		return true
	}
	tooManyAttempts(w, l.LockedFor(key))
	return false
}

// tooManyAttempts writes the shared credential 429 with Retry-After, the time
// until the lockout ends, so a client can say when to try again.
func tooManyAttempts(w http.ResponseWriter, lockedFor time.Duration) {
	setRetryAfter(w, lockedFor)
	writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
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
// it cannot race, so every admitted attempt counts toward the cap - suitable for
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

// LockedFor reports how long key stays locked out (0 when it is not).
func (l *limiter) LockedFor(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return 0
	}
	return max(e.until.Sub(l.now()), 0)
}

// setRetryAfter sets Retry-After to d in whole seconds, rounded up (at least 1).
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.FormatInt(retrySeconds(d), 10))
}

// retrySeconds is d in whole seconds for a retry hint, rounded up, at least 1.
func retrySeconds(d time.Duration) int64 {
	return max(int64((d+time.Second-1)/time.Second), 1)
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
			continue // still locked out - keep
		}
		if now.Sub(e.last) > l.window {
			delete(l.entries, k)
		}
	}
}

// initRateLimits builds the independent address and credential budgets before serving.
func (a *API) initRateLimits(now func() time.Time) {
	limits := a.boot.RateLimit.Effective()
	bucket := func(b config.RateBucket) *quota.Limiter { return quota.PerSecond(b.Rate, float64(b.Burst), now) }
	a.ipLimiter = bucket(limits.Address)
	a.tokenLimiter = bucket(limits.Token)
	a.mediaLimiter = bucket(limits.Media)
	a.bridgeLimiter = quota.PerSecond(bridgeRate, bridgeBurst, now)
}
