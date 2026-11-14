package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// The first-run admin (server#369): the start's hint ends with an admin's
// password sign-in, and not with a member's, a refused try or an auth code.
func TestAdminSignInEndsTheFirstRunHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newTestEnv(t)
	if err := e.auth.MarkFirstAdmin(ctx, e.adminID); err != nil {
		t.Fatal(err)
	}
	waiting := func() bool {
		t.Helper()
		u, err := e.auth.FirstAdminWaiting(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return u != nil
	}
	if !waiting() {
		t.Fatal("not waiting after MarkFirstAdmin")
	}
	if _, err := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"username":"member","password":"member-password"}`, // not an admin
		`{"username":"admin","password":"wrong-password"}`,   // refused
	} {
		e.do(t, "POST", "/api/v1/auth/login", "", body)
		if !waiting() {
			t.Fatalf("login %s ended the hint", body)
		}
	}
	if resp, body := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("redeem = %d %s", resp.StatusCode, body)
	}
	if !waiting() {
		t.Fatal("an auth code ended the hint: it is about the password")
	}
	if resp, body := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"admin-password"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login = %d %s", resp.StatusCode, body)
	}
	if waiting() {
		t.Fatal("still waiting after the admin signed in")
	}
}

// printedPassword is GET /admin/setup's printed_password for token.
func printedPassword(t *testing.T, e *testEnv, token string) bool {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/admin/setup", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/setup = %d %s", resp.StatusCode, body)
	}
	var env struct {
		PrintedPassword *bool `json:"printed_password"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || env.PrintedPassword == nil {
		t.Fatalf("no printed_password in %s (%v)", body, err)
	}
	return *env.PrintedPassword
}

// The Your account step's printed_password: true for the first-run admin until
// they set their own password (POST /auth/password, which also ends the hint);
// false for an admin with no first-run row (the --setup wizard's, an older
// install's) and for another admin. A password an admin resets for them (the
// console, reset-admin) is not their own choice.
func TestSetupPrintedPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	if printedPassword(t, e, adminTok) {
		t.Fatal("printed_password without a first-run admin")
	}
	if err := e.auth.MarkFirstAdmin(ctx, e.adminID); err != nil {
		t.Fatal(err)
	}
	if !printedPassword(t, e, adminTok) {
		t.Fatal("printed_password false for the first-run admin")
	}
	other, err := e.auth.CreateUser(ctx, "alex", "alex-password", auth.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	otherTok, _ := e.auth.IssueToken(ctx, other.ID, auth.KindSession, "t", 0)
	if printedPassword(t, e, otherTok) {
		t.Error("printed_password true for another admin")
	}
	pw := "reset-by-alex-1"
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(e.adminID, 10), otherTok, `{"password":"`+pw+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("reset = %d %s", resp.StatusCode, body)
	}
	adminTok, _ = e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if !printedPassword(t, e, adminTok) {
		t.Error("a password another admin set counted as the admin's own")
	}
	// A wrong current password: 401 with its own code (not a session that
	// ended), and nothing changes.
	resp, body := e.do(t, "POST", "/api/v1/auth/password", adminTok, `{"password":"my-own-password","current_password":"wrong"}`)
	wantCode(t, "wrong current password", resp, body, http.StatusUnauthorized, codeCurrentPasswordIncorrect)
	if !printedPassword(t, e, adminTok) {
		t.Fatal("a refused change replaced the printed password")
	}
	if resp, body := e.do(t, "POST", "/api/v1/auth/password", adminTok,
		`{"password":"my-own-password","current_password":"`+pw+`"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("own password = %d %s", resp.StatusCode, body)
	}
	if printedPassword(t, e, adminTok) {
		t.Error("printed_password still true after the admin set their own")
	}
	if u, _ := e.auth.FirstAdminWaiting(ctx); u != nil {
		t.Error("the hint is still waiting after the admin set their own password")
	}
}

// The console's People dialog changes the admin's own password through PATCH
// /admin/users/{id}: their own choice as much as POST /auth/password's, so the
// printed password is gone and the hint ends.
func TestOwnPasswordByPatchReplacesThePrintedOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	if err := e.auth.MarkFirstAdmin(ctx, e.adminID); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(e.adminID, 10), adminTok,
		`{"password":"my-own-password"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("own password = %d %s", resp.StatusCode, body)
	}
	if printedPassword(t, e, adminTok) {
		t.Error("printed_password still true after the admin set their own in People")
	}
	if u, _ := e.auth.FirstAdminWaiting(ctx); u != nil {
		t.Error("the hint is still waiting after the admin set their own password")
	}
}

