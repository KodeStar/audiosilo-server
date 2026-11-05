package meta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
)

func TestReportAllowed(t *testing.T) {
	t.Parallel()
	for base, want := range map[string]bool{
		"https://meta.audiosilo.app":     true,
		"https://meta.example/prefix":    true,
		"http://127.0.0.1:8080":          true,
		"http://127.9.9.9":               true,
		"http://localhost:9000":          true,
		"http://LOCALHOST":               true,
		"http://[::1]:9000":              true,
		"http://meta.audiosilo.app":      false, // plain http over the network
		"http://10.0.0.5:8080":           false,
		"http://192.168.1.2":             false,
		"http://localhost.example.com":   false,
		"ftp://meta.audiosilo.app":       false,
		"meta.audiosilo.app":             false,
		"":                               false,
		"https://":                       false,
		"http://127.0.0.1.example.test/": false,
	} {
		if got := reportAllowed(base); got != want {
			t.Errorf("reportAllowed(%q) = %v, want %v", base, got, want)
		}
	}
}

// The POST carries {"asins": [...]} and nothing else: the exact header set,
// no client id, server id, auth or version; the body's only key is "asins".
func TestReportUnknownASINsWire(t *testing.T) {
	t.Parallel()
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":1,"known":1,"invalid":0}`))
	}))
	t.Cleanup(srv.Close)
	s := NewService(srv.URL+"/", nil)
	ans, err := s.ReportUnknownASINs(context.Background(), []string{"B0ABCDEFGH", "B0XXXXXXXX"})
	if err != nil || ans != (ReportAnswer{Accepted: 1, Known: 1}) {
		t.Fatalf("report = %+v, %v", ans, err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/api/v1/intake/unknown-asins" || got.URL.RawQuery != "" {
		t.Fatalf("request = %s %s", got.Method, got.URL)
	}
	keys := slices.Sorted(maps.Keys(got.Header))
	if want := []string{"Accept-Encoding", "Content-Length", "Content-Type", "User-Agent"}; !slices.Equal(keys, want) {
		t.Fatalf("headers = %v, want exactly %v", got.Header, want)
	}
	if got.Header.Get("Content-Type") != "application/json" || got.Header.Get("User-Agent") != "AudioSilo" {
		t.Fatalf("headers = %v", got.Header)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || len(m) != 1 || string(m["asins"]) != `["B0ABCDEFGH","B0XXXXXXXX"]` {
		t.Fatalf("body = %s (%v)", body, err)
	}
}

// What each answer means: only a 202 with an intake answer is accepted.
func TestReportUnknownASINsAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error // nil: accepted; errStatus: a plain failure
	}{
		{"accepted", http.StatusAccepted, `{"accepted":2,"known":0,"invalid":0}`, nil},
		{"old metaserve or intake off", http.StatusNotFound, `<html>not here</html>`, ErrReportNotOffered},
		{"method not allowed", http.StatusMethodNotAllowed, ``, ErrReportNotOffered},
		{"a 2xx without accepted", http.StatusOK, `{"ok":true}`, ErrReportNotOffered},
		{"a 202 without accepted", http.StatusAccepted, `{}`, ErrReportNotOffered},
		{"a 2xx that is no JSON", http.StatusOK, `<html></html>`, ErrReportNotOffered},
		{"a 200 intake answer", http.StatusOK, `{"accepted":2}`, errStatus},
		{"rate limited", http.StatusTooManyRequests, `{"error":"slow down"}`, errStatus},
		{"unavailable", http.StatusServiceUnavailable, `{"error":"down"}`, errStatus},
		{"bad request", http.StatusBadRequest, `{"error":"bad"}`, errStatus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var posts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				posts.Add(1)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			_, err := NewService(srv.URL, nil).ReportUnknownASINs(context.Background(), []string{"B0ABCDEFGH"})
			var se *statusError
			switch {
			case tc.want == nil && err != nil, tc.want == errStatus && !errors.As(err, &se),
				tc.want != nil && tc.want != errStatus && !errors.Is(err, tc.want):
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := posts.Load(); n != 1 {
				t.Fatalf("%d POSTs, want 1 (a refused POST is never retried here)", n)
			}
		})
	}
}

