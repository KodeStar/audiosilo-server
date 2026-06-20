package api

import (
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
