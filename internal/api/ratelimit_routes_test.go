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
	for _, l := range []*rateLimiter{e.api.ipLimiter, e.api.tokenLimiter, e.api.mediaLimiter} {
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
	t.Parallel()
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
	t.Parallel()
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
	// The address's whole general budget is left: a call that spends from it
	// (no token, so it is limited by address) succeeds burst times.
	for i := range 200 {
		if code := e.get(ip, "/api/v1/server", ""); code != http.StatusOK {
			t.Fatalf("API call %d after the media burst = %d, want 200", i, code)
		}
	}
}

// Media without a valid token is bounded like the general API: every failure
// spends from the address's general bucket, and once it is empty the address's
// media is refused before any token lookup, valid token or not. Another address
// is unaffected.
func TestRateLimitUnauthenticatedMediaBounded(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A request nobody has authenticated has the general burst of 200 per address,
// then is refused; another address has its own bucket.
func TestRateLimitGeneralBurst(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	const ip = "192.0.2.40"
	for i := range 200 {
		if code := e.get(ip, "/api/v1/server", ""); code != http.StatusOK {
			t.Fatalf("API call %d = %d, want 200", i, code)
		}
	}
	for range 10 {
		if code := e.get(ip, "/api/v1/server", ""); code != http.StatusTooManyRequests {
			t.Fatalf("API call past the burst = %d, want 429", code)
		}
	}
	if code := e.get("192.0.2.41", "/api/v1/server", ""); code != http.StatusOK {
		t.Fatalf("API call from another address = %d, want 200", code)
	}
}

// An authenticated request spends from its credential's bucket, not its
// address's (server#246): one address sending far more than the address burst
// with valid tokens (a household behind NAT, a grid loading a whole library) is
// never refused, two tokens from it have a bucket each, and the address's own
// budget is untouched.
func TestRateLimitAuthenticatedPerToken(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	first, second := e.session(t), e.session(t)
	const ip = "192.0.2.42"
	for i := range 1000 {
		tok := first
		if i%2 == 1 {
			tok = second
		}
		if code := e.get(ip, "/api/v1/me", tok); code != http.StatusOK {
			t.Fatalf("authenticated call %d = %d, want 200", i, code)
		}
	}
	for i := range 200 {
		if code := e.get(ip, "/api/v1/server", ""); code != http.StatusOK {
			t.Fatalf("unauthenticated call %d after the authenticated ones = %d, want 200", i, code)
		}
	}
	// An API key is a credential like a session.
	_, key := e.mintAPIKey(t, first, "script")
	for i := range 300 {
		if code := e.get("192.0.2.43", "/api/v1/me", key); code != http.StatusOK {
			t.Fatalf("API key call %d = %d, want 200", i, code)
		}
	}
}

// The routes that limit themselves (the authenticated API, media) refuse an
// address whose general bucket is empty in rateLimit already, before the timeout
// layer behind it runs; another address goes through.
func TestRateLimitRefusesSpentAddressFirst(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	tok := e.session(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/me", e.api.requireAuth(ok))
	mux.Handle("GET /api/v1/libraries/{id}/cover", e.api.requireBookMediaAuth(ok))
	reached := 0
	h := e.api.rateLimit(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		mux.ServeHTTP(w, r)
	}))
	send := func(ip, path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = ip + ":1234"
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	const spent = "192.0.2.70"
	for range 200 {
		e.api.ipLimiter.Allow(spent)
	}
	for _, path := range []string{"/api/v1/me", "/api/v1/libraries/1/cover?path=x"} {
		if code := send(spent, path); code != http.StatusTooManyRequests || reached != 0 {
			t.Fatalf("%s from a spent address = %d, %d passed on; want 429 before passing it on", path, code, reached)
		}
		if code := send("192.0.2.71", path); code != http.StatusNoContent || reached != 1 {
			t.Fatalf("%s from another address = %d, %d passed on; want 204, passed on", path, code, reached)
		}
		reached = 0
	}
}

// A credential past its own budget is refused while another from the same
// address carries on; each refusal has cost a lookup, so it pays into the
// address's bucket, and a client hammering on soon has its address refused
// before any lookup (another address unaffected).
func TestRateLimitTokenBudget(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	e.api.tokenLimiter.burst = 5
	first, second := e.session(t), e.session(t)
	const ip = "192.0.2.44"
	for i := range 5 {
		if code := e.get(ip, "/api/v1/me", first); code != http.StatusOK {
			t.Fatalf("call %d = %d, want 200", i, code)
		}
	}
	if code := e.get(ip, "/api/v1/me", first); code != http.StatusTooManyRequests {
		t.Fatalf("call past the credential's burst = %d, want 429", code)
	}
	if code := e.get(ip, "/api/v1/me", second); code != http.StatusOK {
		t.Fatalf("another credential from the same address = %d, want 200", code)
	}
	for i := range 200 {
		if code := e.get(ip, "/api/v1/me", first); code != http.StatusTooManyRequests {
			t.Fatalf("call %d past the credential's burst = %d, want 429", i, code)
		}
	}
	if code := e.get(ip, "/api/v1/me", second); code != http.StatusTooManyRequests {
		t.Fatalf("another credential from an address that went past its budget = %d, want 429", code)
	}
	if code := e.get(ip, "/api/v1/server", ""); code != http.StatusTooManyRequests {
		t.Fatalf("unauthenticated call from that address = %d, want 429", code)
	}
	if code := e.get("192.0.2.45", "/api/v1/me", second); code != http.StatusOK {
		t.Fatalf("from another address = %d, want 200", code)
	}
}

// A token is proved before it is a bucket of its own: a missing or refused one
// pays into its address's bucket, so guessing tokens is bounded by the general
// limit, and once the address is spent even a valid token from it is refused
// before the lookup. Another address is unaffected.
func TestRateLimitBadTokensChargeAddress(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	tok := e.session(t)
	for _, tc := range []struct{ name, ip, token string }{
		{"bad token", "192.0.2.46", "not-a-token"},
		{"no token", "192.0.2.47", ""},
	} {
		for i := range 200 {
			if code := e.get(tc.ip, "/api/v1/me", tc.token); code != http.StatusUnauthorized {
				t.Fatalf("%s: request %d = %d, want 401", tc.name, i, code)
			}
		}
		if code := e.get(tc.ip, "/api/v1/me", tc.token); code != http.StatusTooManyRequests {
			t.Fatalf("%s: request past the burst = %d, want 429", tc.name, code)
		}
		if code := e.get(tc.ip, "/api/v1/me", tok); code != http.StatusTooManyRequests {
			t.Fatalf("%s: a valid token from the throttled address = %d, want 429", tc.name, code)
		}
		if code := e.get(tc.ip, "/api/v1/admin/libraries", tok); code != http.StatusTooManyRequests {
			t.Fatalf("%s: an admin route from the throttled address = %d, want 429", tc.name, code)
		}
	}
	if code := e.get("192.0.2.48", "/api/v1/me", tok); code != http.StatusOK {
		t.Fatalf("a valid token from another address = %d, want 200", code)
	}
}

// The sign-in, pairing and account routes stay limited by address with a valid
// token on them, and spend none of the token's budget; a public route stays
// limited by address whatever header it carries. A token can't buy more sign-in
// attempts, pairing codes or API keys than its address has.
func TestRateLimitAuthRoutesByAddress(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	tok := e.session(t)
	// Room for the three checks from another address only: one request on these
	// routes spent from the token's budget would leave one of them refused.
	e.api.tokenLimiter.burst = 3
	for _, tc := range []struct{ name, ip, method, path, body string }{
		{"list keys", "192.0.2.60", "GET", "/api/v1/auth/tokens", ""},
		{"pair", "192.0.2.61", "POST", "/api/v1/auth/pair", "{}"},
		{"public with a token", "192.0.2.63", "GET", "/api/v1/server", ""},
	} {
		// Exactly the address's burst, each charged once (not again per token).
		for i := range 201 {
			code := e.serve(tc.ip, tc.method, tc.path, tok, tc.body)
			if limited := code == http.StatusTooManyRequests; limited != (i == 200) {
				t.Fatalf("%s: request %d with a valid token = %d; want 429 from the 201st only", tc.name, i+1, code)
			}
		}
		if code := e.get(tc.ip, "/api/v1/server", ""); code != http.StatusTooManyRequests {
			t.Fatalf("%s: the address's budget was not spent (%d)", tc.name, code)
		}
		// The token's own budget is whole: from another address it still works.
		if code := e.get("198.51.100.60", "/api/v1/me", tok); code != http.StatusOK {
			t.Fatalf("%s: the token from another address = %d, want 200", tc.name, code)
		}
	}
	// Sign-in is refused, token or not, once its address has spent its budget.
	const ip = "192.0.2.62"
	for range 200 {
		e.get(ip, "/api/v1/server", "")
	}
	if code := e.serve(ip, "POST", "/api/v1/auth/login", tok, `{"username":"admin","password":"admin-password"}`); code != http.StatusTooManyRequests {
		t.Fatalf("sign-in with a token from a spent address = %d, want 429", code)
	}
}

// Sign-in's brute-force lockout holds with a valid token on every attempt: ten
// wrong passwords lock the address out, the right one included.
func TestRateLimitLoginLockoutWithToken(t *testing.T) {
	t.Parallel()
	e := newRouteLimitEnv(t)
	tok := e.session(t)
	const ip = "192.0.2.64"
	for i := range 10 {
		code := e.serve(ip, "POST", "/api/v1/auth/login", tok, `{"username":"admin","password":"wrong-password"}`)
		if code == http.StatusOK || code == http.StatusTooManyRequests {
			t.Fatalf("failure %d = %d, want a refusal short of the lockout", i+1, code)
		}
	}
	if code := e.serve(ip, "POST", "/api/v1/auth/login", tok, `{"username":"admin","password":"admin-password"}`); code != http.StatusTooManyRequests {
		t.Fatalf("right password from a locked-out address, with a token = %d, want 429", code)
	}
}

// The brute-force lockouts are unchanged by the route classes: ten failed
// sign-ins or redemptions lock an address out (the right secret included), and
// another address is unaffected.
func TestRateLimitAuthLockoutsUnchanged(t *testing.T) {
	t.Parallel()
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
