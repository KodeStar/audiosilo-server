package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// TestUnauthorizedMarked: every 401 the server writes carries
// X-AudioSilo-Unauthorized, the body's code when it has one, else
// "unauthorized", so a player behind an auth proxy can tell the server's
// refusal from the proxy's own 401 (WS-401-MARK). A cross-origin page may read
// it. Other refusals (403, 404) carry none.
func TestUnauthorizedMarked(t *testing.T) {
	t.Parallel()
	const origin = "https://player.example.com"
	e := newTestEnvWith(t, func(c *config.Config) { c.CORSOrigins = []string{origin} })
	user, err := e.auth.CreateUser(t.Context(), "listener", "listener-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	userTok := e.session(t, user.ID)

	cases := []struct {
		name, method, path, token, body string
		want                            string // the header's value; "" for none
		status                          int
	}{
		// The middleware's refusals (requireAuth, mediaAuth, requireFeedAuth, requireBridge).
		{"requireAuth no token", "GET", "/api/v1/me", "", "", unauthorizedPlain, 401},
		{"requireAuth refused token", "GET", "/api/v1/me", "not-a-token", "", unauthorizedPlain, 401},
		{"requireAdmin refused token", "GET", "/api/v1/admin/stats", "not-a-token", "", unauthorizedPlain, 401},
		{"mediaAuth ?token=", "GET", "/api/v1/libraries/1/cover?path=a&token=not-a-token", "", "", unauthorizedPlain, 401},
		{"mediaAuth no token", "GET", "/api/v1/libraries/1/stream?path=a", "", "", unauthorizedPlain, 401},
		{"requireFeedAuth", "GET", "/api/v1/me/upcoming/releases.ics?token=not-a-feed", "", "", unauthorizedPlain, 401},
		{"requireFeedAuth a session token", "GET", "/api/v1/me/upcoming/releases.ics", userTok, "", unauthorizedPlain, 401},
		{"requireBridge", "GET", "/api/v1/bridge/libraries/1/marker", "not-a-token", "", unauthorizedPlain, 401},
		{"requireBridge a session token", "GET", "/api/v1/bridge/libraries/1/marker", userTok, "", unauthorizedPlain, 401},
		// The handlers' own 401s.
		{"bridge pair wrong code", "POST", "/api/v1/bridge/pair", "", `{"code":"WRONG-WRONG"}`, unauthorizedPlain, 401},
		{"login wrong password", "POST", "/api/v1/auth/login", "", `{"username":"listener","password":"wrong"}`, unauthorizedPlain, 401},
		{"redeem unknown code", "POST", "/api/v1/auth/redeem", "", `{"code":"NOPE-NOPE"}`, unauthorizedPlain, 401},
		{"exchange unknown token", "POST", "/api/v1/auth/exchange", "", `{"pairing_token":"nope","device_name":"x"}`, unauthorizedPlain, 401},
		{"password change, wrong current", "POST", "/api/v1/auth/password", userTok,
			`{"password":"a-new-password-1","current_password":"wrong"}`, codeCurrentPasswordIncorrect, 401},
		{"oidc finish, no cookie", "POST", oidcFinishPath, "", `{"device_name":"x"}`, codeOIDCNoSignIn, 401},
		// Denied: not a 401, no mark.
		{"admin only (403)", "GET", "/api/v1/admin/stats", userTok, "", "", 403},
		{"signed in (200)", "GET", "/api/v1/me", userTok, "", "", 200},
	}
	for _, c := range cases {
		resp, body := e.doHeaders(t, c.method, c.path, c.token, c.body, map[string]string{"Origin": origin})
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, resp.StatusCode, body, c.status)
			continue
		}
		if got := resp.Header.Get(headerUnauthorized); got != c.want {
			t.Errorf("%s: %s = %q, want %q", c.name, headerUnauthorized, got, c.want)
		}
		if c.status == http.StatusUnauthorized {
			wantCORSList(t, resp.Header, "Access-Control-Expose-Headers", []string{"X-AudioSilo-Unauthorized"})
		}
	}
}

// TestUnauthorizedMarkWriters: the writers mark a 401 alone, with its code when
// it has one (writeErrorCode, writeErrorReason) and the plain value otherwise
// (writeError, writeSessionEnded, a 401 written by writeJSON).
func TestUnauthorizedMarkWriters(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		write func(http.ResponseWriter)
		want  string
	}{
		{"writeError", func(w http.ResponseWriter) { writeError(w, http.StatusUnauthorized, "no") }, unauthorizedPlain},
		{"writeErrorCode", func(w http.ResponseWriter) {
			writeErrorCode(w, http.StatusUnauthorized, codeInviteUsed, "used")
		}, codeInviteUsed},
		{"writeErrorReason", func(w http.ResponseWriter) {
			writeErrorReason(w, http.StatusUnauthorized, "some_code", "why", "no")
		}, "some_code"},
		{"writeErrorCode, empty code", func(w http.ResponseWriter) {
			writeErrorCode(w, http.StatusUnauthorized, "", "no")
		}, unauthorizedPlain},
		{"writeJSON", func(w http.ResponseWriter) { writeJSON(w, http.StatusUnauthorized, nil) }, unauthorizedPlain},
		{"writeSessionEnded", func(w http.ResponseWriter) { writeSessionEnded(w, auth.ErrInvalidToken) }, unauthorizedPlain},
		// Denied: other statuses carry no mark, a code or not.
		{"403 with a code", func(w http.ResponseWriter) {
			writeErrorCode(w, http.StatusForbidden, codeNoAccess, "no")
		}, ""},
		{"429", func(w http.ResponseWriter) { writeError(w, http.StatusTooManyRequests, "slow") }, ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		c.write(rec)
		if got := rec.Header().Get(headerUnauthorized); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
