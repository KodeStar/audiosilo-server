package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/oidc"
	"github.com/kodestar/audiosilo-server/internal/oidc/oidctest"
)

const (
	oidcPublic = "https://audiosilo.test"
	oidcHost   = "audiosilo.test"
	oidcSecret = "s3cret-value-never-shown"
)

var (
	oidcAlice = oidctest.Person{Subject: "sub-alice", Email: "alice@example.org", EmailVerified: true,
		PreferredUsername: "alice", Name: "Alice", Groups: []string{"listeners"}}
	oidcCarol = oidctest.Person{Subject: "sub-carol", Email: "carol@example.org", EmailVerified: true,
		PreferredUsername: "carol", Groups: []string{"guests"}}
)

// oidcEnv is a test server with single sign-on on, against a fake provider.
type oidcEnv struct {
	*testEnv
	idp *oidctest.Provider
}

func newOIDCEnv(t *testing.T, p oidctest.Person, configure func(*config.OIDCConfig)) *oidcEnv {
	t.Helper()
	idp := oidctest.New(t, "audiosilo", oidcSecret, p)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = oidcPublic
		c.Auth.OIDC = config.OIDCConfig{Enabled: true, Name: "Keycloak", Issuer: idp.Issuer, ClientID: "audiosilo", ClientSecret: oidcSecret}
		if configure != nil {
			configure(&c.Auth.OIDC)
		}
	})
	return &oidcEnv{testEnv: e, idp: idp}
}

var noFollow = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// get requests path on the test server as host, with cookies, never following
// a redirect.
func (e *testEnv) get(t *testing.T, path, host string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Host = host
	// The test server is plain HTTP behind an https public_url, as behind a
	// proxy that ends TLS.
	req.Header.Set("X-Forwarded-Proto", "https")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func flowCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == oidcCookie {
			return c
		}
	}
	t.Fatalf("no flow cookie: %v", resp.Header.Values("Set-Cookie"))
	return nil
}

