package api

import (
	"fmt"
	"testing"
	"time"
)

// limiter is the brute-force lockout behind login + auth-code redemption. The
// injectable now() makes the window deterministic.
func TestLimiterLockoutAndWindowExpiry(t *testing.T) {
	now := time.Now()
	l := newLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	const key = "1.2.3.4"

	l.Fail(key)
	l.Fail(key)
	if !l.Allowed(key) {
		t.Fatal("under the threshold the key should still be allowed")
	}

	l.Fail(key) // third failure trips the lock
	if l.Allowed(key) {
		t.Fatal("at the threshold the key must be locked")
	}

	now = now.Add(2 * time.Minute) // wait out the window
	if !l.Allowed(key) {
		t.Fatal("after the window the key should be allowed again")
	}
}

// Acquire gates the demo-session endpoint: it must meter every admitted attempt
// (so the Nth call within the window is denied) and recover after the window.
func TestLimiterAcquire(t *testing.T) {
	now := time.Now()
	l := newLimiter(5, 15*time.Minute) // mirrors the demoLimiter config
	l.now = func() time.Time { return now }
	const ip = "1.2.3.4"

	for i := 1; i <= 5; i++ {
		if !l.Acquire(ip) {
			t.Fatalf("attempt %d should be admitted (cap is 5)", i)
		}
	}
	if l.Acquire(ip) {
		t.Fatal("the 6th attempt within the window must be denied")
	}

	now = now.Add(16 * time.Minute) // wait out the window
	if !l.Acquire(ip) {
		t.Fatal("after the window a fresh attempt must be admitted again")
	}
}

func TestLimiterReset(t *testing.T) {
	l := newLimiter(2, time.Minute)
	l.Fail("k")
	l.Fail("k")
	if l.Allowed("k") {
		t.Fatal("should be locked after two failures")
	}
	l.Reset("k") // a successful attempt clears the counter
	if !l.Allowed("k") {
		t.Fatal("Reset should clear the lock")
	}
}

// rateLimiter is the token bucket on request rate (per IP, or per credential for media).
func TestRateLimiterBucket(t *testing.T) {
	now := time.Now()
	r := newRateLimiter(10, 2) // 10 tokens/sec, burst of 2
	r.now = func() time.Time { return now }
	const ip = "9.9.9.9"

	if !r.Allow(ip) {
		t.Fatal("first request consumes the initial burst token")
	}
	if !r.Allow(ip) {
		t.Fatal("second request consumes the last burst token")
	}
	if r.Allow(ip) {
		t.Fatal("third request must be denied - bucket empty")
	}

	now = now.Add(100 * time.Millisecond) // refills 0.1s * 10/s = 1 token
	if !r.Allow(ip) {
		t.Fatal("request allowed again after the bucket refills")
	}
}

// Ready reports whether a token is left without spending it: media checks it
// before authenticating, so a throttled address can't run token lookups.
func TestRateLimiterReady(t *testing.T) {
	now := time.Now()
	r := newRateLimiter(10, 2)
	r.now = func() time.Time { return now }
	const ip = "9.9.9.9"

	for range 5 {
		if !r.Ready(ip) {
			t.Fatal("a fresh key must be ready, however often it is asked")
		}
	}
	r.Allow(ip)
	r.Allow(ip)
	if r.Ready(ip) {
		t.Fatal("an empty bucket must not be ready")
	}
	now = now.Add(100 * time.Millisecond) // refills one token
	for range 3 {
		if !r.Ready(ip) {
			t.Fatal("a refilled bucket must be ready, and asking must not spend it")
		}
	}
	if !r.Allow(ip) || r.Allow(ip) {
		t.Fatal("exactly the one refilled token must be left to spend")
	}
}

// Charge pays for work already done even past empty: requests that all passed
// Ready together each pay, so the bucket goes into debt (down to -burst) and
// stays not ready until the debt is repaid.
func TestRateLimiterCharge(t *testing.T) {
	now := time.Now()
	r := newRateLimiter(10, 2)
	r.now = func() time.Time { return now }
	const ip = "9.9.9.9"

	// Four requests passed Ready on a full bucket of two before any was charged.
	for range 4 {
		r.Charge(ip)
	}
	now = now.Add(200 * time.Millisecond) // refills two tokens: back to zero
	if r.Ready(ip) || r.Allow(ip) {
		t.Fatal("a bucket still repaying its debt must not be ready")
	}
	now = now.Add(100 * time.Millisecond) // one more: a token to spend
	if !r.Ready(ip) {
		t.Fatal("a bucket that has repaid its debt must be ready")
	}

	// However many are charged, the debt stops at -burst: from there three
	// tokens (300 ms) make it ready again.
	for range 100 {
		r.Charge(ip)
	}
	now = now.Add(200 * time.Millisecond)
	if r.Ready(ip) {
		t.Fatal("a bucket at the debt floor must not be ready before it refills to one")
	}
	now = now.Add(100 * time.Millisecond)
	if !r.Ready(ip) {
		t.Fatal("the debt must stop at -burst")
	}
	if r.Charge("8.8.8.8"); !r.Ready("8.8.8.8") {
		t.Fatal("another key must keep its own bucket")
	}
}

// A flood of distinct IPs must not grow the bucket map without bound.
func TestRateLimiterEviction(t *testing.T) {
	now := time.Now()
	r := newRateLimiter(20, 40)
	r.now = func() time.Time { return now }
	for i := 0; i < 100; i++ {
		r.Allow(fmt.Sprintf("10.0.0.%d", i))
	}
	if len(r.buckets) != 100 {
		t.Fatalf("expected 100 buckets, got %d", len(r.buckets))
	}
	// Advance past the idle TTL; the next call sweeps the now-stale buckets.
	now = now.Add(r.idleTTL + time.Second)
	r.Allow("10.0.0.200")
	if len(r.buckets) > 1 {
		t.Fatalf("stale buckets not evicted: %d remain", len(r.buckets))
	}
}

// The login/redeem failure limiter must likewise evict stale, unlocked entries.
func TestLimiterEviction(t *testing.T) {
	now := time.Now()
	l := newLimiter(3, time.Minute)
	l.now = func() time.Time { return now }
	for i := 0; i < 50; i++ {
		l.Fail(fmt.Sprintf("k%d", i))
	}
	if len(l.entries) != 50 {
		t.Fatalf("expected 50 entries, got %d", len(l.entries))
	}
	now = now.Add(2 * time.Minute) // past the window
	l.Fail("k-new")
	if len(l.entries) > 1 {
		t.Fatalf("stale entries not evicted: %d remain", len(l.entries))
	}
}
