package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
)

// samKinds is sam's live credentials of every kind a person holds (server#281):
// the phone (castEnv's samTok) and a laptop session, a cast token minted from
// each, an API key and the calendar feed token.
type samKinds struct {
	phone, laptop, castPhone, castLaptop, key, feed string
}

func mintSamKinds(t *testing.T, c *castEnv) samKinds {
	t.Helper()
	ctx := context.Background()
	laptop, err := c.auth.IssueToken(ctx, c.sam, auth.KindSession, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	k := samKinds{phone: c.samTok, laptop: laptop, castPhone: c.castToken(t, cradleBook)}
	status, body := c.mint(t, laptop, c.lib, cradleBook)
	var out struct {
		Token string `json:"token"`
	}
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &out) != nil || out.Token == "" {
		t.Fatalf("mint from the laptop = %d %s", status, body)
	}
	k.castLaptop = out.Token
	if k.key, _, err = c.auth.IssueAPIToken(ctx, c.sam, "script"); err != nil {
		t.Fatal(err)
	}
	if k.feed, _, err = c.auth.IssueFeedToken(ctx, c.sam, 0); err != nil {
		t.Fatal(err)
	}
	// Every credential used once: the token cache holds the ones it can.
	k.check(t, c, true, true)
	return k
}

// check asserts over HTTP which of sam's credentials still work: each session
// as a bearer and as a media ?token=, the cast token minted from it on the
// stream, and the API key and the feed token always.
func (k samKinds) check(t *testing.T, c *castEnv, phoneLive, laptopLive bool) {
	t.Helper()
	status := func(live bool) int {
		if live {
			return http.StatusOK
		}
		return http.StatusUnauthorized
	}
	want := func(what string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", what, got, want)
		}
	}
	me := func(tok string) int {
		resp, _ := c.do(t, "GET", "/api/v1/me", tok, "")
		return resp.StatusCode
	}
	stream := func(tok string) int { return c.media(t, c.lib, "stream", cradlePart, tok, "").StatusCode }
	for _, s := range []struct {
		name       string
		tok, cast  string
		wantStatus int
	}{
		{"phone", k.phone, k.castPhone, status(phoneLive)},
		{"laptop", k.laptop, k.castLaptop, status(laptopLive)},
	} {
		want("the "+s.name+"'s session", me(s.tok), s.wantStatus)
		want("the "+s.name+"'s session as ?token=", stream(s.tok), s.wantStatus)
		want("the "+s.name+"'s cast token", stream(s.cast), s.wantStatus)
	}
	want("the API key", me(k.key), http.StatusOK)
	want("the API key as ?token=", stream(k.key), http.StatusOK)
	resp, _ := c.do(t, "GET", feedPath+"?token="+url.QueryEscape(k.feed), "", "")
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("the feed token = 401, want it accepted")
	}
}

// bridgeAccepted reports whether a bridge token still authenticates on a
// Bridge route (anything but 401: the route's own answer doesn't matter).
func bridgeAccepted(t *testing.T, e *testEnv, tok string, lib int64) bool {
	t.Helper()
	resp, _ := e.do(t, "GET", "/api/v1/bridge/libraries/"+strconv.FormatInt(lib, 10)+"/marker", tok, "")
	return resp.StatusCode != http.StatusUnauthorized
}