// A member's own password change is no admin's: the hint stays.
func TestMemberPasswordKeepsTheHint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newTestEnv(t)
	if err := e.auth.MarkFirstAdmin(ctx, e.adminID); err != nil {
		t.Fatal(err)
	}
	_, memberTok := opsTokens(t, e)
	if resp, body := e.do(t, "POST", "/api/v1/auth/password", memberTok,
		`{"password":"new-member-password","current_password":"member-password"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("member password = %d %s", resp.StatusCode, body)
	}
	if u, _ := e.auth.FirstAdminWaiting(ctx); u == nil {
		t.Error("a member's password change ended the admin's hint")
	}
}

// PATCH /admin/users/{id} {username}: an admin renames an account (their own,
// in the Your account step), keeping its sessions; the new name signs in, the
// old one doesn't. Refused, changing nothing: a member (403), signed out (401),
// "" (400 username_required), another account's name (409 username_taken), an
// unknown account (404).
func TestRenameUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newTestEnv(t)
	adminTok, memberTok := opsTokens(t, e)
	path := "/api/v1/admin/users/" + strconv.FormatInt(e.adminID, 10)
	for _, tc := range []struct {
		name, token, path, body string
		status                  int
		code                    string
	}{
		{"a member", memberTok, path, `{"username":"boss"}`, http.StatusForbidden, ""},
		{"signed out", "", path, `{"username":"boss"}`, http.StatusUnauthorized, ""},
		{"empty", adminTok, path, `{"username":""}`, http.StatusBadRequest, codeUsernameRequired},
		{"taken", adminTok, path, `{"username":"member"}`, http.StatusConflict, codeUsernameTaken},
		{"unknown account", adminTok, "/api/v1/admin/users/99999", `{"username":"boss"}`, http.StatusNotFound, ""},
	} {
		resp, body := e.do(t, "PATCH", tc.path, tc.token, tc.body)
		if resp.StatusCode != tc.status || tc.code != "" && !strings.Contains(body, `"code":"`+tc.code+`"`) {
			t.Errorf("%s: %d %s, want %d %s", tc.name, resp.StatusCode, body, tc.status, tc.code)
		}
	}
	if u, _ := e.auth.GetUser(ctx, e.adminID); u.Username != "admin" {
		t.Fatalf("a refused rename renamed the admin to %q", u.Username)
	}

	resp, body := e.do(t, "PATCH", path, adminTok, `{"username":"kode"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"username":"kode"`) {
		t.Fatalf("rename = %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("the session ended with the rename: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"kode","password":"admin-password"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("the new name doesn't sign in: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"admin-password"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the old name still signs in: %d", resp.StatusCode)
	}
}

// regionChosen is GET /admin/settings' metadata.region_chosen.
func regionChosen(t *testing.T, e *testEnv, token string) bool {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/admin/settings", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/settings = %d %s", resp.StatusCode, body)
	}
	var s struct {
		Metadata struct {
			RegionChosen *bool `json:"region_chosen"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(body), &s); err != nil || s.Metadata.RegionChosen == nil {
		t.Fatalf("no metadata.region_chosen in %s (%v)", body, err)
	}
	return *s.Metadata.RegionChosen
}

// metadata.region_chosen (server#192): false on a new server, so the console may
// offer the nearest marketplace; true once an admin saves one ("No preference"
// too), but not after a refused save; true from config.yaml or the environment.
func TestSettingsRegionChosen(t *testing.T) {
	t.Run("saved here", func(t *testing.T) {
		t.Parallel()
		e := newTestEnv(t)
		adminTok, _ := opsTokens(t, e)
		if regionChosen(t, e, adminTok) {
			t.Fatal("region_chosen on a new server")
		}
		if resp, _ := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"region":"gb"}}`); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("an unknown marketplace = %d, want 400", resp.StatusCode)
		}
		if resp, _ := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"mode":"remote"}}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("another metadata setting = %d", resp.StatusCode)
		}
		if regionChosen(t, e, adminTok) {
			t.Fatal("region_chosen after a refused save or another setting")
		}
		if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"metadata":{"region":""}}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("No preference = %d %s", resp.StatusCode, body)
		}
		if !regionChosen(t, e, adminTok) {
			t.Fatal("No preference saved, yet not chosen")
		}
	})
	t.Run("config.yaml", func(t *testing.T) {
		t.Parallel()
		e := newTestEnvWith(t, func(c *config.Config) { c.Metadata.Region = "uk" })
		adminTok, _ := opsTokens(t, e)
		if !regionChosen(t, e, adminTok) {
			t.Fatal("a region from config.yaml isn't chosen")
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv("AUDIOSILO_METADATA_REGION", "de")
		cfg, _, err := config.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		e := newTestEnvWith(t, func(c *config.Config) { *c = *cfg })
		adminTok, _ := opsTokens(t, e)
		if !regionChosen(t, e, adminTok) {
			t.Fatal("a region from the environment isn't chosen")
		}
	})
}

// The setup flow's steps include the account step first and the phone last
// (server#369): PUT /admin/setup takes "account".
func TestSetupAccountStep(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _ := opsTokens(t, e)
	resp, body := e.do(t, "PUT", "/api/v1/admin/setup", adminTok, `{"steps":{"account":"done"}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"account":"done"`) {
		t.Fatalf("PUT account = %d %s", resp.StatusCode, body)
	}
}
