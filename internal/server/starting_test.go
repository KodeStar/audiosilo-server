package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// Until Open, every request (any path, any method) is answered 503 with the
// starting body (the envelope's fields, and how long the start has run) and
// Retry-After, a HEAD with no body; after it, the handler Open was given
// answers.
func TestStartGate(t *testing.T) {
	t.Parallel()
	g := NewStartGate()
	g.started = time.Now().Add(-90 * time.Second)
	for _, path := range []string{"/healthz", "/api/v1/server", "/admin/"} {
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: body %q: %v", path, rec.Body, err)
		}
		running, _ := body["starting_for_seconds"].(float64)
		if rec.Code != http.StatusServiceUnavailable || body["status"] != StartingStatus ||
			body["code"] != "server_starting" || body["error"] == "" || running < 90 || running > 150 ||
			rec.Header().Get("Retry-After") == "" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s while starting = %d %v %v, want 503 starting for 90 s", path, rec.Code, body, rec.Header())
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
// answers until its context ends, Done closed only then, and Wait returns the
// same nil every call.
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

	cfg.Bind = freeAddr(t)
	ctx, stop := context.WithCancel(t.Context())
	s, err := Start(ctx, cfg, NewStartGate(), log)
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
	select {
	case <-s.Done():
		t.Fatal("Done closed while serving")
	default:
	}
	stop()
	if err := s.Wait(); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	<-s.Done() // closed once Wait shut it down
	if err := s.Wait(); err != nil {
		t.Fatalf("second Wait = %v", err)
	}
}

// A serve that fails on its own (the listener lost) closes Done without
// anyone asking, so a start still running can stop there, and Wait returns
// why.
func TestServingFailureClosesDone(t *testing.T) {
	t.Parallel()
	lost := errors.New("accept: listener lost")
	s := serveIn(context.Background(), &http.Server{}, func() error { return lost }, slog.New(slog.DiscardHandler))
	<-s.Done()
	if err := s.Wait(); !errors.Is(err, lost) {
		t.Fatalf("Wait = %v, want %v", err, lost)
	}
}