// start runs /start for ret and the provider's authorize: the flow cookie and
// the callback's path and query on the test server.
func (e *oidcEnv) start(t *testing.T, ret string) (*http.Cookie, string) {
	t.Helper()
	resp := e.get(t, oidcStartPath+"?return="+ret, oidcHost)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookie := flowCookie(t, resp)
	auth, err := noFollow.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	auth.Body.Close()
	back, err := url.Parse(auth.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(back.String(), oidcPublic+oidcCallbackPath+"?") {
		t.Fatalf("authorize sent the browser to %q", auth.Header.Get("Location"))
	}
	return cookie, back.RequestURI()
}

// callback sends the browser back: the redirect's Location.
func (e *oidcEnv) callback(t *testing.T, path string, cookie *http.Cookie) string {
	t.Helper()
	loc, _ := e.callbackDone(t, path, cookie)
	return loc
}

// callbackDone is callback with the finished sign-in's cookie it set (nil
// when it set none).
func (e *oidcEnv) callbackDone(t *testing.T, path string, cookie *http.Cookie) (string, *http.Cookie) {
	t.Helper()
	var cs []*http.Cookie
	if cookie != nil {
		cs = append(cs, cookie)
	}
	resp := e.get(t, path, oidcHost, cs...)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	c := resp.Cookies()
	if len(c) == 0 || c[0].Name != oidcCookie || c[0].MaxAge >= 0 {
		t.Fatalf("callback left the flow cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	var done *http.Cookie
	for _, k := range c[1:] {
		if k.Name == oidcDoneCookie {
			done = k
		}
	}
	return resp.Header.Get("Location"), done
}

// signIn runs a whole flow for ret: the callback's Location.
func (e *oidcEnv) signIn(t *testing.T, ret string) string {
	t.Helper()
	loc, _ := e.signInDone(t, ret)
	return loc
}

// signInDone runs a whole flow for ret: the callback's Location and the
// finished sign-in's cookie (nil when none).
func (e *oidcEnv) signInDone(t *testing.T, ret string) (string, *http.Cookie) {
	t.Helper()
	cookie, path := e.start(t, ret)
	return e.callbackDone(t, path, cookie)
}

func wantResult(t *testing.T, loc, reason string) {
	t.Helper()
	if loc != oidcPublic+"/connect#oidc="+reason {
		t.Fatalf("landed on %q; want the result panel %q", loc, reason)
	}
}

// finish is the landing page's POST /auth/oidc/finish with the browser's
// cookie (none when nil): the response and its decoded body.
func (e *testEnv) finish(t *testing.T, done *http.Cookie, device string) (*http.Response, map[string]any) {
	t.Helper()
	headers := map[string]string{}
	if done != nil {
		headers["Cookie"] = done.Name + "=" + done.Value
	}
	body := ""
	if device != "" {
		body = `{"device_name":"` + device + `"}`
	}
	resp, b := e.doHeaders(t, "POST", oidcFinishPath, "", body, headers)
	var out map[string]any
	_ = json.Unmarshal([]byte(b), &out)
	return resp, out
}

// finishAdmin checks a sign-in landed on the console and finishes it there:
// the status and body.
func (e *oidcEnv) finishAdmin(t *testing.T, loc string, done *http.Cookie) (int, map[string]any) {
	t.Helper()
	if loc != oidcPublic+oidcDoneAdmin || done == nil {
		t.Fatalf("landed on %q (cookie %v); want the console", loc, done)
	}
	resp, out := e.finish(t, done, "admin-web")
	return resp.StatusCode, out
}

// signInAdmin runs a whole flow for the console and finishes it.
func (e *oidcEnv) signInAdmin(t *testing.T) (int, map[string]any) {
	t.Helper()
	loc, done := e.signInDone(t, oidcReturnAdmin)
	return e.finishAdmin(t, loc, done)
}

func (e *testEnv) session(t *testing.T, id int64) string {
	t.Helper()
	tok, err := e.auth.IssueToken(context.Background(), id, auth.KindSession, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *testEnv) mustUser(t *testing.T, name, email string) *auth.User {
	t.Helper()
	u, err := e.auth.CreateUserWithEmail(context.Background(), name, "", auth.RoleUser, email)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func serverInfo(t *testing.T, e *testEnv) (caps map[string]bool, authBlock map[string]any) {
	t.Helper()
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	var out struct {
		Capabilities map[string]bool `json:"capabilities"`
		Auth         map[string]any  `json:"auth"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Capabilities, out.Auth
}

// TestOIDCServerInfo: the capability is false (not absent) while off or
// misconfigured, and the button's name rides in auth.oidc only while on.
func TestOIDCServerInfo(t *testing.T) {
	t.Parallel()
	off := newTestEnv(t)
	caps, a := serverInfo(t, off)
	if on, present := caps["oidc"]; !present || on {
		t.Fatalf("off: oidc %v present %v", on, present)
	}
	if _, ok := a["oidc"]; ok || len(a["methods"].([]any)) != 2 {
		t.Fatalf("off: auth %v", a)
	}
	if loc := off.get(t, oidcStartPath+"?return=admin", "x").Header.Get("Location"); loc != "/connect#oidc="+oidcOff {
		t.Fatalf("off: %q", loc)
	}

	e := newOIDCEnv(t, oidcAlice, nil)
	caps, a = serverInfo(t, e.testEnv)
	if !caps["oidc"] || a["oidc"].(map[string]any)["name"] != "Keycloak" || len(a["methods"].([]any)) != 3 {
		t.Fatalf("on: %v %v", caps["oidc"], a)
	}

	// No public_url: nothing to build the redirect URI from.
	noURL := newTestEnvWith(t, func(c *config.Config) {
		c.Auth.OIDC = config.OIDCConfig{Enabled: true, Issuer: "https://idp.example", ClientID: "x"}
	})
	if caps, _ := serverInfo(t, noURL); caps["oidc"] {
		t.Fatal("on without public_url")
	}
	if loc := noURL.get(t, oidcStartPath+"?return=web", "x").Header.Get("Location"); loc != "/connect#oidc="+oidcUnavailable {
		t.Fatalf("no public_url: %q", loc)
	}
	// A bad provider config never stops the server: off, with the reason.
	bad := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = oidcPublic
		c.Auth.OIDC = config.OIDCConfig{Enabled: true, Issuer: "ftp://idp", ClientID: "x"}
	})
	if caps, _ := serverInfo(t, bad); caps["oidc"] {
		t.Fatal("on with a bad issuer")
	}
	resp, body := bad.do(t, "GET", "/api/v1/admin/oidc/status", bad.session(t, bad.adminID), "")
	if resp.StatusCode != 200 || !strings.Contains(body, `"configured":false`) || !strings.Contains(body, "issuer") {
		t.Fatalf("status of a bad config: %d %s", resp.StatusCode, body)
	}
}

// TestOIDCCookieSecureAnyCase: the flow cookie is Secure on an https public_url
// in any case (public_url keeps the case it was typed in), and not on http.
func TestOIDCCookieSecureAnyCase(t *testing.T) {
	t.Parallel()
	for base, want := range map[string]bool{"HTTPS://audiosilo.test": true, "https://audiosilo.test": true, "http://audiosilo.test": false} {
		e := newTestEnvWith(t, func(c *config.Config) { c.PublicURL = base })
		w := httptest.NewRecorder()
		e.api.setOIDCCookie(w, oidcCookie, "v", 600)
		if c := w.Result().Cookies(); len(c) != 1 || c[0].Secure != want {
			t.Errorf("public_url %s: %+v, want Secure %v", base, c, want)
		}
	}
}

// TestOIDCStart: return is an enum, every redirect is built from public_url,
// and the flow cookie is HttpOnly, Lax, Secure, ten minutes, on its path.
func TestOIDCStart(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	for _, bad := range []string{"", "https://evil.example", "/admin", "WEB"} {
		if resp := e.get(t, oidcStartPath+"?return="+url.QueryEscape(bad), oidcHost); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("return=%q: %d", bad, resp.StatusCode)
		}
	}
	// Another Host: sent to the same route on public_url first, once.
	resp := e.get(t, oidcStartPath+"?return=admin", "evil.example")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?return=admin&hop=1" {
		t.Fatalf("another host: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("a cookie set on the wrong host")
	}
	// Plain HTTP straight from the browser (home HTTP on public_url's own host
	// and port): the hop too, since a Secure cookie set there is dropped.
	plain, _ := http.NewRequest("GET", e.srv.URL+oidcStartPath+"?return=admin", nil)
	plain.Host = oidcHost
	resp, err := noFollow.Do(plain)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?return=admin&hop=1" ||
		len(resp.Cookies()) != 0 {
		t.Fatalf("plain HTTP on the public host: %d %q %v", resp.StatusCode, resp.Header.Get("Location"), resp.Cookies())
	}
	// After the hop the flow goes on, whatever Host a proxy passes.
	if resp := e.get(t, oidcStartPath+"?return=admin&hop=1", "backend:8080"); resp.StatusCode != http.StatusFound ||
		!strings.HasPrefix(resp.Header.Get("Location"), e.idp.Issuer+"/authorize?") {
		t.Fatalf("after the hop: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp = e.get(t, oidcStartPath+"?return=admin", oidcHost)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	q := loc.Query()
	if q.Get("redirect_uri") != oidcPublic+oidcCallbackPath || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("client_id") != "audiosilo" {
		t.Fatalf("authorize URL: %v", q)
	}
	c := flowCookie(t, resp)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != oidcCookiePath || c.MaxAge != 600 {
		t.Fatalf("cookie: %+v", c)
	}
	if strings.Contains(c.Value, q.Get("state")) {
		t.Fatal("the cookie shows the state")
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("start is cacheable")
	}
}

// TestOIDCAdminSignIn: alice's verified email links her ordinary account,
// admin_group makes her an admin, and the console exchanges the token it is
// handed for a session, once.
func TestOIDCAdminSignIn(t *testing.T) {
	t.Parallel()
	p := oidcAlice
	p.Groups = []string{"audiosilo-admins"}
	// The group as config.yaml or the environment may hold it: only the
	// console's save trims it, and a claim's group is always trimmed.
	e := newOIDCEnv(t, p, func(o *config.OIDCConfig) { o.AdminGroup = " audiosilo-admins " })
	alice := e.mustUser(t, "alice", "Alice@Example.org")
	loc, done := e.signInDone(t, oidcReturnAdmin)
	status, out := e.finishAdmin(t, loc, done)
	if status != 200 || out["user"].(map[string]any)["role"] != auth.RoleAdmin || out["token"] == "" {
		t.Fatalf("exchange: %d %v", status, out)
	}
	if _, ok := out["user"].(map[string]any)["email"]; ok {
		t.Fatal("the player's user carries the email")
	}
	// The token was single-use.
	if status, out := e.finishAdmin(t, loc, done); status != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired {
		t.Fatalf("a second finish: %d %v", status, out)
	}
	events, _, err := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
		if ev.ActorID != nil || ev.Via != catalog.ViaSystem {
			t.Fatalf("an automatic event with an actor: %+v", ev)
		}
	}
	if strings.Join(actions, ",") != "user.role_sync,user.oidc_link" {
		t.Fatalf("audit: %v", actions)
	}
	// The provider stops sending the groups claim at all (neither the ID
	// token nor userinfo): her role is left alone, and the log says why.
	logs := &syncBuffer{}
	e.api.log = slog.New(slog.NewTextHandler(logs, nil))
	p.Groups = nil
	e.idp.SetPerson(p)
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleAdmin {
		t.Fatalf("no groups claim: %d %v", status, out)
	}
	if !strings.Contains(logs.String(), "sent no groups claim") {
		t.Fatalf("no warning: %s", logs.String())
	}
	if n := e.idp.Calls("userinfo"); n == 0 {
		t.Fatal("userinfo wasn't asked for the missing claim")
	}
	// She leaves the group (an empty list is a claim): an ordinary user at the
	// next sign-in.
	p.Groups = []string{}
	e.idp.SetPerson(p)
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleUser {
		t.Fatalf("after leaving the group: %d %v", status, out)
	}
	if u, _ := e.auth.GetUser(context.Background(), alice.ID); u.Role != auth.RoleUser {
		t.Fatalf("role %q", u.Role)
	}
}

// TestOIDCWebSignIn: return=web lands on the web player's connect route,
// which finishes the sign-in with the cookie; no token in the address.
func TestOIDCWebSignIn(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	loc, done := e.signInDone(t, oidcReturnWeb)
	if loc != oidcPublic+oidcDoneWeb || done == nil || strings.Contains(loc, done.Value) {
		t.Fatalf("landed on %q (cookie %v)", loc, done)
	}
	if resp, out := e.finish(t, done, "web"); resp.StatusCode != 200 || out["token"] == "" {
		t.Fatalf("finish: %d %v", resp.StatusCode, out)
	}
}

// TestOIDCFinish: a sign-in's pairing token reaches the page only in its
// cookie (HttpOnly, Lax, Secure on an https public_url, the flow's path, 60 s),
// never in a URL; POST /auth/oidc/finish trades it once, clearing the cookie,
// and answers as POST /auth/exchange does. A browser that never signed in
// (someone else's link to /admin/#oidc=done) holds no cookie and gets no
// session; a spent or expired token gets none either.
func TestOIDCFinish(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	alice := e.mustUser(t, "alice", "alice@example.org")
	loc, done := e.signInDone(t, oidcReturnAdmin)
	if done == nil || !done.HttpOnly || !done.Secure || done.SameSite != http.SameSiteLaxMode ||
		done.Path != oidcCookiePath || done.MaxAge != 60 || done.Value == "" {
		t.Fatalf("finish cookie: %+v", done)
	}
	if loc != oidcPublic+oidcDoneAdmin || strings.Contains(loc, done.Value) {
		t.Fatalf("the landing page %q", loc)
	}

	// Denied: no cookie (the link followed in another browser).
	resp, out := e.finish(t, nil, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCNoSignIn || out["token"] != nil {
		t.Fatalf("no cookie: %d %v", resp.StatusCode, out)
	}
	// Denied: a token that isn't a sign-in's.
	resp, out = e.finish(t, &http.Cookie{Name: oidcDoneCookie, Value: "forged"}, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired {
		t.Fatalf("a forged cookie: %d %v", resp.StatusCode, out)
	}
	// Denied: an expired one (issued 61 s ago, with its 60 s life).
	past := auth.New(e.db, func() time.Time { return time.Now().Add(-oidcPairingTTL - time.Second) })
	stale, err := past.IssueToken(context.Background(), alice.ID, auth.KindPairing, "", oidcPairingTTL)
	if err != nil {
		t.Fatal(err)
	}
	resp, out = e.finish(t, &http.Cookie{Name: oidcDoneCookie, Value: stale}, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired {
		t.Fatalf("an expired token: %d %v", resp.StatusCode, out)
	}

	// Allowed: the cookie the sign-in set, once; the answer is exchange's.
	resp, out = e.finish(t, done, "admin-web")
	if _, ok := out["server_id"]; resp.StatusCode != 200 || out["token"] == "" || !ok ||
		out["user"].(map[string]any)["username"] != "alice" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("finish: %d %v", resp.StatusCode, out)
	}
	if c := resp.Cookies(); len(c) != 1 || c[0].Name != oidcDoneCookie || c[0].MaxAge >= 0 || c[0].Path != oidcCookiePath {
		t.Fatalf("finish left the cookie: %v", resp.Header.Values("Set-Cookie"))
	}
	if r, _ := e.do(t, "GET", "/api/v1/me", out["token"].(string), ""); r.StatusCode != 200 {
		t.Fatalf("the session: %d", r.StatusCode)
	}
	// Spent.
	resp, out = e.finish(t, done, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired {
		t.Fatalf("a second finish: %d %v", resp.StatusCode, out)
	}
	// Only POST.
	if r, _ := e.do(t, "GET", oidcFinishPath, "", ""); r.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET finish: %d", r.StatusCode)
	}
}

// TestOIDCLastAdminKept: the only enabled admin, signing in out of the admin
// group, stays an admin (refused demotion audited), and still signs in.
func TestOIDCLastAdminKept(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, func(o *config.OIDCConfig) { o.AdminGroup = "audiosilo-admins" })
	ctx := context.Background()
	email := "alice@example.org"
	if err := e.auth.UpdateUser(ctx, e.adminID, auth.UserChanges{Email: &email}); err != nil {
		t.Fatal(err)
	}
	// An admin's email is never linked by itself (decision 4).
	wantResult(t, e.signIn(t, oidcReturnAdmin), oidcPending)
	pending, _ := e.auth.ListPending(ctx)
	if len(pending) != 1 || pending[0].Reason != oidc.ReasonAdminAccount {
		t.Fatalf("pending: %+v", pending)
	}
	tok := e.session(t, e.adminID)
	if resp, body := e.do(t, "POST", "/api/v1/admin/oidc/pending/"+itoa(pending[0].ID)+"/link", tok,
		`{"user_id":`+itoa(e.adminID)+`}`); resp.StatusCode != 200 {
		t.Fatalf("link: %d %s", resp.StatusCode, body)
	}
	status, out := e.signInAdmin(t)
	if status != 200 || out["user"].(map[string]any)["role"] != auth.RoleAdmin {
		t.Fatalf("the last admin: %d %v", status, out)
	}
	events, _, _ := e.cat.ListAudit(ctx, catalog.AuditFilter{Limit: 1})
	if events[0].Action != "user.role_sync" || events[0].Details["refused"] != "last_admin" {
		t.Fatalf("audit: %+v", events[0])
	}
}

// TestOIDCFlowTamperingDenied: a state that isn't the cookie's, no cookie, a
// cookie used before, another flow's cookie and an expired one are refused, as
// is a provider's ?error=.
func TestOIDCFlowTamperingDenied(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")

	cookie, path := e.start(t, oidcReturnAdmin)
	u, _ := url.Parse(path)
	q := u.Query()
	q.Set("state", "forged")
	u.RawQuery = q.Encode()
	wantResult(t, e.callback(t, u.RequestURI(), cookie), oidcFailed)

	_, path = e.start(t, oidcReturnAdmin)
	wantResult(t, e.callback(t, path, nil), oidcExpired)

	// Another flow's cookie.
	c1, _ := e.start(t, oidcReturnAdmin)
	_, p2 := e.start(t, oidcReturnAdmin)
	wantResult(t, e.callback(t, p2, c1), oidcFailed)

	// Used once: a copy of the cookie with the same callback is refused.
	cookie, path = e.start(t, oidcReturnAdmin)
	if loc := e.callback(t, path, cookie); !strings.Contains(loc, oidcDoneAdmin) {
		t.Fatalf("the first use: %q", loc)
	}
	wantResult(t, e.callback(t, path, cookie), oidcFailed)

	// A cookie made over ten minutes ago.
	var mu sync.Mutex
	now := time.Now()
	sealer, err := oidc.NewSealer(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	if err != nil {
		t.Fatal(err)
	}
	e.api.oidc.sealer = sealer
	cookie, path = e.start(t, oidcReturnAdmin)
	mu.Lock()
	now = now.Add(oidc.FlowMaxAge + time.Second)
	mu.Unlock()
	wantResult(t, e.callback(t, path, cookie), oidcExpired)

	// The provider ended it (the person cancelled). Only a completed flow is
	// remembered, so states sent back bare can't fill the replay memory: the
	// same flow still completes afterwards.
	cookie, path = e.start(t, oidcReturnAdmin)
	u, _ = url.Parse(path)
	q = u.Query()
	q.Del("code")
	q.Set("error", "access_denied")
	u.RawQuery = q.Encode()
	wantResult(t, e.callback(t, u.RequestURI(), cookie), oidcFailed)
	if loc := e.callback(t, path, cookie); !strings.Contains(loc, oidcDoneAdmin) {
		t.Fatalf("a flow sent back bare was remembered: %q", loc)
	}
}

// TestOIDCTokenRefused: an ID token the checks refuse (a symmetric signature,
// another issuer) signs no one in; the same flow with a good token does.
func TestOIDCTokenRefused(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	for _, k := range []oidctest.Knobs{{Alg: "HS256"}, {TokenIssuer: "https://evil.example"}, {Nonce: "another"},
		{UserinfoOnly: true, UserinfoSubject: "sub-mallory"}} {
		e.idp.SetKnobs(k)
		wantResult(t, e.signIn(t, oidcReturnAdmin), oidcFailed)
	}
	e.idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true})
	if loc := e.signIn(t, oidcReturnAdmin); !strings.Contains(loc, oidcDoneAdmin) {
		t.Fatalf("a good token from userinfo: %q", loc)
	}
}

// TestOIDCPendingAndConsole: carol is unknown (auto_create off): pending, then
// the console links, creates or dismisses; a non-admin can do none of it.
func TestOIDCPendingAndConsole(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcCarol, nil)
	ctx := context.Background()
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	tok := e.session(t, e.adminID)
	plain := e.mustUser(t, "plain", "")
	plainTok := e.session(t, plain.ID)

	resp, body := e.do(t, "GET", "/api/v1/admin/oidc/pending", tok, "")
	var list struct {
		Pending []auth.PendingSignIn `json:"pending"`
	}
	_ = json.Unmarshal([]byte(body), &list)
	if resp.StatusCode != 200 || len(list.Pending) != 1 || list.Pending[0].Reason != oidc.ReasonAutoCreateOff ||
		list.Pending[0].Email != "carol@example.org" || !list.Pending[0].EmailVerified {
		t.Fatalf("pending: %d %s", resp.StatusCode, body)
	}
	id := itoa(list.Pending[0].ID)
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/api/v1/admin/oidc/pending", ""},
		{"GET", "/api/v1/admin/oidc/status", ""},
		{"POST", "/api/v1/admin/oidc/pending/" + id + "/link", `{"user_id":` + itoa(plain.ID) + `}`},
		{"POST", "/api/v1/admin/oidc/pending/" + id + "/create", `{}`},
		{"DELETE", "/api/v1/admin/oidc/pending/" + id, ""},
	} {
		if resp, _ := e.do(t, r.method, r.path, plainTok, r.body); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("a non-admin's %s %s: %d", r.method, r.path, resp.StatusCode)
		}
	}
	resp, body = e.do(t, "POST", "/api/v1/admin/oidc/pending/"+id+"/create", tok, `{"username":"carol-c"}`)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"username":"carol-c"`) || !strings.Contains(body, `"email":"carol@example.org"`) {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/admin/oidc/pending/"+id+"/create", tok, `{}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("create again: %d", resp.StatusCode)
	}
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("carol after create: %q", loc)
	}
	// Dismiss and link, on another person.
	e.idp.SetPerson(oidctest.Person{Subject: "sub-bob", Email: "bob@example.org", EmailVerified: false})
	bob := e.mustUser(t, "bob", "bob@example.org")
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	rows, _ := e.auth.ListPending(ctx)
	if len(rows) != 1 || rows[0].Reason != oidc.ReasonUnverifiedEmail {
		t.Fatalf("bob pending: %+v", rows)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/oidc/pending/"+itoa(rows[0].ID), tok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dismiss: %d", resp.StatusCode)
	}
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	rows, _ = e.auth.ListPending(ctx)
	if resp, body := e.do(t, "POST", "/api/v1/admin/oidc/pending/"+itoa(rows[0].ID)+"/link", tok, `{"user_id":`+itoa(bob.ID)+`}`); resp.StatusCode != 200 {
		t.Fatalf("link: %d %s", resp.StatusCode, body)
	}
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("bob after link: %q", loc)
	}
	// The identities on bob's page; unlink.
	resp, body = e.do(t, "GET", "/api/v1/admin/users/"+itoa(bob.ID), tok, "")
	var detail struct {
		User       map[string]any  `json:"user"`
		Identities []auth.Identity `json:"identities"`
	}
	_ = json.Unmarshal([]byte(body), &detail)
	if resp.StatusCode != 200 || len(detail.Identities) != 1 || detail.Identities[0].LinkMethod != auth.LinkAdmin ||
		detail.User["email"] != "bob@example.org" {
		t.Fatalf("detail: %d %s", resp.StatusCode, body)
	}
	path := "/api/v1/admin/users/" + itoa(bob.ID) + "/identities/" + itoa(detail.Identities[0].ID)
	if resp, _ := e.do(t, "DELETE", path, plainTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a non-admin's unlink: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", path, tok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlink: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "DELETE", path, tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unlink again: %d", resp.StatusCode)
	}
	var actions []string
	events, _, _ := e.cat.ListAudit(ctx, catalog.AuditFilter{Limit: 10})
	for _, ev := range events {
		actions = append(actions, ev.Action)
		// The audit keeps no email (bob's dismissed sign-in has no username).
		if strings.Contains(ev.Target, "@") {
			t.Fatalf("%s names an email: %q", ev.Action, ev.Target)
		}
	}
	if got := strings.Join(actions, ","); got != "user.oidc_unlink,user.oidc_link,oidc.pending_dismiss,user.oidc_create" {
		t.Fatalf("audit: %s", got)
	}
}

// TestOIDCUnlinkAdminNeedsPassword: a password-less admin's last identity
// can't be unlinked (409); with a password it can.
func TestOIDCUnlinkAdminNeedsPassword(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcCarol, func(o *config.OIDCConfig) { o.AutoCreate = true })
	ctx := context.Background()
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("auto-create: %q", loc)
	}
	users, _ := e.auth.ListUsers(ctx)
	var carol auth.User
	for _, u := range users {
		if u.Username == "carol" {
			carol = u
		}
	}
	tok := e.session(t, e.adminID)
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+itoa(carol.ID), tok, `{"role":"admin"}`); resp.StatusCode != 200 {
		t.Fatalf("promote a linked, password-less account: %d %s", resp.StatusCode, body)
	}
	ids, _ := e.auth.ListIdentities(ctx, carol.ID)
	path := "/api/v1/admin/users/" + itoa(carol.ID) + "/identities/" + itoa(ids[0].ID)
	resp, body := e.do(t, "DELETE", path, tok, "")
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, codeAdminNeedsPassword) {
		t.Fatalf("unlink the last way in: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "PATCH", "/api/v1/admin/users/"+itoa(carol.ID), tok, `{"password":"a-long-password"}`); resp.StatusCode != 200 {
		t.Fatal("set a password")
	}
	if resp, _ := e.do(t, "DELETE", path, tok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unlink with a password: %d", resp.StatusCode)
	}
}

