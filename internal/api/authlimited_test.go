package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// TestAuthLimitedBothWrappers: requireAuth and mediaAuth run one sequence
// (authLimited, server#252), and each still takes and refuses what it did. A
// session or API key in the header is let through by both; ?token= only by
// media; a missing, unknown or pairing token is a 401 that charges the address,
// so once the address is spent both refuse a valid token with 429 and
// Retry-After before it is looked up. A credential past its own bucket is a 429
// with Retry-After, and a media answer to it under cors_origins still carries
// the cross-origin resource policy.
func TestAuthLimitedBothWrappers(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) { c.CORSOrigins = []string{"http://localhost:8081"} })
	ctx := context.Background()
	now := time.Now()
	for _, l := range []*rateLimiter{e.api.ipLimiter, e.api.tokenLimiter, e.api.mediaLimiter} {
		l.now = func() time.Time { return now }
		l.burst, l.rate = 3, 0.5 // 2 s to the next token
	}
	session, err := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	pairing, err := e.auth.IssueToken(ctx, e.adminID, auth.KindPairing, "", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, key := e.mintAPIKey(t, session, "script")
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if userFrom(r.Context()) == nil {
			t.Error("handler reached without a user")
		}
		w.WriteHeader(http.StatusOK)
	})
	wrappers := map[string]http.Handler{
		"requireAuth":      e.api.requireAuth(ok),
		"requireMediaAuth": e.api.requireMediaAuth(ok),
	}
	send := func(h http.Handler, ip, header, query string) *httptest.ResponseRecorder {
		target := "/api/v1/libraries/1/cover"
		if query != "" {
			target += "?token=" + escape(query)
		}
		r := httptest.NewRequest("GET", target, nil)
		r.RemoteAddr = ip + ":1234"
		if header != "" {
			r.Header.Set("Authorization", "Bearer "+header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	for name, h := range wrappers {
		media := name == "requireMediaAuth"
		// Allowed: a session and an API key in the header (each its own bucket).
		for _, tok := range []string{session, key} {
			if rec := send(h, "192.0.2.80", tok, ""); rec.Code != http.StatusOK {
				t.Fatalf("%s: header credential = %d, want 200", name, rec.Code)
			}
		}
		// ?token= is media's alone.
		want := http.StatusUnauthorized
		if media {
			want = http.StatusOK
		}
		if rec := send(h, "192.0.2.81", "", session); rec.Code != want {
			t.Fatalf("%s: ?token= = %d, want %d", name, rec.Code, want)
		}
		// Denied: missing, unknown and pairing tokens are 401s paid by the address.
		ip := map[bool]string{false: "192.0.2.82", true: "192.0.2.83"}[media]
		for _, bad := range []string{"", "not-a-token", pairing} {
			if rec := send(h, ip, bad, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: bad token %q = %d, want 401", name, bad, rec.Code)
			}
		}
		rec := send(h, ip, session, "")
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: a valid token from the spent address = %d, Retry-After %q; want 429 with one",
				name, rec.Code, rec.Header().Get("Retry-After"))
		}
	}

	// A credential past its own bucket: 429 + Retry-After from both, the media
	// answer keeping its CORP header. A fresh session, so its buckets are full.
	fresh, err := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t2", 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range wrappers {
		ip := map[string]string{"requireAuth": "192.0.2.84", "requireMediaAuth": "192.0.2.85"}[name]
		for i := range 3 {
			if rec := send(h, ip, fresh, ""); rec.Code != http.StatusOK {
				t.Fatalf("%s: request %d within the credential's burst = %d", name, i, rec.Code)
			}
		}
		rec := send(h, ip, fresh, "")
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "2" {
			t.Fatalf("%s: past the credential's burst = %d, Retry-After %q; want 429, 2", name, rec.Code, rec.Header().Get("Retry-After"))
		}
		corp := rec.Header().Get("Cross-Origin-Resource-Policy")
		if name == "requireMediaAuth" && corp != "cross-origin" {
			t.Fatalf("media 429 under cors_origins: CORP %q, want cross-origin", corp)
		}
	}
}