// errStatus marks a case expecting a *statusError.
var errStatus = errors.New("status error")

// A 3xx is the answer, never followed: the POST can't be moved elsewhere or
// downgraded.
func TestReportUnknownASINsNoRedirect(t *testing.T) {
	t.Parallel()
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":1}`))
	}))
	t.Cleanup(other.Close)
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+reportPath, code)
		}))
		_, err := NewService(srv.URL, nil).ReportUnknownASINs(context.Background(), []string{"B0ABCDEFGH"})
		srv.Close()
		if !errors.Is(err, ErrReportNotOffered) {
			t.Fatalf("%d: err = %v, want ErrReportNotOffered", code, err)
		}
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("a redirect was followed %d times", n)
	}
}

// countingTransport counts the requests it is handed and fails them.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("no network in this test")
}

// Plain http to another machine: nothing is sent, not even a connection.
func TestReportUnknownASINsHTTPOffMachine(t *testing.T) {
	t.Parallel()
	s := NewService("http://meta.audiosilo.app", nil)
	rt := &countingTransport{}
	s.report.Transport = rt
	if s.CanReport() {
		t.Fatal("CanReport over plain http to another machine")
	}
	if _, err := s.ReportUnknownASINs(context.Background(), []string{"B0ABCDEFGH"}); !errors.Is(err, ErrReportNeedsHTTPS) {
		t.Fatalf("err = %v, want ErrReportNeedsHTTPS", err)
	}
	if rt.n.Load() != 0 {
		t.Fatal("a request was made")
	}
}

// countingMirror is a ready local copy answering every request with a 404 and
// counting them.
type countingMirror struct{ hits *atomic.Int32 }

func (m countingMirror) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		m.hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
}
func (m countingMirror) Ready() bool { return true }

// In mirror mode the report still goes to the remote service, never the copy.
func TestReportUnknownASINsMirrorGoesRemote(t *testing.T) {
	t.Parallel()
	var remote atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == reportPath {
			remote.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"accepted":1}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	var local atomic.Int32
	s := NewService(srv.URL, nil)
	s.SetMirror(countingMirror{&local}, nil)
	if _, err := s.ReportUnknownASINs(context.Background(), []string{"B0ABCDEFGH"}); err != nil {
		t.Fatal(err)
	}
	if remote.Load() != 1 || local.Load() != 0 {
		t.Fatalf("remote %d, local %d: want the POST at the remote service only", remote.Load(), local.Load())
	}
}

// KnownASIN: a hit is known, only a 404 is unknown, anything else an error.
func TestKnownASIN(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		switch r.URL.Query().Get("asin") {
		case "B0KNOWN001":
			_, _ = w.Write([]byte(`{"work":{"id":"w1","title":"T"},"recording_id":"r1"}`))
		case "B0UNKNOWN1":
			http.NotFound(w, r)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	s := NewService(srv.URL, nil)
	ctx := context.Background()
	if known, err := s.KnownASIN(ctx, " b0known001 "); !known || err != nil {
		t.Fatalf("known = %v, %v", known, err)
	}
	if known, err := s.KnownASIN(ctx, "B0UNKNOWN1"); known || err != nil {
		t.Fatalf("unknown = %v, %v", known, err)
	}
	if _, err := s.KnownASIN(ctx, "B0BROKEN01"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("a 500 = %v, want an error that is not ErrNotFound", err)
	}
	n := lookups.Load()
	if _, err := s.KnownASIN(ctx, "not-an-asin"); err == nil || lookups.Load() != n {
		t.Fatalf("a malformed ASIN = %v and %d lookups, want an error and none", err, lookups.Load()-n)
	}
}
