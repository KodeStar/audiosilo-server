package meta

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rateLimitedServer answers 429 with retryAfter for the first limited
// requests, then 200 with {"recording_id":"r1"}; it counts the requests.
func rateLimitedServer(t *testing.T, limited int32, retryAfter string) (*client, *atomic.Int32, *[]time.Duration) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= limited {
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"recording_id":"r1"}`))
	}))
	t.Cleanup(srv.Close)
	c := newClient(srv.URL)
	waits := &[]time.Duration{}
	c.sleep = func(_ context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return nil
	}
	return c, &n, waits
}

// A 429 is waited out (its Retry-After) and asked again (server#247).
func TestClientRetriesRateLimit(t *testing.T) {
	t.Parallel()
	c, n, waits := rateLimitedServer(t, 1, "2")
	got, err := c.lookup(context.Background(), "B00TEST", "")
	if err != nil || got.RecordingID != "r1" {
		t.Fatalf("lookup after one 429 = %+v %v, want the answer", got, err)
	}
	if n.Load() != 2 || len(*waits) != 1 || (*waits)[0] != 2*time.Second {
		t.Fatalf("requests %d waits %v, want 2 requests and one 2s wait", n.Load(), *waits)
	}
}

// Repeated 429s give up after the retries with a clear error, each wait capped,
// never a tight loop; a wait the caller's deadline would cut short isn't made.
func TestClientRateLimitGivesUp(t *testing.T) {
	t.Parallel()
	c, n, waits := rateLimitedServer(t, 100, "3600")
	_, err := c.lookup(context.Background(), "B00TEST", "")
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("lookup under 429s = %v, want a rate-limited error", err)
	}
	if n.Load() != rateLimitRetries+1 || len(*waits) != rateLimitRetries {
		t.Fatalf("requests %d waits %v, want %d requests", n.Load(), *waits, rateLimitRetries+1)
	}
	for _, d := range *waits {
		if d != maxRetryAfter {
			t.Fatalf("wait %v, want the cap %v", d, maxRetryAfter)
		}
	}

	// A wait longer than the whole deadline, so the answer has all of the deadline
	// to arrive however busy the machine is and the wait never fits.
	c, n, waits = rateLimitedServer(t, 100, "10")
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	if _, err := c.lookup(ctx, "B00TEST", ""); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("lookup under a short deadline = %v, want a rate-limited error", err)
	}
	if n.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("requests %d waits %v under a short deadline, want one request and no wait", n.Load(), *waits)
	}
}

// Another part of the process closing http.DefaultTransport's idle connections
// (each httptest.Server's Close does) doesn't reach the client's: they are its
// own, so its connection outlives every close. On that transport a close can fail
// a lookup: net/http puts the connection of a 429 with no body back in the idle
// list a moment before it hands the caller the response, a close in that moment
// fails the request instead, and it doesn't retry that failure. No body is also
// what makes this exact: the connection is idle by the time lookup returns, so on
// that transport each close costs a connection, one per lookup. Fewer is the pass,
// not exactly one: net/http drops a connection by itself when its write goroutine
// is 50 ms late reporting the request written (wroteRequest), as on a busy runner.
func TestClientConnectionsAreItsOwn(t *testing.T) {
	t.Parallel()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	c := newClient(srv.URL)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	const lookups = 5
	for range lookups {
		if _, err := c.lookup(context.Background(), "B00TEST", ""); err == nil || !strings.Contains(err.Error(), "rate limited") {
			t.Fatalf("lookup = %v, want a rate-limited error", err)
		}
		http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	}
	if n := conns.Load(); n >= lookups {
		t.Fatalf("%d connections over %d lookups, each close reached the client's", n, lookups)
	}
}

// The client's transport is a copy of http.DefaultTransport, settings and all, not
// a bare one; a round tripper the program embedding the server put there has
// nothing to copy and is kept, never a panic at start-up.
func TestOwnTransport(t *testing.T) {
	t.Parallel()
	shared := http.DefaultTransport.(*http.Transport)
	rt := newClient("http://meta.test").http.Transport
	got, ok := rt.(*http.Transport)
	if !ok || got == shared {
		t.Fatalf("transport = %T (%p), want a copy of http.DefaultTransport", rt, rt)
	}
	if got.Proxy == nil || got.DialContext == nil || !got.ForceAttemptHTTP2 ||
		got.TLSHandshakeTimeout != shared.TLSHandshakeTimeout || got.IdleConnTimeout != shared.IdleConnTimeout {
		t.Fatalf("the copy lost http.DefaultTransport's settings: %+v", got)
	}
	tlsConf := &tls.Config{MinVersion: tls.VersionTLS13}
	if tr, ok := ownTransport(&http.Transport{TLSClientConfig: tlsConf}).(*http.Transport); !ok || tr.TLSClientConfig.MinVersion != tlsConf.MinVersion {
		t.Fatalf("the copy lost its TLS settings: %+v", tr)
	}
	embedder := &struct{ http.RoundTripper }{}
	if rt := ownTransport(embedder); rt != http.RoundTripper(embedder) {
		t.Fatalf("ownTransport(a wrapper) = %T, want the wrapper", rt)
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"":     defaultRetryAfter,
		"junk": defaultRetryAfter,
		"0":    100 * time.Millisecond,
		"4":    4 * time.Second,
		"99":   maxRetryAfter,
		// Large enough to wrap if multiplied as it came.
		"9223372037": maxRetryAfter,
		now.Add(3 * time.Second).Format(http.TimeFormat): 3 * time.Second,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
}