func pairTestBridge(t *testing.T, e *testEnv) string {
	t.Helper()
	ctx := context.Background()
	code, _, err := e.auth.IssueBridgePairCode(ctx, e.adminID)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.auth.ExchangeBridgeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Security-critical (server#281): changing your own password signs out your
// other sessions and their cast tokens on the very next request; the device you
// changed it on, your API keys and calendar link keep working, and nobody
// else's sessions move.
func TestSelfPasswordChangeEndsOtherSessions(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	k := mintSamKinds(t, c)

	resp, body := c.do(t, "POST", "/api/v1/auth/password", k.phone,
		`{"password":"a-new-password","current_password":"sam-password"}`)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change = %d %s, want 204", resp.StatusCode, body)
	}
	k.check(t, c, true, false)
	if resp, _ := c.do(t, "GET", "/api/v1/me", c.kidTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("another person's session = %d, want 200", resp.StatusCode)
	}

	// Denied: a refused change (the wrong current password) ends nothing.
	k2 := mintSamKinds(t, c)
	resp, _ = c.do(t, "POST", "/api/v1/auth/password", k2.phone,
		`{"password":"third-password","current_password":"wrong-password"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("change with the wrong password = %d, want 401", resp.StatusCode)
	}
	k2.check(t, c, true, true)
}

// A first password replaces none: setting it signs nobody out.
func TestFirstPasswordEndsNoSession(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	u, _ := e.auth.CreateUser(ctx, "paired", "", auth.RoleUser)
	phone, _ := e.auth.IssueToken(ctx, u.ID, auth.KindSession, "phone", 0)
	tablet, _ := e.auth.IssueToken(ctx, u.ID, auth.KindSession, "tablet", 0)
	if resp, body := e.do(t, "POST", "/api/v1/auth/password", phone, `{"password":"first-password"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first password = %d %s, want 204", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/me", tablet, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the tablet after a first password = %d, want 200", resp.StatusCode)
	}
}

// Security-critical: an admin's reset ends every session of that person and
// their cast tokens; their API key and calendar link stay. An admin resetting
// their own keeps the console they did it from, ends their other sessions and
// leaves the Bridge paired.
func TestAdminPasswordResetEndsSessions(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	k := mintSamKinds(t, c)
	ctx := context.Background()
	console := adminSession(t, c.testEnv)
	other, _ := c.auth.IssueToken(ctx, c.adminID, auth.KindSession, "admin's phone", 0)
	bridgeTok := pairTestBridge(t, c.testEnv)

	resp, body := c.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(c.sam, 10), console, `{"password":"reset-by-admin"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset = %d %s, want 200", resp.StatusCode, body)
	}
	k.check(t, c, false, false)
	if resp, _ := c.do(t, "GET", "/api/v1/me", other, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the admin's own phone after resetting sam = %d, want 200", resp.StatusCode)
	}

	// The admin's own password: the console stays, the phone ends, the Bridge stays.
	resp, body = c.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(c.adminID, 10), console, `{"password":"admins-new-password"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("own reset = %d %s, want 200", resp.StatusCode, body)
	}
	if resp, _ := c.do(t, "GET", "/api/v1/admin/users", console, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the console after its own reset = %d, want 200", resp.StatusCode)
	}
	if resp, _ := c.do(t, "GET", "/api/v1/me", other, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the admin's other session = %d, want 401", resp.StatusCode)
	}
	if !bridgeAccepted(t, c.testEnv, bridgeTok, c.lib) {
		t.Fatal("the bridge token was refused after its admin's password reset")
	}

	// A change that sets no password ends nothing.
	c.samTok, _ = c.auth.IssueToken(ctx, c.sam, auth.KindSession, "new phone", 0)
	k2 := mintSamKinds(t, c)
	if resp, body := c.do(t, "PATCH", "/api/v1/admin/users/"+strconv.FormatInt(c.sam, 10), console, `{"can_add_members":false}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	k2.check(t, c, true, true)
}

// Security-critical: Sign out everywhere is admin-only, ends every session of
// the person (keeping the admin's own console on their own account) with the
// cast tokens minted from them, and nothing of another kind.
func TestSignOutEverywhereRoute(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	ctx := context.Background()
	console := adminSession(t, c.testEnv)
	samPath := "/api/v1/admin/users/" + strconv.FormatInt(c.sam, 10) + "/sign-out"
	k := mintSamKinds(t, c)

	// Denied: not an admin (sam on their own account), no credential, a bad
	// or unknown id; none of them ends anything.
	if resp, _ := c.do(t, "POST", samPath, k.phone, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("as sam = %d, want 403", resp.StatusCode)
	}
	if resp, _ := c.do(t, "POST", samPath, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credential = %d, want 401", resp.StatusCode)
	}
	if resp, _ := c.do(t, "POST", "/api/v1/admin/users/x/sign-out", console, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", resp.StatusCode)
	}
	if resp, _ := c.do(t, "POST", "/api/v1/admin/users/9999/sign-out", console, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", resp.StatusCode)
	}
	k.check(t, c, true, true)

	resp, body := c.do(t, "POST", samPath, console, "")
	var out struct {
		SignedOut *int `json:"signed_out"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.SignedOut == nil || *out.SignedOut != 2 {
		t.Fatalf("sign out everywhere = %d %s, want 200 signed_out 2", resp.StatusCode, body)
	}
	k.check(t, c, false, false)
	if resp, _ := c.do(t, "GET", "/api/v1/me", c.kidTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("another person's session = %d, want 200", resp.StatusCode)
	}

	// Their own account: the console stays, the other session ends, the Bridge stays.
	other, _ := c.auth.IssueToken(ctx, c.adminID, auth.KindSession, "admin's phone", 0)
	bridgeTok := pairTestBridge(t, c.testEnv)
	resp, body = c.do(t, "POST", "/api/v1/admin/users/"+strconv.FormatInt(c.adminID, 10)+"/sign-out", console, "")
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || *out.SignedOut != 1 {
		t.Fatalf("own sign out everywhere = %d %s, want 200 signed_out 1", resp.StatusCode, body)
	}
	if resp, _ := c.do(t, "GET", "/api/v1/admin/users", console, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the console after signing its account out everywhere = %d, want 200", resp.StatusCode)
	}
	if resp, _ := c.do(t, "GET", "/api/v1/me", other, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the admin's other session = %d, want 401", resp.StatusCode)
	}
	if !bridgeAccepted(t, c.testEnv, bridgeTok, c.lib) {
		t.Fatal("the bridge token was refused after its admin signed out everywhere")
	}
}