// TestOIDCIssuerWayIn: the issuer whose identities are an admin's way into the
// console (auth.SetSSOIssuer) is the configured one only while single sign-on
// can run: off, or on without a public_url, none is.
func TestOIDCIssuerWayIn(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcCarol, nil)
	if got := e.api.oidcIssuer(); got != e.idp.Issuer {
		t.Fatalf("running: %q, want %q", got, e.idp.Issuer)
	}
	for name, configure := range map[string]func(*config.Config){
		"off": func(c *config.Config) {
			c.PublicURL = oidcPublic
			c.Auth.OIDC = config.OIDCConfig{Issuer: e.idp.Issuer, ClientID: "audiosilo"}
		},
		"no public_url": func(c *config.Config) {
			c.Auth.OIDC = config.OIDCConfig{Enabled: true, Issuer: e.idp.Issuer, ClientID: "audiosilo"}
		},
	} {
		if got := newTestEnvWith(t, configure).api.oidcIssuer(); got != "" {
			t.Fatalf("%s: %q", name, got)
		}
	}
}

// TestOIDCDisabledDenied: a disabled account is refused at the callback, and an
// account disabled between the callback and the exchange gets no session.
func TestOIDCDisabledDenied(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	ctx := context.Background()
	alice := e.mustUser(t, "alice", "alice@example.org")
	loc, done := e.signInDone(t, oidcReturnAdmin) // links her; not yet finished
	yes := true
	if err := e.auth.UpdateUser(ctx, alice.ID, auth.UserChanges{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.finishAdmin(t, loc, done); status != http.StatusUnauthorized {
		t.Fatalf("finish for a disabled account: %d", status)
	}
	wantResult(t, e.signIn(t, oidcReturnAdmin), oidcDisabled)
	no := false
	if err := e.auth.UpdateUser(ctx, alice.ID, auth.UserChanges{Disabled: &no}); err != nil {
		t.Fatal(err)
	}
	if status, _ := e.signInAdmin(t); status != 200 {
		t.Fatalf("enabled again: %d", status)
	}
}

// TestOIDCAutoCreateAllowedGroup: with auto_create on, a person in
// allowed_group gets an account; one outside it is refused and recorded.
func TestOIDCAutoCreateAllowedGroup(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcCarol, func(o *config.OIDCConfig) { o.AutoCreate, o.AllowedGroup = true, "family" })
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcNotAllowed)
	p := oidcCarol
	p.Groups = []string{"family"}
	e.idp.SetPerson(p)
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("in the group: %q", loc)
	}
	events, _, _ := e.cat.ListAudit(context.Background(), catalog.AuditFilter{Limit: 1})
	if events[0].Action != "user.oidc_create" || events[0].Target != "carol" {
		t.Fatalf("audit: %+v", events[0])
	}
}

