package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// POST /auth/pair takes an optional ttl_seconds (D7): the console's Listen in
// player asks for 120, so a token left in a tab's URL and history dies within
// two minutes. Clamped to [30 s, 10 min]: a caller can shorten a token's life,
// never lengthen it. No body is today's 10 minutes; a malformed body, an API
// key and no token are refused.
func TestPairTTL(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	tok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)

	// expiry is how long from now the newest pairing token lives.
	expiry := func(body string) time.Duration {
		t.Helper()
		resp, out := e.do(t, "POST", "/api/v1/auth/pair", tok, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("pair %q = %d %s", body, resp.StatusCode, out)
		}
		var p struct {
			PairingToken string `json:"pairing_token"`
		}
		if err := json.Unmarshal([]byte(out), &p); err != nil || p.PairingToken == "" {
			t.Fatalf("pair %q: no token in %s", body, out)
		}
		var at string
		if err := e.db.QueryRowContext(ctx,
			`SELECT expires_at FROM tokens WHERE user_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
			member.ID, auth.KindPairing).Scan(&at); err != nil {
			t.Fatal(err)
		}
		when, err := time.Parse(time.RFC3339, at)
		if err != nil {
			t.Fatalf("expires_at %q: %v", at, err)
		}
		return time.Until(when)
	}
	near := func(got, want time.Duration) bool {
		return got > want-5*time.Second && got <= want+time.Second
	}

	// Allowed: no body, the console's 120, and clamping at both ends.
	for _, c := range []struct {
		body string
		want time.Duration
	}{
		{"", 10 * time.Minute},
		{`{}`, 10 * time.Minute},
		{`{"ttl_seconds":120}`, 2 * time.Minute},
		{`{"ttl_seconds":5}`, 30 * time.Second},
		{`{"ttl_seconds":-1}`, 30 * time.Second},
		{`{"ttl_seconds":86400}`, 10 * time.Minute},
		{`{"ttl_seconds":9223372036854775807}`, 10 * time.Minute},
	} {
		if got := expiry(c.body); !near(got, c.want) {
			t.Errorf("pair %s: lives %v, want about %v", c.body, got, c.want)
		}
	}

	// Denied: a malformed body, an unknown field, an API key, no token.
	for _, body := range []string{`{"ttl_seconds":"soon"}`, `{"ttl":60}`, `{`} {
		if resp, out := e.do(t, "POST", "/api/v1/auth/pair", tok, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("pair %s = %d %s, want 400", body, resp.StatusCode, out)
		}
	}
	key, _ := e.auth.IssueToken(ctx, member.ID, auth.KindAPI, "key", 0)
	if resp, _ := e.do(t, "POST", "/api/v1/auth/pair", key, `{"ttl_seconds":120}`); resp.StatusCode != http.StatusForbidden {
		t.Errorf("pair with an API key = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/pair", "", `{"ttl_seconds":120}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("pair with no token = %d, want 401", resp.StatusCode)
	}
}
