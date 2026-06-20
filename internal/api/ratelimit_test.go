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

// ipRateLimiter is the per-IP token bucket on overall request rate.
func TestIPRateLimiterBucket(t *testing.T) {
	now := time.Now()
	r := newIPRateLimiter(10, 2) // 10 tokens/sec, burst of 2
	r.now = func() time.Time { return now }
	const ip = "9.9.9.9"

	if !r.Allow(ip) {
		t.Fatal("first request consumes the initial burst token")
	}
	if !r.Allow(ip) {
		t.Fatal("second request consumes the last burst token")
	}
	if r.Allow(ip) {
		t.Fatal("third request must be denied — bucket empty")
	}

	now = now.Add(100 * time.Millisecond) // refills 0.1s * 10/s = 1 token
	if !r.Allow(ip) {
		t.Fatal("request allowed again after the bucket refills")
	}
}

// A flood of distinct IPs must not grow the bucket map without bound.
func TestIPRateLimiterEviction(t *testing.T) {
	now := time.Now()
	r := newIPRateLimiter(20, 40)
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