// TestOIDCCallbackRateLimited: ten failed callbacks from an address lock it out
// for a while; a good one before that resets the count. A callback with no
// flow cookie (what another site's page can make a browser send) counts for
// nothing.
func TestOIDCCallbackRateLimited(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	foreign := &http.Cookie{Name: oidcCookie, Value: "not-this-server's"}
	for range 25 {
		wantResult(t, e.callback(t, oidcCallbackPath+"?code=x&state=y", nil), oidcExpired)
	}
	for range 9 {
		wantResult(t, e.callback(t, oidcCallbackPath+"?code=x&state=y", foreign), oidcFailed)
	}
	if loc := e.signIn(t, oidcReturnAdmin); !strings.Contains(loc, oidcDoneAdmin) {
		t.Fatalf("a good sign-in under the limit: %q", loc)
	}
	for range 10 {
		wantResult(t, e.callback(t, oidcCallbackPath+"?code=x&state=y", foreign), oidcFailed)
	}
	resp := e.get(t, oidcCallbackPath+"?code=x&state=y", oidcHost)
	if loc := resp.Header.Get("Location"); loc != oidcPublic+"/connect#oidc="+oidcRateLimited || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %q %v", loc, resp.Header)
	}
	cookie, path := e.start(t, oidcReturnAdmin)
	wantResult(t, e.callback(t, path, cookie), oidcRateLimited)
}

