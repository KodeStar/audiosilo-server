package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestTimeoutMiddlewareFiresOnSlowRequest asserts a non-streaming request that
// outlives the deadline gets a prompt 503 instead of hanging - the core of the
// "self-recovering" fix. (The request context is also cancelled, which in
// production aborts the stuck DB call.)
func TestTimeoutMiddlewareFiresOnSlowRequest(t *testing.T) {
	t.Parallel()
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

func TestTimeoutMiddlewareBoundsUnbufferedClaim(t *testing.T) {
	a := &API{timeoutDur: 20 * time.Millisecond}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("claim has no deadline")
		}
		<-r.Context().Done()
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	start := time.Now()
	a.timeout(handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/bridge/jobs/next", nil))
	if rec.Code != http.StatusNoContent || time.Since(start) > 2*time.Second {
		t.Fatalf("claim timeout: %d after %v", rec.Code, time.Since(start))
	}
}

// The OPML import runs past the request timeout under a bound of its own
// (allowed: its answer is the handler's, written as the handler wrote it),
// and that bound still ends it (denied: an import that would run on gets its
// context cancelled). A GET of the same path, and any other POST, keeps the
// request timeout.
func TestTimeoutMiddlewareGivesTheOPMLImportItsOwnBound(t *testing.T) {
	t.Parallel()
	a := &API{timeoutDur: 20 * time.Millisecond, opmlImportDur: 2 * time.Second}
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < time.Second {
			t.Errorf("import deadline %v %v, want its own bound", deadline, ok)
		}
		select {
		case <-r.Context().Done():
			w.WriteHeader(http.StatusTeapot)
		case <-time.After(100 * time.Millisecond): // five request timeouts
			w.WriteHeader(http.StatusOK)
		}
	})
	rec := httptest.NewRecorder()
	a.timeout(slow).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, opmlImportPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("import past the request timeout = %d, want 200", rec.Code)
	}

	a.opmlImportDur = 20 * time.Millisecond
	ended := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			w.WriteHeader(http.StatusServiceUnavailable)
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})
	rec = httptest.NewRecorder()
	start := time.Now()
	a.timeout(ended).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, opmlImportPath, nil))
	if rec.Code != http.StatusServiceUnavailable || time.Since(start) > 2*time.Second {
		t.Fatalf("import past its bound = %d after %v, want 503 promptly", rec.Code, time.Since(start))
	}

	a.opmlImportDur = 5 * time.Second
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, opmlImportPath, nil),
		httptest.NewRequest(http.MethodPost, "/api/v1/podcasts/follow", nil),
	} {
		rec = httptest.NewRecorder()
		a.timeout(ended).ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "request timed out") {
			t.Fatalf("%s %s = %d %q, want the request timeout", req.Method, req.URL.Path, rec.Code, rec.Body.String())
		}
	}
}

// TestTimeoutMiddlewareExemptsStreaming asserts streaming routes are NOT bounded:
// a /stream handler that runs past the deadline still completes (audio playback
// must not be cut off - there is deliberately no WriteTimeout either).
func TestTimeoutMiddlewareExemptsStreaming(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	e := newTestEnv(t)
	for _, path := range []string{"/healthz", "/api/v1/healthz"} {
		resp, body := e.do(t, http.MethodGet, path, "", "")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "ok") {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, body)
		}
	}
}

// TestTimeoutMiddlewareBoundsUploads: only reads of a streaming path are exempt;
// an upload to one (the admin's PUT .../cover) is still cut off at the deadline,
// so a slow client can't hold it open.
func TestTimeoutMiddlewareBoundsUploads(t *testing.T) {
	t.Parallel()
	a := &API{timeoutDur: 50 * time.Millisecond}
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/libraries/1/cover", strings.NewReader("img"))
	rec := httptest.NewRecorder()
	a.timeout(slow).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("slow cover upload = %d, want 503", rec.Code)
	}
}

// Only a book's own media, its ebook and a backup's download outlive the request timeout:
// a community cover is a bounded thumbnail, and an admin route ending in /cover
// or /stream is no media.
func TestStreamingPaths(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]bool{
		"/api/v1/libraries/1/cover":       true,
		"/api/v1/libraries/1/stream":      true,
		"/api/v1/libraries/1/meta/cover":  false,
		"/api/v1/libraries//cover":        false,
		"/api/v1/admin/libraries/1/cover": false,
		"/api/v1/libraries/1/books":       false,
		// A book's ebook (a large epub over a slow link), not the reading progress.
		"/api/v1/libraries/1/ebook":            true,
		"/api/v1/libraries/1/reading-progress": false,
		"/api/v1/admin/libraries/1/ebook":      false,
	} {
		if got := isStreamingPath(p); got != want {
			t.Errorf("isStreamingPath(%q) = %v, want %v", p, got, want)
		}
	}
}
