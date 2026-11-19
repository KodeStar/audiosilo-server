package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
	g := NewStartGate(nil)
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

// The wrapper NewStartGate is given goes around the starting answer only: the
// handler Open is given answers bare.
func TestStartGateWrapsOnlyTheStartingAnswer(t *testing.T) {
	t.Parallel()
	g := NewStartGate(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Wrapped", "1")
			next.ServeHTTP(w, r)
		})
	})
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("X-Wrapped") != "1" {
		t.Errorf("while starting = %d %v, want the wrapped 503", rec.Code, rec.Header())
	}
	g.Open(http.NotFoundHandler())
	rec = httptest.NewRecorder()
	g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Wrapped") != "" {
		t.Errorf("after Open = %d %v, want the handler's own answer", rec.Code, rec.Header())
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
	s, err := Start(ctx, cfg, NewStartGate(nil), log)
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
	select {
	case <-s.Done():
	default:
		t.Fatal("Done still open after Wait shut the server down")
	}
	if err := s.Wait(); err != nil {
		t.Fatalf("second Wait = %v", err)
	}
}

// A server stopped the moment it started (a start that fails at its first
// step) has closed its listener by the time Wait returns, whether or not serve
// had reached it: Done is closed and the port can be taken again at once.
func TestWaitReturnsWithTheListenerClosed(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	for range 50 {
		cfg := &config.Config{Bind: freeAddr(t), TLS: config.TLSConfig{Mode: config.TLSOff}}
		ctx, stop := context.WithCancel(t.Context())
		s, err := Start(ctx, cfg, NewStartGate(nil), log)
		if err != nil {
			stop()
			t.Fatal(err)
		}
		stop()
		if err := s.Wait(); err != nil {
			t.Fatalf("Wait = %v", err)
		}
		select {
		case <-s.Done():
		default:
			t.Fatal("Done still open after Wait")
		}
		ln, err := net.Listen("tcp", cfg.Bind)
		if err != nil {
			t.Fatalf("the port is still taken after Wait: %v", err)
		}
		_ = ln.Close()
	}
}

// A serve that failed on its own is Wait's answer even when the context has
// ended too by the time Wait is asked.
func TestWaitReportsServeFailureAfterContextEnds(t *testing.T) {
	t.Parallel()
	lost := errors.New("accept: listener lost")
	ctx, stop := context.WithCancel(context.Background())
	s := serveIn(ctx, &http.Server{}, func() error { return lost }, slog.New(slog.DiscardHandler))
	<-s.Done()
	stop()
	if err := s.Wait(); !errors.Is(err, lost) {
		t.Fatalf("Wait = %v, want %v", err, lost)
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

// A browser opening a page while the server starts gets the same 503 and
// Retry-After as a page that reloads itself, under a CSP that allows its own
// style sheet and nothing else; an API client (a JSON Accept, fetch's */*, no
// Accept) keeps the JSON answer.
func TestStartGateAnswersBrowsersWithAPage(t *testing.T) {
	t.Parallel()
	g := NewStartGate(nil)
	const browser = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	for _, path := range []string{"/", "/admin/", "/web/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept", browser)
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		h := rec.Header()
		body := rec.Body.String()
		if rec.Code != http.StatusServiceUnavailable || h.Get("Retry-After") != startingRetry ||
			!strings.HasPrefix(h.Get("Content-Type"), "text/html") || h.Get("Cache-Control") != "no-store" ||
			!strings.Contains(h.Get("Vary"), "Accept") {
			t.Errorf("%s from a browser = %d %v, want the 503 page", path, rec.Code, h)
		}
		if !strings.Contains(body, "AudioSilo is starting") ||
			!strings.Contains(body, `<meta http-equiv="refresh" content="`+startingRetry+`">`) ||
			strings.Contains(body, "<script") || strings.Contains(body, "http://") || strings.Contains(body, "https://") {
			t.Errorf("%s page = %q, want a self-contained page that reloads itself", path, body)
		}
	}

	// The served CSP allows exactly the served page's one style sheet (the
	// hash of the bytes between its <style> tags), and no script.
	req := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	req.Header.Set("Accept", browser)
	served := httptest.NewRecorder()
	g.ServeHTTP(served, req)
	page := served.Body.String()
	_, style, _ := strings.Cut(page, "<style>")
	style, _, _ = strings.Cut(style, "</style>")
	sum := sha256.Sum256([]byte(style))
	csp := served.Header().Get("Content-Security-Policy")
	if style == "" || strings.Count(page, "<style") != 1 || !strings.Contains(csp, "default-src 'none'") ||
		strings.Contains(csp, "script-src") ||
		!strings.Contains(csp, "style-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"';") {
		t.Errorf("CSP %q does not admit the served page's own style sheet alone", csp)
	}

	head := httptest.NewRequest(http.MethodHead, "/admin/", nil)
	head.Header.Set("Accept", browser)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, head)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.Len() != 0 {
		t.Errorf("HEAD from a browser = %d %q, want 503 with no body", rec.Code, rec.Body)
	}

	for _, accept := range []string{"", "*/*", "application/json", "application/json, text/plain, */*"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/server", nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		var v StartingAnswer
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Content-Type") != "application/json" ||
			json.Unmarshal(rec.Body.Bytes(), &v) != nil || v.Code != "server_starting" {
			t.Errorf("Accept %q = %d %q, want the JSON answer", accept, rec.Code, rec.Body)
		}
	}
}

func TestPrefersHTML(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		accept []string
		want   bool
	}{
		{nil, false},
		{[]string{"*/*"}, false},
		{[]string{"application/json"}, false},
		{[]string{"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"}, true},
		{[]string{"application/xhtml+xml"}, true},
		{[]string{"TEXT/HTML"}, true},
		{[]string{"text/html;q=0"}, false},
		{[]string{"text/html;q=0.5, application/json"}, false},
		{[]string{"text/html", "application/json;q=0.5"}, true},
		{[]string{"application/json, text/html"}, false},
		{[]string{"text/html ; q = 0.9, application/json;q=0.1"}, true},
		// JSON's quality is the most specific range's that names it.
		{[]string{"text/html;q=0.1, */*"}, false},
		{[]string{"text/html;q=0.5, application/*"}, false},
		{[]string{"text/html, application/*;q=0.5, */*"}, true},
		{[]string{"text/html;q=0.9, application/json;q=0.5, */*"}, true},
		{[]string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"}, true},
	} {
		if got := prefersHTML(tc.accept); got != tc.want {
			t.Errorf("prefersHTML(%q) = %v, want %v", tc.accept, got, tc.want)
		}
	}
}
