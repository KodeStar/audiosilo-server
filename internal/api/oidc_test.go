package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
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
	return newOIDCEnvAt(t, oidcPublic, p, configure)
}

// newOIDCEnvAt is newOIDCEnv with public_url public.
func newOIDCEnvAt(t *testing.T, public string, p oidctest.Person, configure func(*config.OIDCConfig)) *oidcEnv {
	t.Helper()
	idp := oidctest.New(t, "audiosilo", oidcSecret, p)
	e := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = public
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

// The flow's cookies as a browser holds them for oidcPublic (https): a
// flow's own cookie starts with hostFlowCookie (hostFlowCookieFor).
const (
	hostFlowCookie = hostCookiePrefix + oidcCookie
	hostDoneCookie = hostCookiePrefix + oidcDoneCookie
)

// hostFlowCookieFor is the name of the flow cookie for state, for oidcPublic.
func hostFlowCookieFor(state string) string { return hostFlowCookie + flowKey(state) }

// stateOf is the state in a callback's path.
func stateOf(t *testing.T, path string) string {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

// respCookie is the cookie named name that resp sets (nil when none).
func respCookie(resp *http.Response, name string) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// flowCookie is the flow cookie /start's resp sets: the one it doesn't delete.
func flowCookie(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if strings.HasPrefix(c.Name, hostFlowCookie) && c.MaxAge > 0 {
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
	return e.startQuery(t, "return="+ret)
}

// startQuery is start with /start's whole query, and the cookies the browser
// holds.
func (e *oidcEnv) startQuery(t *testing.T, query string, cookies ...*http.Cookie) (*http.Cookie, string) {
	t.Helper()
	resp := e.get(t, oidcStartPath+"?"+query, oidcHost, cookies...)
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
// when it set none). Other flows' cookies the browser holds ride along.
func (e *oidcEnv) callbackDone(t *testing.T, path string, cookie *http.Cookie, held ...*http.Cookie) (string, *http.Cookie) {
	t.Helper()
	var cs []*http.Cookie
	if cookie != nil {
		cs = append(cs, cookie)
	}
	resp := e.get(t, path, oidcHost, append(cs, held...)...)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	// Its own flow's cookie deleted (none for a state no flow has), never
	// another's.
	c := resp.Cookies()
	if flowKey(stateOf(t, path)) != "" {
		if len(c) == 0 || c[0].Name != hostFlowCookieFor(stateOf(t, path)) || c[0].MaxAge >= 0 {
			t.Fatalf("callback left the flow cookie: %v", resp.Header.Values("Set-Cookie"))
		}
		c = c[1:]
	}
	for _, other := range c {
		if strings.HasPrefix(other.Name, hostFlowCookie) {
			t.Fatalf("callback touched another flow's cookie: %v", resp.Header.Values("Set-Cookie"))
		}
	}
	return resp.Header.Get("Location"), respCookie(resp, hostDoneCookie)
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

// TestOIDCCookieSecureAnyCase: on an https public_url (in any case: public_url
// keeps the case it was typed in) the flow's cookies are Secure and __Host-
// on Path=/ with no Domain, so nothing but this origin over TLS can plant
// one; on http, where no prefix can be had, they keep their plain names on
// the flow's path.
func TestOIDCCookieSecureAnyCase(t *testing.T) {
	t.Parallel()
	type want struct {
		secure bool
		name   string
		paths  []string
	}
	for base, w := range map[string]want{
		"HTTPS://audiosilo.test": {true, hostFlowCookie, []string{"/"}},
		"https://audiosilo.test": {true, hostFlowCookie, []string{"/"}},
		"http://audiosilo.test":  {false, oidcCookie, []string{oidcCookiePath}},
		// Under a proxy's path prefix (server#406): the provider sends the
		// callback to /audiosilo/api/v1/auth/oidc/..., the console and the
		// player ask /start on the origin's own /api/v1, so a copy at each.
		"http://audiosilo.test/audiosilo/":  {false, oidcCookie, []string{oidcCookiePath, "/audiosilo" + oidcCookiePath}},
		"http://audiosilo.test/my%20books":  {false, oidcCookie, []string{oidcCookiePath, "/my%20books" + oidcCookiePath}},
		"https://audiosilo.test/audiosilo/": {true, hostFlowCookie, []string{"/"}},
	} {
		e := newTestEnvWith(t, func(c *config.Config) { c.PublicURL = base })
		rec := httptest.NewRecorder()
		e.api.setOIDCCookie(rec, oidcCookie, "v", 600)
		var paths []string
		for _, c := range rec.Result().Cookies() {
			if c.Secure != w.secure || c.Name != w.name || c.Domain != "" {
				t.Errorf("public_url %s: %+v, want %+v", base, c, w)
			}
			paths = append(paths, c.Path)
		}
		if !slices.Equal(paths, w.paths) {
			t.Errorf("public_url %s: paths %q, want %q", base, paths, w.paths)
		}
	}
	// The done cookie stays where the finish is called: the console and the
	// web player ask their origin's own /api/v1, never under the prefix.
	e := newTestEnvWith(t, func(c *config.Config) { c.PublicURL = "http://audiosilo.test/audiosilo/" })
	rec := httptest.NewRecorder()
	e.api.setOIDCCookie(rec, oidcDoneCookie, "v", 60)
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Name != oidcDoneCookie || c[0].Path != oidcCookiePath {
		t.Errorf("the done cookie under a prefix: %+v", c)
	}
}

// TestOIDCStart: return is an enum, every redirect is built from public_url,
// and the flow cookie is HttpOnly, Lax, Secure, ten minutes, __Host- on Path=/.
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
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?hop=1&return=admin" {
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
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?hop=1&return=admin" ||
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
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 600 {
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
	// Allowed (the owner's policy, server#405): once her identity has carried
	// the claim, an absent one (neither the ID token nor userinfo: a renamed
	// groups_claim, a dropped scope, Entra's overage) keeps her role, and the
	// log says the provider stopped sending it, once.
	logs := &syncBuffer{}
	e.api.log = slog.New(slog.NewTextHandler(logs, nil))
	p.Groups = nil
	e.idp.SetPerson(p)
	for range 2 {
		if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleAdmin {
			t.Fatalf("no groups claim after one: %d %v", status, out)
		}
	}
	if n := strings.Count(logs.String(), "sent no groups claim this time (it did before)"); n != 1 {
		t.Fatalf("the warning logged %d times, want once: %s", n, logs.String())
	}
	if n := e.idp.Calls("userinfo"); n == 0 {
		t.Fatal("userinfo wasn't asked for the missing claim")
	}
	// Denied: a list that is there without the group demotes.
	p.Groups = []string{"listeners"}
	e.idp.SetPerson(p)
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleUser {
		t.Fatalf("a list without the group: %d %v", status, out)
	}
	// Back in the group, then out of it again as an empty list: the same.
	p.Groups = []string{"audiosilo-admins"}
	e.idp.SetPerson(p)
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleAdmin {
		t.Fatalf("back in the group: %d %v", status, out)
	}
	p.Groups = []string{}
	e.idp.SetPerson(p)
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleUser {
		t.Fatalf("after leaving the group: %d %v", status, out)
	}
	if u, _ := e.auth.GetUser(context.Background(), alice.ID); u.Role != auth.RoleUser {
		t.Fatalf("role %q", u.Role)
	}
}

// TestOIDCNoGroupsClaimNeverSent: with admin_group set, a provider that has
// never sent the groups claim for a person (no scope or mapper for it) demotes
// no one: her role is left alone at each sign-in, and the log says why, once.
func TestOIDCNoGroupsClaimNeverSent(t *testing.T) {
	t.Parallel()
	p := oidcAlice
	p.Groups = nil
	e := newOIDCEnv(t, p, func(o *config.OIDCConfig) { o.AdminGroup = "audiosilo-admins" })
	alice := e.mustUser(t, "alice", "alice@example.org")
	logs := &syncBuffer{}
	e.api.log = slog.New(slog.NewTextHandler(logs, nil))
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleUser {
		t.Fatalf("first sign-in: %d %v", status, out)
	}
	if !strings.Contains(logs.String(), "sent no groups claim") {
		t.Fatalf("no warning: %s", logs.String())
	}
	// Made an admin in the console: the next sign-in, still without the
	// claim, leaves her one.
	role, password := auth.RoleAdmin, "alice-password"
	if err := e.auth.UpdateUser(context.Background(), alice.ID, auth.UserChanges{Role: &role, Password: &password}); err != nil {
		t.Fatal(err)
	}
	if status, out := e.signInAdmin(t); status != 200 || out["user"].(map[string]any)["role"] != auth.RoleAdmin {
		t.Fatalf("never sent: %d %v", status, out)
	}
	// Logged once a run, not at every sign-in (server#405).
	if n := strings.Count(logs.String(), "sent no groups claim"); n != 1 {
		t.Fatalf("the warning logged %d times, want once: %s", n, logs.String())
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

// TestOIDCIntent (audiosilo#202): the web player's tag on /start comes back
// in the finish's answer for the sign-in that flow completed, once, and never
// for any other: the start URL typed later (a provider session still live
// signs it in at once) finishes untagged, and so does a tag sent again.
func TestOIDCIntent(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	tag := strings.Repeat("ab12", 16) // the player's 64 hex
	finishTagged := func(t *testing.T, query string, held ...*http.Cookie) map[string]any {
		t.Helper()
		cookie, path := e.startQuery(t, query, held...)
		loc, done := e.callbackDone(t, path, cookie)
		if loc != oidcPublic+oidcDoneWeb || done == nil {
			t.Fatalf("landed on %q (cookie %v)", loc, done)
		}
		resp, out := e.finish(t, done, "web")
		if resp.StatusCode != 200 || out["token"] == "" {
			t.Fatalf("finish: %d %v", resp.StatusCode, out)
		}
		return out
	}

	// Denied: the device's own tagged trip abandoned at the provider, then the
	// start URL typed (no tag): a session, untagged.
	_, _ = e.startQuery(t, "return=web&intent="+tag)
	if out := finishTagged(t, "return=web"); out["intent"] != nil {
		t.Fatalf("a typed start was echoed %v", out["intent"])
	}
	// Allowed: the tagged trip, its own tag.
	if out := finishTagged(t, "return=web&intent="+tag); out["intent"] != tag {
		t.Fatalf("own trip: intent %v", out["intent"])
	}
	// Denied: the same tag again (the start URL from the history): spent.
	if out := finishTagged(t, "return=web&intent="+tag); out["intent"] != nil {
		t.Fatalf("a reused tag was echoed %v", out["intent"])
	}
	// Denied: a trip abandoned at the provider, its start URL sent again from
	// the history while the browser still holds its flow cookie: untagged.
	again := strings.Repeat("7a", 32)
	abandoned, _ := e.startQuery(t, "return=web&intent="+again)
	if out := finishTagged(t, "return=web&intent="+again, abandoned); out["intent"] != nil {
		t.Fatalf("an abandoned trip's start sent again was echoed %v", out["intent"])
	}
	// Allowed: a new trip's tag beside another trip's flow cookie.
	fresh := strings.Repeat("8b", 32)
	if out := finishTagged(t, "return=web&intent="+fresh, abandoned); out["intent"] != fresh {
		t.Fatalf("a new trip beside an old flow cookie: intent %v", out["intent"])
	}
	// Two trips in flight: the one completed echoes its own, never the other's.
	other := strings.Repeat("cd34", 16)
	_, _ = e.startQuery(t, "return=web&intent="+strings.Repeat("ef56", 16))
	if out := finishTagged(t, "return=web&intent="+other); out["intent"] != other {
		t.Fatalf("the completed trip: intent %v", out["intent"])
	}
	// Denied: a tag that can't be one (validIntent) is dropped, and the
	// sign-in still runs.
	if out := finishTagged(t, "return=web&intent="+strings.Repeat("a", 30)+"%2F"); out["intent"] != nil {
		t.Fatalf("a bad tag echoed %v", out["intent"])
	}
	// The hop to public_url keeps a good tag, never a bad one.
	hop := strings.Repeat("9f", 32)
	resp := e.get(t, oidcStartPath+"?return=web&intent="+hop, "evil.example")
	if resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?hop=1&intent="+hop+"&return=web" {
		t.Fatalf("hop with a tag: %q", resp.Header.Get("Location"))
	}
	resp = e.get(t, oidcStartPath+"?return=web&intent=bad%26hop%3D1", "evil.example")
	if resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?hop=1&return=web" {
		t.Fatalf("hop with a bad tag: %q", resp.Header.Get("Location"))
	}
}

// TestOIDCIntentPending: a tagged trip that ends waiting for an admin spends
// its tag, so once the account is there, the same start URL sent again
// (the history) signs in untagged.
func TestOIDCIntentPending(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	tag := strings.Repeat("c3", 32)
	cookie, path := e.startQuery(t, "return=web&intent="+tag)
	wantResult(t, e.callback(t, path, cookie), oidcPending)
	e.mustUser(t, "alice", "alice@example.org")
	cookie, path = e.startQuery(t, "return=web&intent="+tag)
	loc, done := e.callbackDone(t, path, cookie)
	if loc != oidcPublic+oidcDoneWeb || done == nil {
		t.Fatalf("landed on %q (cookie %v)", loc, done)
	}
	if resp, out := e.finish(t, done, "web"); resp.StatusCode != 200 || out["intent"] != nil {
		t.Fatalf("after a pending trip: %d intent %v", resp.StatusCode, out["intent"])
	}
}

// TestOIDCFinishExpectedIntent (audiosilo#202): a finish whose body names the
// tag the player expects is answered only for the sign-in that carried it.
// Any other is a 403 oidc_intent_mismatch before the token is tried: no
// session, the token unspent, the cookie cleared, nothing charged. Without
// the field (or null) the finish answers as before.
func TestOIDCFinishExpectedIntent(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	tag := strings.Repeat("ab12", 16)
	// trip runs a web sign-in to its done cookie; finish posts body with it.
	trip := func(t *testing.T, query string) *http.Cookie {
		t.Helper()
		cookie, path := e.startQuery(t, query)
		loc, done := e.callbackDone(t, path, cookie)
		if loc != oidcPublic+oidcDoneWeb || done == nil {
			t.Fatalf("landed on %q (cookie %v)", loc, done)
		}
		return done
	}
	finish := func(t *testing.T, done *http.Cookie, body string) (*http.Response, map[string]any) {
		t.Helper()
		headers := map[string]string{}
		if done != nil {
			headers["Cookie"] = done.Name + "=" + done.Value
		}
		resp, b := e.doHeaders(t, "POST", oidcFinishPath, "", body, headers)
		var out map[string]any
		_ = json.Unmarshal([]byte(b), &out)
		return resp, out
	}
	// refused: a 403 with the code, no session, the cookie cleared, and the
	// sign-in's token left unspent.
	refused := func(t *testing.T, name string, done *http.Cookie, body string) {
		t.Helper()
		resp, out := finish(t, done, body)
		if resp.StatusCode != http.StatusForbidden || out["code"] != codeOIDCIntentMismatch || out["token"] != nil || out["intent"] != nil {
			t.Fatalf("%s: %d %v", name, resp.StatusCode, out)
		}
		if !clearsDone(resp) {
			t.Fatalf("%s: the cookie kept: %v", name, resp.Header.Values("Set-Cookie"))
		}
		token, _, _ := strings.Cut(done.Value, oidcIntentSep)
		if u, err := e.auth.ConsumeOIDC(context.Background(), token); err != nil || u.Username != "alice" {
			t.Fatalf("%s: the refused finish spent the token: %v", name, err)
		}
	}

	// Allowed: the trip's own tag; echoed as before.
	resp, out := finish(t, trip(t, "return=web&intent="+tag), `{"device_name":"web","intent":"`+tag+`"}`)
	if resp.StatusCode != http.StatusOK || out["token"] == "" || out["intent"] != tag || !clearsDone(resp) {
		t.Fatalf("own tag: %d %v", resp.StatusCode, out)
	}
	if r, _ := e.do(t, "GET", "/api/v1/me", out["token"].(string), ""); r.StatusCode != 200 {
		t.Fatalf("the session: %d", r.StatusCode)
	}
	// Denied: a sign-in another trip completed (its own tag).
	other := strings.Repeat("cd34", 16)
	refused(t, "another trip's sign-in", trip(t, "return=web&intent="+other), `{"device_name":"web","intent":"`+strings.Repeat("ef56", 16)+`"}`)
	// Denied: an untagged sign-in (the start URL typed, a provider session
	// still live) where the player expects its tag.
	refused(t, "an untagged sign-in", trip(t, "return=web"), `{"intent":"`+strings.Repeat("7a", 32)+`"}`)
	// Denied: a tag spent before (the start URL from the history) signs in
	// untagged, so the player's expectation of it is refused too.
	refused(t, "a reused tag", trip(t, "return=web&intent="+tag), `{"intent":"`+tag+`"}`)
	// Denied: an expectation that can be no tag, the empty one included.
	refused(t, "an empty expectation, untagged", trip(t, "return=web"), `{"intent":""}`)
	refused(t, "an empty expectation, tagged", trip(t, "return=web&intent="+strings.Repeat("8b", 32)), `{"intent":""}`)
	refused(t, "a prefix of the tag", trip(t, "return=web&intent="+strings.Repeat("9f", 32)), `{"intent":"`+strings.Repeat("9f", 31)+`"}`)
	// A refusal charges the address nothing: one failure short of the lock
	// (oidcLimiter's ten), it still isn't locked afterwards.
	untagged := trip(t, "return=web")
	for range 9 {
		finish(t, &http.Cookie{Name: hostDoneCookie, Value: "guess"}, ``)
	}
	refused(t, "a refusal one failure short of the lock", untagged, `{"intent":"`+tag+`"}`)
	// No cookie: there is no sign-in to mismatch (and no lock: a 401, no 429).
	resp, out = finish(t, nil, `{"intent":"`+tag+`"}`)
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCNoSignIn {
		t.Fatalf("no cookie: %d %v", resp.StatusCode, out)
	}
	// Unchanged without an expectation: absent or null, tagged or not.
	for name, c := range map[string]struct{ query, body, want string }{
		"no field, tagged":   {"return=web&intent=" + strings.Repeat("1c", 32), `{"device_name":"web"}`, strings.Repeat("1c", 32)},
		"null, tagged":       {"return=web&intent=" + strings.Repeat("2d", 32), `{"intent":null}`, strings.Repeat("2d", 32)},
		"no body, untagged":  {"return=web", ``, ""},
		"null, untagged":     {"return=web", `{"intent":null}`, ""},
		"no field, untagged": {"return=web", `{"device_name":"web"}`, ""},
	} {
		resp, out := finish(t, trip(t, c.query), c.body)
		got, _ := out["intent"].(string)
		if resp.StatusCode != http.StatusOK || out["token"] == "" || got != c.want {
			t.Fatalf("%s: %d %v", name, resp.StatusCode, out)
		}
	}
}

// TestValidIntent: a tag is 22 to 128 base64url characters.
func TestValidIntent(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"":                                  false,
		"short":                             false,
		strings.Repeat("a", 21):             false,
		strings.Repeat("a", 22):             true,
		strings.Repeat("ab12", 16):          true,
		strings.Repeat("aZ9-_", 25) + "abc": true,
		strings.Repeat("a", 129):            false,
		strings.Repeat("a", 30) + ".x":      false,
		strings.Repeat("a", 30) + "/":       false,
		strings.Repeat("a", 30) + "&hop=1":  false,
	} {
		if got := validIntent(s); got != want {
			t.Errorf("validIntent(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestOIDCFinishCookieTag: the done cookie's tag is read only when it can be
// one (validIntent). Anything else after the token finishes the sign-in
// untagged, never echoed, and an expectation of it is refused as for any
// untagged sign-in.
func TestOIDCFinishCookieTag(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	// forged runs a tagged web sign-in and swaps its cookie's tag for tail.
	forged := func(t *testing.T, tag, tail string) *http.Cookie {
		t.Helper()
		cookie, path := e.startQuery(t, "return=web&intent="+tag)
		_, done := e.callbackDone(t, path, cookie)
		if done == nil {
			t.Fatal("no finished sign-in")
		}
		token, got, _ := strings.Cut(done.Value, oidcIntentSep)
		if got != tag {
			t.Fatalf("the cookie's tag = %q, want %q", got, tag)
		}
		return &http.Cookie{Name: done.Name, Value: token + tail}
	}
	// Denied: a tail that can be no tag is never echoed; the sign-in still runs.
	for name, c := range map[string]struct{ tag, tail string }{
		"too short":          {strings.Repeat("a1", 32), ".short"},
		"empty":              {strings.Repeat("b2", 32), "."},
		"a second separator": {strings.Repeat("c3", 32), "." + strings.Repeat("c3", 32) + ".x"},
	} {
		resp, out := e.finish(t, forged(t, c.tag, c.tail), "web")
		if resp.StatusCode != http.StatusOK || out["token"] == "" || out["intent"] != nil {
			t.Fatalf("%s: %d %v", name, resp.StatusCode, out)
		}
	}
	// Denied: an expectation that names such a tail word for word.
	done := forged(t, strings.Repeat("d4", 32), ".short")
	resp, b := e.doHeaders(t, "POST", oidcFinishPath, "", `{"intent":"short"}`,
		map[string]string{"Cookie": done.Name + "=" + done.Value})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(b, codeOIDCIntentMismatch) {
		t.Fatalf("an expectation of a tail that is no tag: %d %s", resp.StatusCode, b)
	}
}

// TestOIDCIntentMemoryFull: with the memory of spent tags full, a new tag is
// dropped at the callback, so the sign-in runs untagged (the player gives
// nothing back) and the flow states' own memory is untouched.
func TestOIDCIntentMemoryFull(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	for i := 0; e.api.oidc.intents.Use("spent-" + strconv.Itoa(i)); i++ {
	}
	cookie, path := e.startQuery(t, "return=web&intent="+strings.Repeat("e5", 32))
	loc, done := e.callbackDone(t, path, cookie)
	if loc != oidcPublic+oidcDoneWeb || done == nil {
		t.Fatalf("landed on %q (cookie %v)", loc, done)
	}
	if resp, out := e.finish(t, done, "web"); resp.StatusCode != http.StatusOK || out["token"] == "" || out["intent"] != nil {
		t.Fatalf("a tag past a full memory: %d %v", resp.StatusCode, out)
	}
}

// TestOIDCFinish: a sign-in's token reaches the page only in its cookie
// (HttpOnly, Lax, Secure and __Host- on Path=/ for an https public_url, 60 s),
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
		done.Path != "/" || done.Domain != "" || done.MaxAge != 60 || done.Value == "" {
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
	resp, out = e.finish(t, &http.Cookie{Name: hostDoneCookie, Value: "forged"}, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired {
		t.Fatalf("a forged cookie: %d %v", resp.StatusCode, out)
	}
	// Denied: an expired one (issued 61 s ago, with its 60 s life).
	past := auth.New(e.db, func() time.Time { return time.Now().Add(-oidcDoneTTL - time.Second) })
	stale, err := past.IssueToken(context.Background(), alice.ID, auth.KindOIDC, "", oidcDoneTTL)
	if err != nil {
		t.Fatal(err)
	}
	resp, out = e.finish(t, &http.Cookie{Name: hostDoneCookie, Value: stale}, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired {
		t.Fatalf("an expired token: %d %v", resp.StatusCode, out)
	}

	// Allowed: the cookie the sign-in set, once; the answer is exchange's.
	resp, out = e.finish(t, done, "admin-web")
	if _, ok := out["server_id"]; resp.StatusCode != 200 || out["token"] == "" || !ok ||
		out["user"].(map[string]any)["username"] != "alice" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("finish: %d %v", resp.StatusCode, out)
	}
	if c := resp.Cookies(); len(c) != 1 || c[0].Name != hostDoneCookie || c[0].MaxAge >= 0 || c[0].Path != "/" {
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

// TestOIDCFinishNotLockedByExchanges: an address any page made fail ten
// pairing exchanges (a simple cross-site POST, no cookie needed) can still
// finish a sign-in the provider completed: finish has its own limiter.
func TestOIDCFinishNotLockedByExchanges(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	for range 11 {
		e.do(t, "POST", "/api/v1/auth/exchange", "", `{"pairing_token":"guess"}`)
	}
	if r, _ := e.do(t, "POST", "/api/v1/auth/exchange", "", `{"pairing_token":"guess"}`); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("exchange not locked: %d", r.StatusCode)
	}
	_, done := e.signInDone(t, oidcReturnAdmin)
	if resp, out := e.finish(t, done, "admin-web"); resp.StatusCode != http.StatusOK || out["token"] == "" {
		t.Fatalf("finish after failed exchanges: %d %v", resp.StatusCode, out)
	}
}

// clearsDone reports whether resp deleted the finished sign-in's cookie.
func clearsDone(resp *http.Response) bool {
	c := respCookie(resp, hostDoneCookie)
	return c != nil && c.MaxAge < 0
}

// TestOIDCFinishOnlySignInTokens: finish takes only the token a sign-in's
// callback minted. A pairing token planted in the cookie (an invite's QR, a
// person's own /auth/pair) or a session is refused (a login CSRF otherwise:
// the browser signed in as the token's owner) and left unspent; the
// sign-in's token in turn is no pairing token for an exchange.
func TestOIDCFinishOnlySignInTokens(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	alice := e.mustUser(t, "alice", "alice@example.org")
	_, qr, _ := e.redeemCode(t, e.authCode)
	own, err := e.auth.IssueOwnPairingToken(context.Background(), alice.ID, time.Minute)
	if err != nil || qr == "" {
		t.Fatalf("tokens: %q %v", qr, err)
	}
	// Denied: another kind's token; the cookie is cleared all the same.
	for name, tok := range map[string]string{"an invite's QR": qr, "an own pairing": own, "a session": e.session(t, alice.ID)} {
		resp, out := e.finish(t, &http.Cookie{Name: hostDoneCookie, Value: tok}, "admin-web")
		if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired || out["token"] != nil {
			t.Fatalf("%s finished a sign-in: %d %v", name, resp.StatusCode, out)
		}
		if !clearsDone(resp) {
			t.Fatalf("%s: the cookie kept: %v", name, resp.Header.Values("Set-Cookie"))
		}
	}
	// None of them was spent.
	for name, tok := range map[string]string{"the QR": qr, "the own pairing": own} {
		if status, body := e.exchangeToken(t, tok, "phone"); status != http.StatusOK {
			t.Fatalf("%s was spent by the refused finish: %d %s", name, status, body)
		}
	}
	// Allowed: the sign-in's own token, which an exchange refuses.
	_, done := e.signInDone(t, oidcReturnAdmin)
	if status, _ := e.exchangeToken(t, done.Value, "phone"); status == http.StatusOK {
		t.Fatal("an exchange took a sign-in's token")
	}
	if resp, out := e.finish(t, done, "admin-web"); resp.StatusCode != http.StatusOK || out["token"] == "" {
		t.Fatalf("finish: %d %v", resp.StatusCode, out)
	}
}

// Security-critical: a sign-in's token (auth.KindOIDC) authenticates no
// request: it is a 401 on every authenticated route of the table, as the
// header and as ?token=, and is left unspent there (finish still takes it).
func TestOIDCTokenRefusedOnEveryRoute(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	// Two requests per route: more than one address's burst.
	e.api.ipLimiter = newRateLimiter(100000, 100000)
	e.api.mediaLimiter = newRateLimiter(100000, 100000)
	_, done := e.signInDone(t, oidcReturnWeb)
	refusedOnRoutes(t, e.testEnv, "single sign-on", done.Value, authedRoutes(t), "1", "path=x")
	if resp, out := e.finish(t, done, "web"); resp.StatusCode != http.StatusOK || out["token"] == "" {
		t.Fatalf("finish after the refusals: %d %v", resp.StatusCode, out)
	}
}

// TestOIDCFinishCookieName: on an https public_url finish reads only the
// __Host- cookie, so one a sibling subdomain or a plain-http answer planted
// under the bare name finishes nothing (and spends nothing); on plain http,
// where no prefix can be had, it reads the bare name on the flow's path.
func TestOIDCFinishCookieName(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	_, done := e.signInDone(t, oidcReturnAdmin)
	resp, out := e.finish(t, &http.Cookie{Name: oidcDoneCookie, Value: done.Value}, "admin-web")
	if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCNoSignIn {
		t.Fatalf("https, a bare-named cookie: %d %v", resp.StatusCode, out)
	}
	if resp, out := e.finish(t, done, "admin-web"); resp.StatusCode != http.StatusOK || out["token"] == "" {
		t.Fatalf("https, the __Host- cookie: %d %v", resp.StatusCode, out)
	}

	plain := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = "http://audiosilo.test"
		c.Auth.OIDC = config.OIDCConfig{Enabled: true, Issuer: "https://idp.example", ClientID: "audiosilo"}
	})
	bob := plain.mustUser(t, "bob", "")
	tok, err := plain.auth.IssueToken(context.Background(), bob.ID, auth.KindOIDC, "", oidcDoneTTL)
	if err != nil {
		t.Fatal(err)
	}
	if resp, out := plain.finish(t, &http.Cookie{Name: hostDoneCookie, Value: tok}, "web"); resp.StatusCode != http.StatusUnauthorized ||
		out["code"] != codeOIDCNoSignIn {
		t.Fatalf("http, a __Host- cookie: %d %v", resp.StatusCode, out)
	}
	resp, out = plain.finish(t, &http.Cookie{Name: oidcDoneCookie, Value: tok}, "web")
	if resp.StatusCode != http.StatusOK || out["token"] == "" {
		t.Fatalf("http, the bare cookie: %d %v", resp.StatusCode, out)
	}
	if c := resp.Cookies(); len(c) != 1 || c[0].Name != oidcDoneCookie || c[0].Path != oidcCookiePath || c[0].Secure {
		t.Fatalf("http, the clearing cookie: %v", resp.Header.Values("Set-Cookie"))
	}
}

// TestOIDCFinishRefusedWhileOff: a sign-in's token outlives a restart (60 s),
// but finish refuses it once single sign-on can't run (turned off, or its
// provider config broken since the callback): no session, nothing charged,
// the token unspent. The allowed twin is TestOIDCFinish.
func TestOIDCFinishRefusedWhileOff(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]config.OIDCConfig{
		"turned off":    {},
		"misconfigured": {Enabled: true, Issuer: "https://idp.example"}, // no client_id
	} {
		e := newTestEnvWith(t, func(c *config.Config) {
			c.PublicURL = oidcPublic
			c.Auth.OIDC = o
		})
		alice := e.mustUser(t, "alice", "")
		tok, err := e.auth.IssueToken(context.Background(), alice.ID, auth.KindOIDC, "", oidcDoneTTL)
		if err != nil {
			t.Fatal(err)
		}
		resp, out := e.finish(t, &http.Cookie{Name: hostDoneCookie, Value: tok}, "admin-web")
		if resp.StatusCode != http.StatusUnauthorized || out["code"] != codeOIDCSignInExpired || out["token"] != nil {
			t.Fatalf("%s: %d %v", name, resp.StatusCode, out)
		}
		if u, err := e.auth.ConsumeOIDC(context.Background(), tok); err != nil || u.ID != alice.ID {
			t.Fatalf("%s: the refused finish spent the token: %v", name, err)
		}
	}
}

// TestOIDCFinishKeepsCookieUntilTried: a finish refused before it tries the
// token (429, its address over oidcLimiter; 400, a body it can't read) leaves
// the cookie, so the completed sign-in can still be finished in its minute.
func TestOIDCFinishKeepsCookieUntilTried(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	_, done := e.signInDone(t, oidcReturnAdmin)
	cookie := map[string]string{"Cookie": done.Name + "=" + done.Value}

	resp, _ := e.doHeaders(t, "POST", oidcFinishPath, "", `{"device_name":`, cookie)
	if resp.StatusCode != http.StatusBadRequest || clearsDone(resp) {
		t.Fatalf("a bad body: %d %v", resp.StatusCode, resp.Header.Values("Set-Cookie"))
	}
	for range 10 {
		e.finish(t, &http.Cookie{Name: hostDoneCookie, Value: "guess"}, "admin-web")
	}
	resp, _ = e.finish(t, done, "admin-web")
	if resp.StatusCode != http.StatusTooManyRequests || clearsDone(resp) {
		t.Fatalf("over the limit: %d %v", resp.StatusCode, resp.Header.Values("Set-Cookie"))
	}
	e.api.oidcLimiter.Reset("127.0.0.1")
	resp, out := e.finish(t, done, "admin-web")
	if resp.StatusCode != http.StatusOK || out["token"] == "" || !clearsDone(resp) {
		t.Fatalf("finish once the limit lifts: %d %v", resp.StatusCode, out)
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

	// A state forged past the part the cookie is named by: the cookie is
	// found, its sealed state isn't the one sent back.
	cookie, path := e.start(t, oidcReturnAdmin)
	u, _ := url.Parse(path)
	q := u.Query()
	q.Set("state", q.Get("state")[:16]+strings.Repeat("f", 27))
	u.RawQuery = q.Encode()
	wantResult(t, e.callback(t, u.RequestURI(), cookie), oidcFailed)
	// A state no flow has: no cookie by its name.
	q.Set("state", "forged")
	u.RawQuery = q.Encode()
	wantResult(t, e.callback(t, u.RequestURI(), cookie), oidcExpired)

	_, path = e.start(t, oidcReturnAdmin)
	wantResult(t, e.callback(t, path, nil), oidcExpired)

	// Another flow's cookie: under its own name it is no cookie for this
	// state; planted under this state's name, its state isn't this one.
	c1, _ := e.start(t, oidcReturnAdmin)
	_, p2 := e.start(t, oidcReturnAdmin)
	wantResult(t, e.callback(t, p2, c1), oidcExpired)
	planted := &http.Cookie{Name: hostFlowCookieFor(stateOf(t, p2)), Value: c1.Value}
	wantResult(t, e.callback(t, p2, planted), oidcFailed)

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

	// The provider ended it without naming the state: still "failed" (it did
	// not time out), nothing charged, and no flow's cookie touched (whose it
	// was can't be told: callbackDone checks none is), so another tab's
	// sign-in still finishes.
	cookie, path = e.start(t, oidcReturnAdmin)
	for range 12 {
		wantResult(t, e.callback(t, oidcCallbackPath+"?error=access_denied", cookie), oidcFailed)
	}
	if loc := e.callback(t, path, cookie); !strings.Contains(loc, oidcDoneAdmin) {
		t.Fatalf("after an ending that named no state: %q", loc)
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

// TestOIDCPendingNotifies (server#235): a sign-in parked for an admin raises
// oidc_pending once per waiting row (a repeat attempt adds none, a dismissed
// row recorded again is told again), naming the provider, the person and the
// reason's code, never the email, subject or groups; a refused sign-in that
// records nothing (a disabled account) raises none. The pending list carries
// the accounts already linked to each issuer, which Link would refuse.
func TestOIDCPendingNotifies(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	ctx := context.Background()
	tok := e.session(t, e.adminID)
	pending := func() []catalog.ServerEvent {
		t.Helper()
		events, _, err := e.cat.ListServerEvents(ctx, 0, 20, "oidc_pending")
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	// Alice links on her own (a verified email, one account); Carol waits.
	alice := e.mustUser(t, "alice", "alice@example.org")
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("alice: %q", loc)
	}
	e.idp.SetPerson(oidcCarol)
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	events := pending()
	if len(events) != 1 {
		t.Fatalf("events after two attempts = %+v; want one", events)
	}
	if d := events[0].Data; d["provider"] != "Keycloak" || d["person"] != "carol" || d["reason"] != oidc.ReasonAutoCreateOff {
		t.Fatalf("data = %v", d)
	}
	raw, _ := json.Marshal(events[0].Data)
	for _, secret := range []string{"@", "sub-carol", "guests", e.idp.Issuer} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("data %s carries %q", raw, secret)
		}
	}
	resp, body := e.do(t, "GET", "/api/v1/admin/oidc/pending", tok, "")
	var list struct {
		Pending []auth.PendingSignIn `json:"pending"`
		Linked  map[string][]int64   `json:"linked_user_ids"`
	}
	_ = json.Unmarshal([]byte(body), &list)
	if resp.StatusCode != 200 || len(list.Pending) != 1 || len(list.Linked[e.idp.Issuer]) != 1 ||
		list.Linked[e.idp.Issuer][0] != alice.ID {
		t.Fatalf("pending: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/oidc/pending/"+itoa(list.Pending[0].ID), tok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dismiss: %d", resp.StatusCode)
	}
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	if events := pending(); len(events) != 2 {
		t.Fatalf("events after a dismissed row came back = %d; want 2", len(events))
	}
	// A disabled account's sign-in is refused, not parked: nothing raised.
	yes := true
	if err := e.auth.UpdateUser(ctx, alice.ID, auth.UserChanges{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	e.idp.SetPerson(oidcAlice)
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcDisabled)
	if events := pending(); len(events) != 2 {
		t.Fatalf("a refused sign-in raised oidc_pending: %d events", len(events))
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

// TestOIDCWaitingSignInAsksUserinfoAgain: a person userinfo held no email for
// is left waiting; once they have one at the provider, their next attempt
// reads it (allowed: the wait ended what the provider remembered userinfo
// lacked for them, oidc.Provider.AskAgain) and links to their account, where
// the hour's recheck would have left them waiting without it.
func TestOIDCWaitingSignInAsksUserinfoAgain(t *testing.T) {
	t.Parallel()
	bare := oidctest.Person{Subject: oidcCarol.Subject}
	e := newOIDCEnv(t, bare, nil)
	carol := e.mustUser(t, "carol-account", oidcCarol.Email)
	e.idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true})
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcPending)
	e.idp.SetPerson(oidcCarol)
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("the email added since wasn't read: %q", loc)
	}
	if ids, err := e.auth.ListIdentities(context.Background(), carol.ID); err != nil || len(ids) != 1 || ids[0].Email != oidcCarol.Email {
		t.Fatalf("not linked by the email: %+v %v", ids, err)
	}
	if n := e.idp.Calls("userinfo"); n != 2 {
		t.Fatalf("userinfo asked %d times, want 2", n)
	}
}

// TestOIDCUserinfoDownMakesNoSecondAccount: with auto_create on and an ID token
// that leaves the email to userinfo, a first sign-in while userinfo fails is
// refused as unavailable (denied: it would make a second account for someone
// whose email links an existing one); once userinfo answers, the same person
// links to their account (allowed), and a later sign-in while it fails still
// signs in, keeping the identity's stored email.
func TestOIDCUserinfoDownMakesNoSecondAccount(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcCarol, func(o *config.OIDCConfig) { o.AutoCreate = true })
	carol := e.mustUser(t, "carol-account", oidcCarol.Email)
	accounts := func() int {
		t.Helper()
		users, err := e.auth.ListUsers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return len(users)
	}
	before := accounts()
	e.idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true, FailUserinfo: true})
	wantResult(t, e.signIn(t, oidcReturnWeb), oidcUnavailable)
	if ids, err := e.auth.ListIdentities(context.Background(), carol.ID); err != nil || len(ids) != 0 {
		t.Fatalf("linked while userinfo failed: %v %v", ids, err)
	}
	if n := accounts(); n != before {
		t.Fatalf("an account made while userinfo failed: %d, want %d", n, before)
	}
	e.idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true})
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("userinfo back: %q", loc)
	}
	ids, err := e.auth.ListIdentities(context.Background(), carol.ID)
	if err != nil || len(ids) != 1 || ids[0].Email != oidcCarol.Email {
		t.Fatalf("not linked by email: %+v %v", ids, err)
	}
	e.idp.SetKnobs(oidctest.Knobs{UserinfoOnly: true, FailUserinfo: true})
	if loc := e.signIn(t, oidcReturnWeb); !strings.Contains(loc, oidcDoneWeb) {
		t.Fatalf("a linked sign-in while userinfo fails: %q", loc)
	}
	if ids, err := e.auth.ListIdentities(context.Background(), carol.ID); err != nil || len(ids) != 1 ||
		ids[0].Email != oidcCarol.Email || ids[0].Username != oidcCarol.PreferredUsername {
		t.Fatalf("the stored email or username lost: %+v %v", ids, err)
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
	state := strings.Repeat("y", 43)
	bare := oidcCallbackPath + "?code=x&state=" + state
	foreign := &http.Cookie{Name: hostFlowCookieFor(state), Value: "not-this-server's"}
	for range 25 {
		wantResult(t, e.callback(t, bare, nil), oidcExpired)
	}
	for range 9 {
		wantResult(t, e.callback(t, bare, foreign), oidcFailed)
	}
	if loc := e.signIn(t, oidcReturnAdmin); !strings.Contains(loc, oidcDoneAdmin) {
		t.Fatalf("a good sign-in under the limit: %q", loc)
	}
	for range 10 {
		wantResult(t, e.callback(t, bare, foreign), oidcFailed)
	}
	resp := e.get(t, bare, oidcHost)
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

// TestOIDCBudgetFitsRequestTimeout: a sign-in's provider calls end inside the
// request's own timeout, so a slow provider ends on the result panel, not on
// the timeout's bare 503.
func TestOIDCBudgetFitsRequestTimeout(t *testing.T) {
	t.Parallel()
	if oidc.UseTime+5*time.Second > requestTimeout {
		t.Fatalf("oidc.UseTime %v leaves too little of requestTimeout %v", oidc.UseTime, requestTimeout)
	}
}

// TestOIDCScopesFollowGroupPolicy: with the default scopes, /start asks for
// groups only while admin_group is set, or allowed_group with auto_create on
// (server#406), as the
// console saves them (live, no restart), and the status and the settings'
// default say what is asked.
func TestOIDCScopesFollowGroupPolicy(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	tok := e.session(t, e.adminID)
	check := func(want string) {
		t.Helper()
		e.start(t, oidcReturnAdmin)
		if got := e.idp.LastAuthorize().Get("scope"); got != want {
			t.Fatalf("asked for %q, want %q", got, want)
		}
		list := `["` + strings.ReplaceAll(want, " ", `","`) + `"]`
		_, body := e.do(t, "GET", "/api/v1/admin/oidc/status", tok, "")
		if !strings.Contains(body, `"scopes":`+list) {
			t.Fatalf("status scopes, want %q: %s", want, body)
		}
		_, body = e.do(t, "GET", "/api/v1/admin/settings", tok, "")
		if !strings.Contains(body, `"scopes_default":`+list) {
			t.Fatalf("settings' default scopes, want %q: %s", want, body)
		}
	}
	save := func(patch string) {
		t.Helper()
		if resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", tok, `{"signin":`+patch+`}`); resp.StatusCode != 200 {
			t.Fatalf("save %s: %d %s", patch, resp.StatusCode, body)
		}
	}
	check("openid profile email")
	// allowed_group gates only an account a sign-in makes: with auto_create
	// off it decides nothing, so groups aren't asked for (denied); on, they are.
	save(`{"allowed_group":"family"}`)
	check("openid profile email")
	save(`{"auto_create":true}`)
	check("openid profile email groups")
	save(`{"allowed_group":"","auto_create":false,"admin_group":"admins"}`)
	check("openid profile email groups")
}

// TestOIDCStartProviderGone: a provider discovered before and gone down since
// is noticed at /start (the probe), which lands on the unavailable panel
// rather than sending the browser to it; the status says so too. Back up, the
// sign-in starts again.
func TestOIDCStartProviderGone(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	now := time.Now()
	p, err := oidc.New(oidc.Settings{Issuer: e.idp.Issuer, ClientID: "audiosilo", ClientSecret: oidcSecret,
		UsernameClaim: "preferred_username", GroupsClaim: "groups", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	e.api.oidc.provider = p
	e.start(t, oidcReturnAdmin)
	e.idp.SetKnobs(oidctest.Knobs{FailDiscovery: true})
	now = now.Add(time.Minute)
	resp := e.get(t, oidcStartPath+"?return=admin", oidcHost)
	if loc := resp.Header.Get("Location"); loc != oidcPublic+"/connect#oidc="+oidcUnavailable {
		t.Fatalf("a provider gone down: %d %q", resp.StatusCode, loc)
	}
	_, body := e.do(t, "GET", "/api/v1/admin/oidc/status", e.session(t, e.adminID), "")
	if !strings.Contains(body, `"state":"error"`) {
		t.Fatalf("status of a provider gone down: %s", body)
	}
	e.idp.SetKnobs(oidctest.Knobs{})
	now = now.Add(time.Minute)
	e.start(t, oidcReturnAdmin)
}

// TestEvictionTallyLogsOncePerHour: at the waiting list's cap every new
// person's attempt drops a row, and the warning is not one line per attempt:
// the first eviction is reported (allowed), those within the hour after it
// are held back (denied) and reported together with the next one past it.
func TestEvictionTallyLogsOncePerHour(t *testing.T) {
	t.Parallel()
	var e evictionTally
	now := time.Unix(1_800_000_000, 0)
	if n := e.note(1, now); n != 1 {
		t.Fatalf("the first eviction reported %d, want 1", n)
	}
	for range 3 {
		now = now.Add(time.Minute)
		if n := e.note(1, now); n != 0 {
			t.Fatalf("an eviction within the hour reported %d, want none", n)
		}
	}
	now = now.Add(time.Hour)
	if n := e.note(2, now); n != 5 {
		t.Fatalf("past the hour reported %d, want the 5 dropped since the last line", n)
	}
	if n := e.note(1, now.Add(time.Second)); n != 0 {
		t.Fatalf("right after a report: %d, want none", n)
	}
}

// browser is a cookie jar for one host, as a browser keeps it: a Set-Cookie
// replaces the cookie of its name, a negative Max-Age deletes it, and the
// cookies go back oldest first.
type browser struct{ cookies []*http.Cookie }

func (b *browser) apply(resp *http.Response) {
	for _, c := range resp.Cookies() {
		b.cookies = slices.DeleteFunc(b.cookies, func(o *http.Cookie) bool { return o.Name == c.Name })
		if c.MaxAge >= 0 {
			b.cookies = append(b.cookies, c)
		}
	}
}

func (b *browser) named(name string) *http.Cookie {
	for _, c := range b.cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// flows counts the flow cookies the browser holds.
func (b *browser) flows() int {
	n := 0
	for _, c := range b.cookies {
		if strings.HasPrefix(c.Name, hostFlowCookie) {
			n++
		}
	}
	return n
}

// start runs /start in the browser and the provider's authorize: the
// callback's path.
func (b *browser) start(t *testing.T, e *oidcEnv, query string) string {
	t.Helper()
	resp := e.get(t, oidcStartPath+"?"+query, oidcHost, b.cookies...)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	b.apply(resp)
	authz, err := noFollow.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	authz.Body.Close()
	back, err := url.Parse(authz.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return back.RequestURI()
}

// callback sends the browser back on path: where it lands.
func (b *browser) callback(t *testing.T, e *oidcEnv, path string) string {
	t.Helper()
	resp := e.get(t, path, oidcHost, b.cookies...)
	b.apply(resp)
	return resp.Header.Get("Location")
}

// TestOIDCTwoTabs (server#406): a sign-in started in a second tab keeps the
// first's flow, so both finish, in either order; the oldest flows beyond
// oidcMaxFlows are dropped at /start, and only theirs.
func TestOIDCTwoTabs(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	var b browser
	first := b.start(t, e, "return=admin")
	second := b.start(t, e, "return=web")
	if b.flows() != 2 {
		t.Fatalf("the second tab's start left %d flows", b.flows())
	}
	// The first tab comes back: the second's flow stays.
	if loc := b.callback(t, e, first); loc != oidcPublic+oidcDoneAdmin {
		t.Fatalf("the first tab landed on %q", loc)
	}
	if resp, out := e.finish(t, b.named(hostDoneCookie), "admin-web"); resp.StatusCode != 200 || out["token"] == "" {
		t.Fatalf("the first tab's finish: %d %v", resp.StatusCode, out)
	}
	if b.flows() != 1 {
		t.Fatalf("after the first tab: %d flows", b.flows())
	}
	if loc := b.callback(t, e, second); loc != oidcPublic+oidcDoneWeb {
		t.Fatalf("the second tab landed on %q", loc)
	}
	if resp, out := e.finish(t, b.named(hostDoneCookie), "web"); resp.StatusCode != 200 || out["token"] == "" {
		t.Fatalf("the second tab's finish: %d %v", resp.StatusCode, out)
	}

	// The cap: the oldest flow goes, the newest held still finish.
	var many browser
	paths := make([]string, oidcMaxFlows+1)
	for i := range paths {
		paths[i] = many.start(t, e, "return=admin")
	}
	if many.flows() != oidcMaxFlows {
		t.Fatalf("%d flows held, want %d", many.flows(), oidcMaxFlows)
	}
	wantResult(t, many.callback(t, e, paths[0]), oidcExpired)
	for _, p := range paths[1:] {
		if loc := many.callback(t, e, p); loc != oidcPublic+oidcDoneAdmin {
			t.Fatalf("a held flow landed on %q", loc)
		}
	}
}

// pathBrowser is a browser in front of a proxy that serves the server under
// prefix: a cookie jar that keeps each cookie to its path. It asks for a path
// as the address bar has it, sends only the cookies whose path holds it, and
// the proxy takes the prefix off.
type pathBrowser struct {
	prefix  string
	cookies []*http.Cookie
}

// sent are the cookies the browser sends to path.
func (b *pathBrowser) sent(path string) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range b.cookies {
		if strings.HasPrefix(path, c.Path) {
			out = append(out, c)
		}
	}
	return out
}

// flows counts the flow cookies the browser sends to path.
func (b *pathBrowser) flows(path string) int {
	n := 0
	for _, c := range b.sent(path) {
		if strings.HasPrefix(c.Name, oidcCookie) {
			n++
		}
	}
	return n
}

func (b *pathBrowser) get(t *testing.T, e *oidcEnv, path string) *http.Response {
	t.Helper()
	resp := e.get(t, strings.TrimPrefix(path, b.prefix), oidcHost, b.sent(path)...)
	for _, c := range resp.Cookies() {
		b.cookies = slices.DeleteFunc(b.cookies, func(o *http.Cookie) bool { return o.Name == c.Name && o.Path == c.Path })
		if c.MaxAge >= 0 {
			b.cookies = append(b.cookies, c)
		}
	}
	return resp
}

// start runs /start at path and the provider's authorize: the callback's
// path as the provider sends the browser to it.
func (b *pathBrowser) start(t *testing.T, e *oidcEnv, path string) string {
	t.Helper()
	resp := b.get(t, e, path)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	authz, err := noFollow.Get(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	authz.Body.Close()
	back, err := url.Parse(authz.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(back.Path, b.prefix+oidcCallbackPath) {
		t.Fatalf("authorize sent the browser to %q", authz.Header.Get("Location"))
	}
	return back.RequestURI()
}

// TestOIDCFlowsHeldUnderPrefix (server#406): on a plain-http public_url with a
// path (a proxy serving the server under /audiosilo/) the console and the web
// player still ask /start on their origin's own /api/v1, while the provider
// sends the callback under the prefix. /start sees the flows the browser
// holds at either, so the cap and the same-trip tag check (audiosilo#202)
// hold there as anywhere, and the callback takes its flow's cookie from both.
func TestOIDCFlowsHeldUnderPrefix(t *testing.T) {
	t.Parallel()
	const prefix = "/audiosilo"
	public := "http://" + oidcHost + prefix
	e := newOIDCEnvAt(t, public, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	b := &pathBrowser{prefix: prefix}
	// finish lands callback and asks the finish where the player does (the
	// origin's own /api/v1), expecting the tag want when it is not "".
	finish := func(t *testing.T, callback, want string) (*http.Response, map[string]any) {
		t.Helper()
		resp := b.get(t, e, callback)
		if loc := resp.Header.Get("Location"); loc != public+oidcDoneWeb {
			t.Fatalf("landed on %q", loc)
		}
		headers := map[string]string{}
		for _, c := range b.sent(oidcFinishPath) {
			if c.Name == oidcDoneCookie {
				headers["Cookie"] = c.Name + "=" + c.Value
			}
		}
		body := `{"device_name":"web"}`
		if want != "" {
			body = `{"device_name":"web","intent":"` + want + `"}`
		}
		fin, raw := e.doHeaders(t, "POST", oidcFinishPath, "", body, headers)
		var out map[string]any
		_ = json.Unmarshal([]byte(raw), &out)
		return fin, out
	}

	// Denied: a trip abandoned at the provider, its start URL sent again from
	// the history while the browser still holds its flow cookie: the new flow
	// goes untagged, so the finish the player asks for its tag is refused.
	tag := strings.Repeat("5c", 32)
	first := b.start(t, e, oidcStartPath+"?return=web&intent="+tag)
	again := b.start(t, e, oidcStartPath+"?return=web&intent="+tag)
	if fin, out := finish(t, again, tag); fin.StatusCode != http.StatusForbidden || out["code"] != codeOIDCIntentMismatch || out["token"] != nil {
		t.Fatalf("an abandoned trip's start sent again: %d %v", fin.StatusCode, out)
	}
	// The finished flow's cookie is gone wherever it was held, the abandoned
	// one's still there for its own return.
	for _, at := range []string{oidcStartPath, prefix + oidcCallbackPath} {
		if n := b.flows(at); n != 1 {
			t.Fatalf("%d flows held at %s after one of two finished", n, at)
		}
	}
	// Allowed: the abandoned trip comes back and finishes, with its own tag.
	if fin, out := finish(t, first, tag); fin.StatusCode != http.StatusOK || out["token"] == "" || out["intent"] != tag {
		t.Fatalf("the first trip: %d %v", fin.StatusCode, out)
	}

	// The cap, asked where the console and the player ask and where the hop
	// to public_url does: never more than oidcMaxFlows held.
	var last string
	for i := range oidcMaxFlows + 2 {
		at := oidcStartPath + "?return=web"
		if i%2 == 1 {
			at = prefix + oidcStartPath + "?return=web&hop=1"
		}
		last = b.start(t, e, at)
	}
	for _, at := range []string{oidcStartPath, prefix + oidcCallbackPath} {
		if n := b.flows(at); n != oidcMaxFlows {
			t.Fatalf("%d flows held at %s, want %d", n, at, oidcMaxFlows)
		}
	}
	if fin, out := finish(t, last, ""); fin.StatusCode != http.StatusOK || out["token"] == "" {
		t.Fatalf("the last flow under the cap: %d %v", fin.StatusCode, out)
	}
}

// TestOIDCConsoleDeepLink (server#406): the console page a sign-in started
// from (?next=, through the hop too) is where it lands; anything that isn't a
// console path on this origin, or a web sign-in's, lands on the start page.
func TestOIDCConsoleDeepLink(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	e.mustUser(t, "alice", "alice@example.org")
	land := func(t *testing.T, query string) string {
		t.Helper()
		cookie, path := e.startQuery(t, query)
		return e.callback(t, path, cookie)
	}
	deep := "/admin/people/user/3?tab=devices"
	cookie, path := e.startQuery(t, "return=admin&next="+url.QueryEscape(deep))
	loc, done := e.callbackDone(t, path, cookie)
	if resp, out := e.finish(t, done, "admin-web"); loc != oidcPublic+deep+oidcDoneMarker || resp.StatusCode != 200 || out["token"] == "" {
		t.Fatalf("the deep link's finish: %q %d %v", loc, resp.StatusCode, out)
	}
	for _, bad := range []string{"//evil.example/admin/", "https://evil.example/admin/", "/web/connect", "/admin/../connect",
		"/admin/%2e%2e/connect", "/admin//evil.example", "/admin/x#oidc=done", "/admin/\\evil.example", "/admin/x y",
		`/admin/x?q="y"`,
		"/admin/" + strings.Repeat("a", oidcNextMax)} {
		if loc := land(t, "return=admin&next="+url.QueryEscape(bad)); loc != oidcPublic+oidcDoneAdmin {
			t.Fatalf("next=%q landed on %q", bad, loc)
		}
	}
	if loc := land(t, "return=web&next="+url.QueryEscape(deep)); loc != oidcPublic+oidcDoneWeb {
		t.Fatalf("a web sign-in with a deep link landed on %q", loc)
	}
	// The hop keeps a good one, never a bad one.
	resp := e.get(t, oidcStartPath+"?return=admin&next="+url.QueryEscape(deep), "evil.example")
	if resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?hop=1&next="+url.QueryEscape(deep)+"&return=admin" {
		t.Fatalf("hop with a deep link: %q", resp.Header.Get("Location"))
	}
	resp = e.get(t, oidcStartPath+"?return=admin&next=%2F%2Fevil.example", "evil.example")
	if resp.Header.Get("Location") != oidcPublic+oidcStartPath+"?hop=1&return=admin" {
		t.Fatalf("hop with a bad deep link: %q", resp.Header.Get("Location"))
	}
}

func TestValidNext(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"/admin/":                         true,
		"/admin/people":                   true,
		"/admin/people/user/3?tab=access": true,
		"/admin/library/books/":           true,
		"/admin/search?q=caf%C3%A9":       true,
		"/admin":                          false,
		"":                                false,
		"/connect":                        false,
		"//evil.example/admin/":           false,
		"https://evil.example/admin/":     false,
		"/admin/../connect":               false,
		"/admin/./people":                 false,
		"/admin/%2E%2E/connect":           false,
		"/admin/%2Fevil":                  false, // read as an empty segment: refused, never needed
		"/admin//evil.example":            false,
		"/admin/%5Cevil":                  false,
		"/admin/x#y":                      false,
		`/admin/x?q="y"`:                  false, // a browser sends %22; raw, the sealed JSON doubles it
		"/admin/x?q=%22y%22":              true,
		"/admin/x\ny":                     false,
		"/admin/café":                     false,
	} {
		if got := validNext(s); got != want {
			t.Errorf("validNext(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestOIDCFlowCookiesFitAHeader: the longest deep link validNext takes, of any
// one character it takes, beside the longest tag still seals into a flow
// cookie small enough that the oidcMaxFlows a browser holds (a page elsewhere
// can start them) stay well under a proxy's 8 KB header limit: no character
// taken costs the sealed JSON more than its own byte.
func TestOIDCFlowCookiesFitAHeader(t *testing.T) {
	t.Parallel()
	e := newOIDCEnv(t, oidcAlice, nil)
	taken := 0
	for c := byte(' '); c < 0x80; c++ {
		head := oidcConsolePath + "x?"
		next := head + strings.Repeat(string(c), oidcNextMax-len(head))
		if !validNext(next) {
			continue
		}
		taken++
		v, err := e.api.oidc.sealer.Seal(oidc.Flow{State: strings.Repeat("s", 43), Nonce: strings.Repeat("n", 43),
			Verifier: strings.Repeat("v", 43), RedirectURI: e.api.oidcRedirectURI(), Return: oidcReturnAdmin,
			Intent: strings.Repeat("i", 128), Next: next})
		if err != nil {
			t.Fatal(err)
		}
		// "name=value; " as the Cookie header carries each.
		if size := len(hostFlowCookieFor(strings.Repeat("s", 43))) + len(v) + 3; size*oidcMaxFlows > 5500 {
			t.Errorf("a deep link of %q: %d bytes a flow, %d for %d", c, size, size*oidcMaxFlows, oidcMaxFlows)
		}
	}
	if taken < 60 {
		t.Fatalf("only %d characters taken: the check ran on too few", taken)
	}
}

// TestOIDCSignInProvider (server#408): the console's people list and a
// person's page say which single sign-on they sign in with, from the server:
// only for an identity from the provider it runs, on an account it signs in.
func TestOIDCSignInProvider(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newOIDCEnv(t, oidcAlice, nil)
	alice := e.mustUser(t, "alice", "alice@example.org")
	if loc := e.signIn(t, oidcReturnWeb); loc != oidcPublic+oidcDoneWeb {
		t.Fatalf("alice's sign-in landed on %q", loc)
	}
	demo, err := e.auth.CreateDemoUser(ctx, "demo-1")
	if err != nil {
		t.Fatal(err)
	}
	member, _, _, err := e.auth.CreateMember(ctx, e.adminID, auth.NewMember{Name: "Pip", Kind: auth.MemberGrownup,
		Access: auth.MemberAll}, auth.MemberPolicy{Enabled: true, Max: 5})
	if err != nil {
		t.Fatal(err)
	}
	old := e.mustUser(t, "olive", "")
	link := func(t *testing.T, env *testEnv, user int64, issuer, subject string) {
		t.Helper()
		if _, err := env.db.ExecContext(ctx, `INSERT INTO user_identities(user_id, issuer, subject, link_method, linked_at)
			VALUES(?, ?, ?, 'admin', 't')`, user, issuer, subject); err != nil {
			t.Fatal(err)
		}
	}
	link(t, e.testEnv, demo.ID, e.idp.Issuer, "sub-demo")
	link(t, e.testEnv, member.ID, e.idp.Issuer, "sub-pip")
	link(t, e.testEnv, old.ID, "https://old-idp.example", "sub-olive")

	admin := e.session(t, e.adminID)
	providers := func(t *testing.T, env *testEnv) map[string]string {
		t.Helper()
		resp, body := env.do(t, "GET", "/api/v1/admin/users", env.session(t, env.adminID), "")
		var out struct {
			Users []map[string]any `json:"users"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("list: %d %s", resp.StatusCode, body)
		}
		got := map[string]string{}
		for _, u := range out.Users {
			p, ok := u["sign_in_provider"].(string)
			if !ok {
				t.Fatalf("no sign_in_provider on %v", u)
			}
			got[u["username"].(string)] = p
		}
		return got
	}
	// Allowed: alice's identity is from the provider the server runs.
	// Denied: a demo account's, a household member's, one from a provider no
	// longer configured, and none at all.
	got := providers(t, e.testEnv)
	if got["alice"] != "Keycloak" || got["demo-1"] != "" || got[member.Username] != "" || got["olive"] != "" || got["admin"] != "" {
		t.Fatalf("list: %v", got)
	}
	detail := func(t *testing.T, id int64) (map[string]any, []any) {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/admin/users/"+itoa(id), admin, "")
		var out struct {
			User       map[string]any `json:"user"`
			Identities []any          `json:"identities"`
		}
		if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &out) != nil {
			t.Fatalf("detail: %d %s", resp.StatusCode, body)
		}
		return out.User, out.Identities
	}
	u, ids := detail(t, alice.ID)
	if u["sign_in_provider"] != "Keycloak" || len(ids) != 1 || ids[0].(map[string]any)["provider"] != "Keycloak" {
		t.Fatalf("alice's page: %v %v", u["sign_in_provider"], ids)
	}
	u, ids = detail(t, old.ID)
	if u["sign_in_provider"] != "" || len(ids) != 1 || ids[0].(map[string]any)["provider"] != "" {
		t.Fatalf("an old provider's identity: %v %v", u["sign_in_provider"], ids)
	}
	if u, _ := detail(t, demo.ID); u["sign_in_provider"] != "" {
		t.Fatalf("a demo account: %v", u["sign_in_provider"])
	}
	// A PATCH answers the same.
	resp, body := e.do(t, "PATCH", "/api/v1/admin/users/"+itoa(alice.ID), admin, `{"email":"alice@example.org"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"sign_in_provider":"Keycloak"`) {
		t.Fatalf("patch: %d %s", resp.StatusCode, body)
	}

	// Denied: single sign-on off, the same identity is no way in.
	off := newTestEnv(t)
	bob := off.mustUser(t, "bob", "")
	link(t, off, bob.ID, e.idp.Issuer, "sub-bob")
	if got := providers(t, off); got["bob"] != "" {
		t.Fatalf("off: %v", got)
	}
}