// TestOIDCSecretsNeverShown: the client secret is in neither settings nor the
// status, and no log line carries it, a code or a token.
func TestOIDCSecretsNeverShown(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	logs := &syncBuffer{}
	e.api.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tok := e.session(t, e.adminID)
	_, settings := e.do(t, "GET", "/api/v1/admin/settings", tok, "")
	_, status := e.do(t, "GET", "/api/v1/admin/oidc/status", tok, "")
	for name, body := range map[string]string{"settings": settings, "status": status} {
		if strings.Contains(body, oidcSecret) {
			t.Fatalf("%s shows the secret", name)
		}
		if !strings.Contains(body, `"client_secret_set":true`) {
			t.Fatalf("%s doesn't say a secret is set: %s", name, body)
		}
	}
	if !strings.Contains(settings, `"username_claim_default":"preferred_username"`) {
		t.Fatalf("settings don't say the defaults: %s", settings)
	}
	if !strings.Contains(status, `"redirect_uri":"`+oidcPublic+oidcCallbackPath+`"`) || !strings.Contains(status, `"state":"ok"`) {
		t.Fatalf("status: %s", status)
	}
	// A refused exchange (a code the provider never issued) and a sign-in.
	cookie, path := e.start(t, oidcReturnAdmin)
	u, _ := url.Parse(path)
	code := u.Query().Get("code")
	q := u.Query()
	q.Set("code", "forged-code-value")
	u.RawQuery = q.Encode()
	wantResult(t, e.callback(t, u.RequestURI(), cookie), oidcFailed)
	e.mustUser(t, "alice", "alice@example.org")
	_, done := e.signInDone(t, oidcReturnAdmin)
	pairing := done.Value
	out := logs.String()
	if !strings.Contains(out, "oidc: sign-in refused") {
		t.Fatalf("the refusal wasn't logged: %s", out)
	}
	for _, secret := range []string{oidcSecret, code, "forged-code-value", pairing} {
		if strings.Contains(out, secret) {
			t.Fatalf("a log line carries %q: %s", secret, out)
		}
	}
}

