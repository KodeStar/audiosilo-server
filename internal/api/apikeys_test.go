package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
)

// mintAPIKey POSTs /auth/tokens with sessionTok and returns the new key's id and
// plaintext secret, asserting the 200 create-response shape along the way.
func (e *testEnv) mintAPIKey(t *testing.T, sessionTok, label string) (int64, string) {
	t.Helper()
	resp, b := e.do(t, "POST", "/api/v1/auth/tokens", sessionTok, `{"label":"`+label+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint api key = %d %s, want 200", resp.StatusCode, b)
	}
	var out struct {
		Token  string `json:"token"`
		APIKey struct {
			ID        int64   `json:"id"`
			Label     string  `json:"label"`
			CreatedAt string  `json:"created_at"`
			LastSeen  *string `json:"last_seen"`
		} `json:"api_key"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("decode mint response: %v (%s)", err, b)
	}
	if out.Token == "" || out.APIKey.ID == 0 || out.APIKey.Label != label || out.APIKey.CreatedAt == "" {
		t.Fatalf("bad mint response: %s", b)
	}
	if out.APIKey.LastSeen != nil {
		t.Fatalf("a freshly minted key should carry last_seen=null: %s", b)
	}
	return out.APIKey.ID, out.Token
}

// TestAPIKeyMintAndUseOnAdminStats is the acceptance target: an admin mints a key
// and it authenticates GET /admin/stats as a plain Bearer credential.
func TestAPIKeyMintAndUseOnAdminStats(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	_, key := e.mintAPIKey(t, adminTok, "Heimdall dashboard")
	if resp, b := e.do(t, "GET", "/api/v1/admin/stats", key, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin stats with api key = %d %s, want 200", resp.StatusCode, b)
	}
}

// TestAPIKeyRevokedRejected: a revoked key no longer authenticates (allowed
// before, 401 after).
func TestAPIKeyRevokedRejected(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	id, key := e.mintAPIKey(t, adminTok, "temp")
	if resp, _ := e.do(t, "GET", "/api/v1/me", key, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("api key should authenticate before revocation, got %d", resp.StatusCode)
	}
	del := "/api/v1/auth/tokens/" + strconv.FormatInt(id, 10)
	if resp, _ := e.do(t, "DELETE", del, adminTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke own api key = %d, want 204", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", key, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked api key = %d, want 401", resp.StatusCode)
	}
	// Revoking again is a 404 (it is gone).
	if resp, _ := e.do(t, "DELETE", del, adminTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("double revoke = %d, want 404", resp.StatusCode)
	}
}

// TestPairingTokenNotAcceptedAsAuth: a pairing token is pairing-only and must be
// rejected on a requireAuth route (it is not a durable credential).
func TestPairingTokenNotAcceptedAsAuth(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	ptok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindPairing, "", 10*time.Minute)
	if resp, _ := e.do(t, "GET", "/api/v1/me", ptok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pairing token on requireAuth = %d, want 401", resp.StatusCode)
	}
}

// TestAPIKeyRejectedOnExchange: an api key must NOT be accepted by /auth/exchange
// (that path is pairing-only).
func TestAPIKeyRejectedOnExchange(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	_, key := e.mintAPIKey(t, adminTok, "k")

	if status, b := e.exchangeToken(t, key, "device"); status != http.StatusUnauthorized {
		t.Fatalf("api key on exchange = %d %s, want 401", status, b)
	}
}

// TestAPIKeyNonAdminForbiddenOnAdmin: a non-admin's key authenticates normal
// routes but the admin role is still enforced (allowed on /me, 403 on /admin).
func TestAPIKeyNonAdminForbiddenOnAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	_, key := e.mintAPIKey(t, memberTok, "k")

	if resp, _ := e.do(t, "GET", "/api/v1/me", key, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("member key on /me = %d, want 200", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/stats", key, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member key on /admin/stats = %d, want 403", resp.StatusCode)
	}
}

// TestAPIKeyRevokeCrossUser404: a user cannot revoke another user's key by id.
func TestAPIKeyRevokeCrossUser404(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	adminKeyID, adminKey := e.mintAPIKey(t, adminTok, "admin key")

	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)

	del := "/api/v1/auth/tokens/" + strconv.FormatInt(adminKeyID, 10)
	if resp, _ := e.do(t, "DELETE", del, memberTok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user revoke = %d, want 404", resp.StatusCode)
	}
	// The admin's key is untouched.
	if resp, _ := e.do(t, "GET", "/api/v1/me", adminKey, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin key should still work after a failed cross-user revoke, got %d", resp.StatusCode)
	}
}

// TestAPIKeyDemoRefused: demo accounts cannot mint, list, or revoke API keys
// (403, matching the recovery/password refusal).
func TestAPIKeyDemoRefused(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	demo, _ := e.auth.CreateDemoUser(ctx, "demo_x")
	demoTok, _ := e.auth.IssueToken(ctx, demo.ID, auth.KindSession, "t", 0)

	if resp, _ := e.do(t, "POST", "/api/v1/auth/tokens", demoTok, `{"label":"x"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo mint = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/auth/tokens", demoTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo list = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/auth/tokens/1", demoTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo revoke = %d, want 403", resp.StatusCode)
	}
}

// TestAPIKeyEmptyLabelRejected: a blank/missing label is a 400 (a key must be
// named so the owner can tell them apart to revoke one).
func TestAPIKeyEmptyLabelRejected(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	if resp, _ := e.do(t, "POST", "/api/v1/auth/tokens", adminTok, `{"label":"   "}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("whitespace label = %d, want 400", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/tokens", adminTok, `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing label = %d, want 400", resp.StatusCode)
	}
}

// TestAPIKeyListScopedAndMetadataOnly: the list returns only the caller's live
// keys, newest first, and never a secret, hash, or another user's key.
func TestAPIKeyListScopedAndMetadataOnly(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	_, alphaSecret := e.mintAPIKey(t, adminTok, "alpha")
	betaID, betaSecret := e.mintAPIKey(t, adminTok, "beta")
	_, gammaSecret := e.mintAPIKey(t, adminTok, "gamma")
	// Revoke beta so it must be excluded from the list.
	if resp, _ := e.do(t, "DELETE", "/api/v1/auth/tokens/"+strconv.FormatInt(betaID, 10), adminTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke beta = %d, want 204", resp.StatusCode)
	}

	// A second user's key must not appear in the admin's list.
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	e.mintAPIKey(t, memberTok, "member key")

	resp, b := e.do(t, "GET", "/api/v1/auth/tokens", adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d %s, want 200", resp.StatusCode, b)
	}
	var out struct {
		APIKeys []struct {
			ID       int64   `json:"id"`
			Label    string  `json:"label"`
			LastSeen *string `json:"last_seen"`
		} `json:"api_keys"`
	}
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("decode list: %v (%s)", err, b)
	}
	// Only the two live keys, newest first (gamma then alpha).
	if len(out.APIKeys) != 2 || out.APIKeys[0].Label != "gamma" || out.APIKeys[1].Label != "alpha" {
		t.Fatalf("list scope/order wrong: %s", b)
	}
	// Metadata only: no secret leaks, no revoked key, no other user's key.
	for _, s := range []string{alphaSecret, betaSecret, gammaSecret, "member key", "beta", "token_hash"} {
		if strings.Contains(b, s) {
			t.Fatalf("list leaked %q: %s", s, b)
		}
	}
}

// TestServerInfoAdvertisesAPIKeys: the capability flag is advertised so clients
// can gate the API-keys UI on it.
func TestServerInfoAdvertisesAPIKeys(t *testing.T) {
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	if !strings.Contains(body, `"api_keys":true`) {
		t.Fatalf("/server missing api_keys capability: %s", body)
	}
}

// TestAPIKeyWorksAsMediaQueryToken: an api key rides in the media ?token= query
// param exactly like a session token (browser <audio> can't set headers).
func TestAPIKeyWorksAsMediaQueryToken(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if _, err := library.NewScanner(e.cat, "", nil).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	_, key := e.mintAPIKey(t, adminTok, "media")

	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)
	stream := libPath + "/stream?path=" + url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b") + "&token=" + key
	// No Authorization header: the api key authenticates via ?token= alone.
	if resp, _ := e.do(t, "GET", stream, "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("stream with api key as ?token= = %d, want 200", resp.StatusCode)
	}
}
