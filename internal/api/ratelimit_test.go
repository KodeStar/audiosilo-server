package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// limiter is the brute-force lockout behind login + auth-code redemption. The
// injectable now() makes the window deterministic.
func TestLimiterLockoutAndWindowExpiry(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// The idle TTL is two refills, floored at a minute and capped at maxIdleTTL, on
// every architecture: a configured rate and burst whose refill time is more
// nanoseconds than a Duration holds still sweep a day later.
func TestRateLimiterIdleTTL(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rate, burst float64
		want        time.Duration
	}{
		{50, 200, time.Minute},
		{1, 3600, 2 * time.Hour},
		{0.001, 1000, maxIdleTTL},
		{1, 1e12, maxIdleTTL},
		{1e-9, 1e12, maxIdleTTL},
	} {
		if got := newRateLimiter(c.rate, c.burst).idleTTL; got != c.want {
			t.Errorf("rate %g, burst %g: idleTTL %v, want %v", c.rate, c.burst, got, c.want)
		}
	}
}

// The login/redeem failure limiter must likewise evict stale, unlocked entries.
func TestLimiterEviction(t *testing.T) {
	t.Parallel()
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

// Wait is the time to the bucket's next token: 0 while one is there, then the
// refill time of what is missing, for the 429's Retry-After.
func TestRateLimiterWait(t *testing.T) {
	t.Parallel()
	now := time.Now()
	r := newRateLimiter(2, 1) // 2 tokens/s, burst 1
	r.now = func() time.Time { return now }
	if d := r.Wait("k"); d != 0 {
		t.Fatalf("fresh bucket Wait = %v, want 0", d)
	}
	r.Allow("k")
	if d := r.Wait("k"); d != 500*time.Millisecond {
		t.Fatalf("empty bucket Wait = %v, want 500ms", d)
	}
	now = now.Add(250 * time.Millisecond)
	if d := r.Wait("k"); d != 250*time.Millisecond {
		t.Fatalf("half-refilled Wait = %v, want 250ms", d)
	}
	rec := httptest.NewRecorder()
	rateLimited(rec, r.Wait("k"))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("rateLimited = %d Retry-After %q, want 429 and 1", rec.Code, rec.Header().Get("Retry-After"))
	}

	// A rate so slow its wait is more nanoseconds than a Duration holds is capped
	// at maxIdleTTL, never wrapped round to a negative (a Retry-After of 1).
	slow := newRateLimiter(1e-12, 1)
	slow.now = func() time.Time { return now }
	slow.Allow("k")
	if d := slow.Wait("k"); d != maxIdleTTL {
		t.Fatalf("very slow bucket Wait = %v, want %v", d, maxIdleTTL)
	}
}

// A cover answers HEAD as it answers GET, headers and no body, so a player can
// probe a cover that failed to load for its status and Retry-After without
// fetching the art again; a cross-origin player may read Retry-After.
func TestCoverAnswersHead(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	root := t.TempDir()
	copyFixture(t, root, cradlePart, "A/Book/01.m4b")
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0, 0xFF, 0xD9}
	if err := os.WriteFile(filepath.Join(root, "A", "Book", "cover.jpg"), jpeg, 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Covers", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	tok := adminSession(t, e)
	if resp, body := e.do(t, "GET", mediaURL(lib.ID, "cover", "A/Book", tok), "", ""); resp.StatusCode != http.StatusOK || len(body) != len(jpeg) {
		t.Fatalf("GET cover = %d %d bytes", resp.StatusCode, len(body))
	}
	resp, body := e.do(t, "HEAD", mediaURL(lib.ID, "cover", "A/Book", tok), "", "")
	if resp.StatusCode != http.StatusOK || body != "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "image/") {
		t.Fatalf("HEAD cover = %d %q %d bytes, want 200 image/* and no body", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
	}
	if !strings.Contains(corsExposeHeaders, "Retry-After") {
		t.Fatalf("Access-Control-Expose-Headers %q lacks Retry-After", corsExposeHeaders)
	}
}
