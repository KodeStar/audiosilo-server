package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTimeoutMiddlewareFiresOnSlowRequest asserts a non-streaming request that
// outlives the deadline gets a prompt 503 instead of hanging — the core of the
// "self-recovering" fix. (The request context is also cancelled, which in
// production aborts the stuck DB call.)
func TestTimeoutMiddlewareFiresOnSlowRequest(t *testing.T) {
	a := &API{timeoutDur: 50 * time.Millisecond}
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done(): // cancelled by the timeout
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})
	h := a.timeout(slow)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("timeout did not fire promptly: took %v", el)
	}
}

// TestTimeoutMiddlewareExemptsStreaming asserts streaming routes are NOT bounded:
// a /stream handler that runs past the deadline still completes (audio playback
// must not be cut off — there is deliberately no WriteTimeout either).
func TestTimeoutMiddlewareExemptsStreaming(t *testing.T) {
	a := &API{timeoutDur: 50 * time.Millisecond}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond) // exceeds timeoutDur
		w.WriteHeader(http.StatusOK)
	})
	h := a.timeout(handler)

	for _, path := range []string{
		"/api/v1/libraries/1/stream",
		"/api/v1/libraries/1/cover",
		"/web/index.html",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("path %s: status = %d, want 200 (should be exempt from timeout)", path, rec.Code)
		}
	}
}

// TestHealthzPublicOK asserts the health probe is public and returns 200 with the
// database reachable.
func TestHealthzPublicOK(t *testing.T) {
	e := newTestEnv(t)
	for _, path := range []string{"/healthz", "/api/v1/healthz"} {
		resp, body := e.do(t, http.MethodGet, path, "", "")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "ok") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
}
