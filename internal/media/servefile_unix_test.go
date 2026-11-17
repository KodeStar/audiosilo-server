//go:build unix

package media

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO named as audio with no writer is a 404 at once: its open never waits
// for a writer, so it is never given up on and left counted on its mount (one
// per request would fill the mount's cap and refuse every read there), and a
// Cache-Control set for the file expected is not sent with it.
func TestServeFileTurnsAwayAFIFO(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "part.mp3")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Cache-Control", "private, max-age=86400")
	start := time.Now()
	if err := ServeFile(rec, httptest.NewRequest(http.MethodGet, "/", nil), path, false, time.Minute); err != nil {
		t.Fatalf("ServeFile of a FIFO = %v, want it answered here", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("ServeFile of a FIFO took %v, want it turned away at once", took)
	}
	if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "" {
		t.Errorf("ServeFile of a FIFO = %d (Cache-Control %q), want an uncached 404", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
