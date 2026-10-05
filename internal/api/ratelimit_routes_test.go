package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// routeLimitEnv is a test env whose token buckets are frozen in time, so a test
// counts exactly the burst with no refill, plus a way to send a request from a
// chosen address.
type routeLimitEnv struct {
	*testEnv
	h http.Handler
}

func newRouteLimitEnv(t *testing.T) *routeLimitEnv {
	t.Helper()
	e := newTestEnv(t)
	now := time.Now()
	for _, l := range []*rateLimiter{e.api.ipLimiter, e.api.mediaLimiter} {
		l.now = func() time.Time { return now }
	}
	return &routeLimitEnv{testEnv: e, h: e.api.Handler()}
}

// serve sends a request from ip (with a bearer token when tok is set) and
// returns the status.
func (e *routeLimitEnv) serve(ip, method, path, tok, body string) int {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = ip + ":1234"
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code
}

func (e *routeLimitEnv) get(ip, path, tok string) int { return e.serve(ip, "GET", path, tok, "") }

func (e *routeLimitEnv) post(ip, path, body string) int { return e.serve(ip, "POST", path, "", body) }

func (e *routeLimitEnv) session(t *testing.T) string {
	t.Helper()
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// mediaURL is a media GET authenticated the way a browser <img>/<audio> does it.
func mediaURL(libID int64, kind, path, tok string) string {
	return "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/" + kind +
		"?path=" + escape(path) + "&token=" + escape(tok)
}

// The general limit leaves out the static files the web package serves: a cold
// console page loads more chunks than the burst, and none of them may be refused
// or use up the budget its API calls need. Everything else is still limited.
func TestRateLimitSkipsStaticFiles(t *testing.T) {
	e := newRouteLimitEnv(t)
	const ip = "192.0.2.9"
	for i := range 250 {
		for _, path := range []string{"/admin/assets/chunk.js", "/"} {
			if code := e.get(ip, path, ""); code == http.StatusTooManyRequests {
				t.Fatalf("static request %d (%s) was rate limited", i, path)
			}
		}
	}
	if code := e.get(ip, "/api/v1/server", ""); code == http.StatusTooManyRequests {
		t.Fatal("static requests used up the API's budget")
	}
	// Anything not a static file is limited, outside /api/ too (the health check
	// and the setup page read the database; an unknown path is the catch-all's 404).
	// Each from its own address, so each path has to spend the burst itself.
	for i, path := range []string{"/api/v1/server", "/healthz", "/setup", "/wp-login.php"} {
		ip := "198.51.100." + strconv.Itoa(i+1)
		limited := false
		for range 250 {
			if e.get(ip, path, "") == http.StatusTooManyRequests {
				limited = true
				break
			}
		}
		if !limited {
			t.Fatalf("%s is no longer rate limited", path)
		}
	}
}

// A cover grid is one request per cover: a burst well past the general budget,
// from one address with a valid token, is never refused, and spends none of the
// general budget the same address's API calls need.
func TestRateLimitMediaBurstAllowed(t *testing.T) {
	e := newRouteLimitEnv(t)
	libID, _ := seedCovers(t, e.testEnv)
	tok := e.session(t)
	const ip = "192.0.2.10"
	for i := range 300 {
		if code := e.get(ip, mediaURL(libID, "cover", "Sidecar", tok), ""); code != http.StatusOK {
			t.Fatalf("cover %d = %d, want 200", i, code)
		}
	}
	for i := range 5 {
		if code := e.get(ip, mediaURL(libID, "stream", "Sidecar/book.m4b", tok), ""); code == http.StatusTooManyRequests {
			t.Fatalf("stream %d was rate limited", i)
		}
	}
	for i := range 200 {
		if code := e.get(ip, "/api/v1/me", tok); code != http.StatusOK {
			t.Fatalf("API call %d after the media burst = %d, want 200", i, code)
		}
	}
}

// Media without a valid token is bounded like the general API: every failure
// spends from the address's general bucket, and once it is empty the address's
// media is refused before any token lookup, valid token or not. Another address
// is unaffected.
func TestRateLimitUnauthenticatedMediaBounded(t *testing.T) {
	e := newRouteLimitEnv(t)
	libID, _ := seedCovers(t, e.testEnv)
	tok := e.session(t)

	for _, tc := range []struct{ name, ip, token string }{
		{"bad token", "192.0.2.20", "not-a-token"},
		{"no token", "192.0.2.21", ""},
	} {
		path := mediaURL(libID, "cover", "Sidecar", tc.token)
		for i := range 200 {
			if code := e.get(tc.ip, path, ""); code != http.StatusUnauthorized {
				t.Fatalf("%s: request %d = %d, want 401", tc.name, i, code)
			}
		}
		if code := e.get(tc.ip, path, ""); code != http.StatusTooManyRequests {
			t.Fatalf("%s: request past the burst = %d, want 429", tc.name, code)
		}
		if code := e.get(tc.ip, mediaURL(libID, "cover", "Sidecar", tok), ""); code != http.StatusTooManyRequests {
			t.Fatalf("%s: valid media from the throttled address = %d, want 429", tc.name, code)
		}
		if code := e.get(tc.ip, "/api/v1/server", ""); code != http.StatusTooManyRequests {
			t.Fatalf("%s: API from the throttled address = %d, want 429", tc.name, code)
		}
	}
	if code := e.get("192.0.2.22", mediaURL(libID, "cover", "Sidecar", tok), ""); code != http.StatusOK {
		t.Fatalf("media from another address = %d, want 200", code)
	}
}

// Authenticated media has a bucket per credential: one token past it is refused,
// while another token from the same address keeps its own.
func TestRateLimitMediaPerCredential(t *testing.T) {
	e := newRouteLimitEnv(t)
	e.api.mediaLimiter.burst = 3
	libID, _ := seedCovers(t, e.testEnv)
	first, second := e.session(t), e.session(t)
	const ip = "192.0.2.30"

	for i := range 3 {
		if code := e.get(ip, mediaURL(libID, "cover", "Sidecar", first), ""); code != http.StatusOK {
			t.Fatalf("cover %d = %d, want 200", i, code)
		}
	}
	if code := e.get(ip, mediaURL(libID, "cover", "Sidecar", first), ""); code != http.StatusTooManyRequests {
		t.Fatalf("cover past the credential's burst = %d, want 429", code)
	}
	if code := e.get(ip, mediaURL(libID, "cover", "Sidecar", second), ""); code != http.StatusOK {
		t.Fatalf("another credential from the same address = %d, want 200", code)
	}
}

// A request its credential's media bucket refuses has already cost a token
// lookup, so it pays into the address's general bucket: a client hammering past
// its media budget soon has its address refused before any lookup, while another
// address is unaffected.
func TestRateLimitMediaPastBudgetBounded(t *testing.T) {
	e := newRouteLimitEnv(t)
	e.api.mediaLimiter.burst = 3
	libID, _ := seedCovers(t, e.testEnv)
	tok := e.session(t)
	const ip = "192.0.2.35"
	cover := mediaURL(libID, "cover", "Sidecar", tok)

	for i := range 3 {
		if code := e.get(ip, cover, ""); code != http.StatusOK {
			t.Fatalf("cover %d = %d, want 200", i, code)
		}
	}
	for i := range 200 {
		if code := e.get(ip, cover, ""); code != http.StatusTooManyRequests {
			t.Fatalf("cover %d past the credential's burst = %d, want 429", i, code)
		}
	}
	if code := e.get(ip, "/api/v1/me", tok); code != http.StatusTooManyRequests {
		t.Fatalf("API from an address that went past its media budget = %d, want 429", code)
	}
	if code := e.get("192.0.2.36", "/api/v1/me", tok); code != http.StatusOK {
		t.Fatalf("API from another address = %d, want 200", code)
	}
}

// The general API allows a burst of 200 per address, then refuses; another
// address has its own bucket.
func TestRateLimitGeneralBurst(t *testing.T) {
	e := newRouteLimitEnv(t)
	tok := e.session(t)
	const ip = "192.0.2.40"
	for i := range 200 {
		if code := e.get(ip, "/api/v1/me", tok); code != http.StatusOK {
			t.Fatalf("API call %d = %d, want 200", i, code)
		}
	}
	for range 10 {
		if code := e.get(ip, "/api/v1/me", tok); code != http.StatusTooManyRequests {
			t.Fatalf("API call past the burst = %d, want 429", code)
		}
	}
	if code := e.get("192.0.2.41", "/api/v1/me", tok); code != http.StatusOK {
		t.Fatalf("API call from another address = %d, want 200", code)
	}
}

// The brute-force lockouts are unchanged by the route classes: ten failed
// sign-ins or redemptions lock an address out (the right secret included), and
// another address is unaffected.
func TestRateLimitAuthLockoutsUnchanged(t *testing.T) {
	e := newRouteLimitEnv(t)
	for _, tc := range []struct{ name, ip, other, path, bad, good string }{
		{"login", "192.0.2.50", "192.0.2.51", "/api/v1/auth/login",
			`{"username":"admin","password":"wrong-password"}`,
			`{"username":"admin","password":"admin-password"}`},
		{"redeem", "192.0.2.52", "192.0.2.53", "/api/v1/auth/redeem",
			`{"code":"not-a-code"}`, `{"code":"` + e.authCode + `"}`},
	} {
		if code := e.post(tc.ip, tc.path, tc.good); code != http.StatusOK {
			t.Fatalf("%s: right secret = %d, want 200", tc.name, code)
		}
		for i := range 10 {
			if code := e.post(tc.ip, tc.path, tc.bad); code == http.StatusTooManyRequests || code == http.StatusOK {
				t.Fatalf("%s: failure %d = %d, want a refusal short of the lockout", tc.name, i+1, code)
			}
		}
		if code := e.post(tc.ip, tc.path, tc.good); code != http.StatusTooManyRequests {
			t.Fatalf("%s: right secret from a locked-out address = %d, want 429", tc.name, code)
		}
		if code := e.post(tc.other, tc.path, tc.good); code != http.StatusOK {
			t.Fatalf("%s: right secret from another address = %d, want 200", tc.name, code)
		}
	}
}