// TestOIDCSettings: the live sign-in settings save; the provider's are read
// only.
func TestOIDCSettings(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	tok := e.session(t, e.adminID)
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", tok,
		`{"signin":{"auto_create":true,"allowed_group":" family ","admin_group":"admins"}}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"allowed_group":"family"`) {
		t.Fatalf("save: %d %s", resp.StatusCode, body)
	}
	if o := e.api.config().Auth.OIDC; !o.AutoCreate || o.AllowedGroup != "family" || o.AdminGroup != "admins" {
		t.Fatalf("live config: %+v", o)
	}
	for _, s := range []string{`{"signin":{"issuer":"https://evil.example"}}`, `{"signin":{"client_secret":"x"}}`} {
		if resp, _ := e.do(t, "PATCH", "/api/v1/admin/settings", tok, s); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: %d", s, resp.StatusCode)
		}
	}
}

// TestUserEmailRoutes: POST/PATCH /admin/users take an email (checked); /me
// never shows it.
func TestUserEmailRoutes(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	tok := e.session(t, e.adminID)
	resp, body := e.do(t, "POST", "/api/v1/admin/users", tok, `{"username":"dee","email":" dee@example.org "}`)
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"email":"dee@example.org"`) {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var u struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &u)
	if resp, body := e.do(t, "POST", "/api/v1/admin/users", tok, `{"username":"eve","email":"nope"}`); resp.StatusCode != 400 ||
		!strings.Contains(body, codeInvalidEmail) {
		t.Fatalf("invalid email on create: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+itoa(u.ID), tok, `{"email":"a b@c"}`); resp.StatusCode != 400 ||
		!strings.Contains(body, codeInvalidEmail) {
		t.Fatalf("invalid email on patch: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+itoa(u.ID), tok, `{"email":""}`); resp.StatusCode != 200 ||
		!strings.Contains(body, `"email":""`) {
		t.Fatalf("clear: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+itoa(u.ID), tok, `{"email":"new@example.org"}`); resp.StatusCode != 200 ||
		!strings.Contains(body, `"email":"new@example.org"`) {
		t.Fatalf("set: %d %s", resp.StatusCode, body)
	}
	_, me := e.do(t, "GET", "/api/v1/me", e.session(t, u.ID), "")
	if strings.Contains(me, "new@example.org") || strings.Contains(me, `"email"`) {
		t.Fatalf("/me shows the email: %s", me)
	}
}

// TestOIDCReasonsWorded: the connect page words every reason the callback
// sends it.
func TestOIDCReasonsWorded(t *testing.T) {
	t.Parallel()
	js, err := os.ReadFile("../web/assets/connect.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{oidcOff, oidcUnavailable, oidcPending, oidcDisabled, oidcNotAllowed, oidcFailed, oidcExpired, oidcRateLimited} {
		if !strings.Contains(string(js), `case "`+r+`": return asI18n.t("connect.oidc.`+r+`")`) {
			t.Errorf("connect.js doesn't word #oidc=%s", r)
		}
	}
}
