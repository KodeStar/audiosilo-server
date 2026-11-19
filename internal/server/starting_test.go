package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Until Open, every request (any path, any method) is answered 503 with the
// starting body and Retry-After, a HEAD with no body; after it, the handler
// Open was given answers.
func TestStartGate(t *testing.T) {
	t.Parallel()
	g := &StartGate{}
	for _, path := range []string{"/healthz", "/api/v1/server", "/admin/"} {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body struct{ Status, Error, Code string }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q: %v", path, rec.Body, err)
		}
		if rec.Code != http.StatusServiceUnavailable || body.Status != StartingStatus || body.Code == "" ||
			rec.Header().Get("Retry-After") == "" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s while starting = %d %+v %v, want 503 starting", path, rec.Code, body, rec.Header())
		}
	}
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.Len() != 0 {
		t.Errorf("HEAD while starting = %d %q, want 503 with no body", rec.Code, rec.Body)
	}

	g.Open(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "app") }))
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "app" {
		t.Errorf("after Open = %d %q, want the app's answer", rec.Code, rec.Body)
	}
}

// Start returns a listen error at once, serving nothing; a started server
// answers until its context ends, and Wait returns the same nil every call.
func TestStartListenErrorAndWait(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	log := slog.New(slog.DiscardHandler)
	cfg := &config.Config{Bind: taken.Addr().String(), TLS: config.TLSConfig{Mode: config.TLSOff}}
	if s, err := Start(t.Context(), cfg, http.NotFoundHandler(), log); err == nil {
		t.Fatalf("Start on a taken port = %v, nil; want the listen error", s)
	}

	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = free.Addr().String()
	_ = free.Close()
	ctx, stop := context.WithCancel(t.Context())
	s, err := Start(ctx, cfg, &StartGate{}, log)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + cfg.Bind + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("started server = %d, want the gate's 503", resp.StatusCode)
	}
	stop()
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("second Wait = %v", err)
	}
}
