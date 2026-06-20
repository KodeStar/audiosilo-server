package api

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/config"
)

func TestBearerToken(t *testing.T) {
	cases := []struct {
		name       string
		authHeader string
		query      string
		want       string
	}{
		{"bearer header", "Bearer abc", "", "abc"},
		{"bearer trims surrounding space", "Bearer   abc  ", "", "abc"},
		{"query fallback for media elements", "", "tok123", "tok123"},
		{"header wins over query", "Bearer abc", "tok123", "abc"},
		{"non-bearer header ignored", "Basic xyz", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}
			if tc.query != "" {
				q := r.URL.Query()
				q.Set("token", tc.query)
				r.URL.RawQuery = q.Encode()
			}
			if got := bearerToken(r); got != tc.want {
				t.Fatalf("bearerToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPeerIP(t *testing.T) {
	if got := peerIP("1.2.3.4:5678"); got != "1.2.3.4" {
		t.Fatalf("peerIP host:port = %q, want 1.2.3.4", got)
	}
	// No port: SplitHostPort fails and the raw value is returned.
	if got := peerIP("1.2.3.4"); got != "1.2.3.4" {
		t.Fatalf("peerIP bare = %q, want 1.2.3.4", got)
	}
}

func TestIsTrusted(t *testing.T) {
	_, n, _ := net.ParseCIDR("10.0.0.0/8")
	nets := []*net.IPNet{n}
	if !isTrusted("10.1.2.3", nets) {
		t.Fatal("10.1.2.3 should be trusted by 10.0.0.0/8")
	}
	if isTrusted("192.168.1.1", nets) {
		t.Fatal("192.168.1.1 should not be trusted")
	}
	if isTrusted("not-an-ip", nets) {
		t.Fatal("an unparseable address must never be trusted")
	}
}

func TestSecureHeaders(t *testing.T) {
	h := secureHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-site",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Fatalf("header %s = %q, want %q", k, got, v)
		}
	}
}

func TestCORSAllowList(t *testing.T) {
	a := &API{cfg: &config.Config{CORSOrigins: []string{"https://app.example.com"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := a.cors(next)

	// An allow-listed origin is echoed back.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Fatalf("allowed origin ACAO = %q", got)
	}

	// A non-listed origin gets no CORS grant.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("disallowed origin should get no ACAO, got %q", got)
	}

	// Preflight is answered with 204.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS = %d, want 204", rec.Code)
	}
}

func TestCORSWildcard(t *testing.T) {
	a := &API{cfg: &config.Config{CORSOrigins: []string{"*"}}}
	h := a.cors(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://anything.example")
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://anything.example" {
		t.Fatalf("wildcard ACAO = %q", got)
	}
}

// realIP must honour X-Forwarded-For only when the direct peer is a configured
// trusted proxy, and take the right-most (closest-proxy, spoof-resistant) value.
// This guards the rate limiter against IP spoofing (review finding S8).
func TestRealIPTrust(t *testing.T) {
	a := &API{cfg: &config.Config{TrustedProxies: []string{"10.0.0.0/8"}}}
	var captured string
	h := a.realIP(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = clientIP(r)
	}))

	// Peer is a trusted proxy: trust the right-most XFF entry.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.1.2.3:40000"
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "2.2.2.2" {
		t.Fatalf("trusted XFF: clientIP = %q, want 2.2.2.2", captured)
	}

	// Peer is NOT trusted: ignore XFF, use the peer address.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:40000"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if captured != "203.0.113.9" {
		t.Fatalf("untrusted XFF must be ignored: clientIP = %q, want 203.0.113.9", captured)
	}
}
