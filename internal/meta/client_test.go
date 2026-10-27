package meta

import (
	"context"
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

	c, n, waits = rateLimitedServer(t, 100, "")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := c.lookup(ctx, "B00TEST", ""); err == nil {
		t.Fatal("want an error")
	}
	if n.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("requests %d waits %v under a short deadline, want one request and no wait", n.Load(), *waits)
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
		now.Add(3 * time.Second).Format(http.TimeFormat): 3 * time.Second,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", h, got, want)
		}
	}
}
