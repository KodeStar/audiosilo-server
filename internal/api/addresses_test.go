package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

type pairingWire struct {
	ServerID     string     `json:"server_id"`
	BaseURL      string     `json:"base_url"`
	PairingToken string     `json:"pairing_token"`
	URI          string     `json:"uri"`
	WebURL       string     `json:"web_url"`
	Addresses    *Addresses `json:"addresses"`
}

// linkQuery parses a pairing link and returns its query.
func linkQuery(t *testing.T, link string) url.Values {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("parse %q: %v", link, err)
	}
	return u.Query()
}

// The pairing payload carries both addresses, and both links carry them as
// home=/away= params that round-trip through url.Parse, after the existing
// params (which are unchanged).
func TestPairingCarriesAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = "https://books.example.com"
		c.LANURL = "http://192.168.1.20:8080"
		c.ServerID = "srv-test-id"
	})
	resp, body := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redeem = %d %s", resp.StatusCode, body)
	}
	var p pairingWire
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	want := Addresses{Home: "http://192.168.1.20:8080", Away: "https://books.example.com"}
	if p.Addresses == nil || *p.Addresses != want {
		t.Fatalf("addresses = %+v, want %+v (%s)", p.Addresses, want, body)
	}

	if prefix := "https://books.example.com/web/connect?token=" + p.PairingToken + "&"; !strings.HasPrefix(p.WebURL, prefix) {
		t.Fatalf("web_url %q must start with the unchanged %q", p.WebURL, prefix)
	}
	q := linkQuery(t, p.WebURL)
	if q.Get("token") != p.PairingToken || q.Get("home") != want.Home || q.Get("away") != want.Away ||
		q.Get("sid") != "srv-test-id" {
		t.Fatalf("web_url params = %v", q)
	}

	if !strings.HasPrefix(p.URI, "audiosilo://connect?server=") {
		t.Fatalf("uri %q must keep its server= first", p.URI)
	}
	q = linkQuery(t, p.URI)
	if q.Get("server") != "https://books.example.com" || q.Get("token") != p.PairingToken ||
		q.Get("home") != want.Home || q.Get("away") != want.Away || q.Get("sid") != "srv-test-id" {
		t.Fatalf("uri params = %v", q)
	}
	if p.ServerID != "srv-test-id" {
		t.Fatalf("server_id = %q, want srv-test-id", p.ServerID)
	}
	// sid= comes last, after home=/away=.
	for _, link := range []string{p.WebURL, p.URI} {
		if !strings.HasSuffix(link, "&sid=srv-test-id") {
			t.Fatalf("%q must end with &sid=srv-test-id", link)
		}
	}
}

// With no lan_url, a request that arrived on a home-network address teaches the
// device that address as home; with no public_url there is no away.
func TestPairingAddressesDerivedFromLANRequest(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	resp, body := e.doHeaders(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`, map[string]string{"Host": "192.168.1.20:8080"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("redeem = %d %s", resp.StatusCode, body)
	}
	var p pairingWire
	_ = json.Unmarshal([]byte(body), &p)
	if p.Addresses == nil || *p.Addresses != (Addresses{Home: "http://192.168.1.20:8080"}) {
		t.Fatalf("addresses = %+v (%s)", p.Addresses, body)
	}
	for _, link := range []string{p.URI, p.WebURL} {
		q := linkQuery(t, link)
		if q.Get("home") != "http://192.168.1.20:8080" || q.Has("away") {
			t.Fatalf("%s params = %v, want home only", link, q)
		}
	}
}

// Neither address known (no config; a loopback, public or CGNAT request): no
// addresses field and links exactly as before.
func TestPairingWithoutAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) { c.ServerID = "srv-test-id" })
	for _, host := range []string{"", "books.example.com", "100.64.1.2:8080", "localhost:8080"} {
		resp, body := e.doHeaders(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`, map[string]string{"Host": host})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%q: redeem = %d %s", host, resp.StatusCode, body)
		}
		var p pairingWire
		_ = json.Unmarshal([]byte(body), &p)
		if strings.Contains(body, `"addresses"`) || p.Addresses != nil {
			t.Fatalf("%q: no addresses expected: %s", host, body)
		}
		if q := linkQuery(t, p.WebURL); len(q) != 2 || q.Get("token") != p.PairingToken || q.Get("sid") != "srv-test-id" {
			t.Fatalf("%q: web_url %q must carry only token and sid", host, p.WebURL)
		}
		if q := linkQuery(t, p.URI); len(q) != 3 || q.Get("sid") != "srv-test-id" {
			t.Fatalf("%q: uri %q must carry only server, token and sid", host, p.URI)
		}
	}
}

// A request through a reverse proxy derives no home address from its Host (the
// proxy's upstream, e.g. a container name or bridge IP); a configured lan_url
// still applies.
func TestProxiedRequestDerivesNoHome(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	_, memberTok := opsTokens(t, e)
	for _, fwd := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		for _, host := range []string{"audiosilo:8080", "172.18.0.5:8080"} {
			_, body := e.doHeaders(t, "GET", "/api/v1/addresses", memberTok, "", map[string]string{"Host": host, fwd: "x"})
			if body != "{}\n" {
				t.Fatalf("%s via %s = %q, want {}", host, fwd, body)
			}
		}
	}

	set := newTestEnvWith(t, func(c *config.Config) { c.LANURL = "http://192.168.1.20:8080" })
	_, setMember := opsTokens(t, set)
	_, body := set.doHeaders(t, "GET", "/api/v1/addresses", setMember, "", map[string]string{"Host": "audiosilo:8080", "X-Forwarded-For": "203.0.113.9"})
	if body != `{"home":"http://192.168.1.20:8080"}`+"\n" {
		t.Fatalf("configured lan_url behind a proxy = %q", body)
	}
}

// /auth/pair (an existing session adding a device) carries them too.
func TestAuthPairCarriesAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) { c.PublicURL = "https://books.example.com" })
	_, memberTok := opsTokens(t, e)
	resp, body := e.doHeaders(t, "POST", "/api/v1/auth/pair", memberTok, "", map[string]string{"Host": "nas.local:8080"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair = %d %s", resp.StatusCode, body)
	}
	var p pairingWire
	_ = json.Unmarshal([]byte(body), &p)
	want := Addresses{Home: "http://nas.local:8080", Away: "https://books.example.com"}
	if p.Addresses == nil || *p.Addresses != want {
		t.Fatalf("addresses = %+v, want %+v", p.Addresses, want)
	}
}

// The exchange and login answers carry the addresses; without any, the field is
// absent and the envelope is as before.
func TestExchangeAndLoginCarryAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = "https://books.example.com"
		c.LANURL = "http://10.0.0.2:8080"
	})
	want := Addresses{Home: "http://10.0.0.2:8080", Away: "https://books.example.com"}
	type session struct {
		Token     string     `json:"token"`
		ServerID  *string    `json:"server_id"`
		Addresses *Addresses `json:"addresses"`
	}

	_, ptok, _ := e.redeemCode(t, e.authCode)
	status, body := e.exchangeToken(t, ptok, "phone")
	var ex session
	_ = json.Unmarshal([]byte(body), &ex)
	if status != http.StatusOK || ex.Token == "" || ex.ServerID == nil || ex.Addresses == nil || *ex.Addresses != want {
		t.Fatalf("exchange = %d %s", status, body)
	}

	resp, body := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"admin-password"}`)
	var li session
	_ = json.Unmarshal([]byte(body), &li)
	if resp.StatusCode != http.StatusOK || li.Token == "" || li.ServerID == nil || li.Addresses == nil || *li.Addresses != want {
		t.Fatalf("login = %d %s", resp.StatusCode, body)
	}

	plain := newTestEnv(t)
	resp, body = plain.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"admin-password"}`)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, `"addresses"`) {
		t.Fatalf("login without addresses = %d %s", resp.StatusCode, body)
	}
}

// The demo session answer and its pairing payload carry them as well.
func TestDemoSessionCarriesAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	if _, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Demo", Root: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	e.cfg.Demo.Enabled = true
	e.cfg.Demo.Library = "Demo"
	e.cfg.PublicURL = "https://demo.example.com"

	resp, body := e.doHeaders(t, "POST", "/api/v1/demo/session", "", "", map[string]string{"Host": "192.168.1.20:8080"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("demo = %d %s", resp.StatusCode, body)
	}
	var out struct {
		Token     string      `json:"token"`
		Addresses *Addresses  `json:"addresses"`
		Pairing   pairingWire `json:"pairing"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	want := Addresses{Home: "http://192.168.1.20:8080", Away: "https://demo.example.com"}
	if out.Token == "" || out.Addresses == nil || *out.Addresses != want ||
		out.Pairing.Addresses == nil || *out.Pairing.Addresses != want {
		t.Fatalf("demo addresses: %s", body)
	}

	// A demo session may read them later too.
	if resp, got := e.do(t, "GET", "/api/v1/addresses", out.Token, ""); resp.StatusCode != http.StatusOK ||
		got != `{"away":"https://demo.example.com"}`+"\n" {
		t.Fatalf("demo GET /addresses = %d %q", resp.StatusCode, got)
	}
}

// GET /addresses: any signed-in caller (member, admin, API key) is answered; no
// token is 401.
func TestGetAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) { c.PublicURL = "https://books.example.com/" })
	adminTok, memberTok := opsTokens(t, e)
	key, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindAPI, "script", 0)
	if err != nil {
		t.Fatal(err)
	}

	// Denied: no token, or a token that isn't one.
	if resp, _ := e.do(t, "GET", "/api/v1/addresses", "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/addresses", "not-a-token", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", resp.StatusCode)
	}

	// Allowed, from the home network: both addresses.
	for _, tok := range []string{memberTok, adminTok, key} {
		resp, body := e.doHeaders(t, "GET", "/api/v1/addresses", tok, "", map[string]string{"Host": "192.168.1.20:8080"})
		var got Addresses
		_ = json.Unmarshal([]byte(body), &got)
		if resp.StatusCode != http.StatusOK || got != (Addresses{Home: "http://192.168.1.20:8080", Away: "https://books.example.com"}) {
			t.Fatalf("GET /addresses = %d %s", resp.StatusCode, body)
		}
	}
	// From outside: the away address only.
	if _, body := e.doHeaders(t, "GET", "/api/v1/addresses", memberTok, "", map[string]string{"Host": "books.example.com"}); body != `{"away":"https://books.example.com"}`+"\n" {
		t.Fatalf("public request = %q", body)
	}
	// Nothing known: {}.
	plain := newTestEnv(t)
	_, plainMember := opsTokens(t, plain)
	if resp, body := plain.do(t, "GET", "/api/v1/addresses", plainMember, ""); resp.StatusCode != http.StatusOK || body != "{}\n" {
		t.Fatalf("nothing known = %d %q, want {}", resp.StatusCode, body)
	}
}

// general.lan_url saves from the console and applies on the next request; the
// environment locks it.
func TestLANURLSettingLive(t *testing.T) {
	e := newTestEnv(t)
	adminTok, memberTok := opsTokens(t, e)
	resp, body := e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"general":{"lan_url":"http://192.168.1.20:8080/"}}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"restart_pending":[]`) {
		t.Fatalf("lan_url applies without a restart: %s", body)
	}
	if _, s := e.do(t, "GET", "/api/v1/admin/settings", adminTok, ""); !strings.Contains(s, `"lan_url":"http://192.168.1.20:8080"`) {
		t.Fatalf("settings missing lan_url: %s", s)
	}
	if _, got := e.do(t, "GET", "/api/v1/addresses", memberTok, ""); got != `{"home":"http://192.168.1.20:8080"}`+"\n" {
		t.Fatalf("GET /addresses after the save = %q", got)
	}
	resp, body = e.do(t, "PATCH", "/api/v1/admin/settings", adminTok, `{"general":{"lan_url":"192.168.1.20"}}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"general.lan_url"`) {
		t.Fatalf("invalid lan_url = %d %s", resp.StatusCode, body)
	}

	t.Setenv("AUDIOSILO_LAN_URL", "http://nas.local:8080")
	cfg, _, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locked := newTestEnvWith(t, func(c *config.Config) { *c = *cfg })
	lockedAdmin, _ := opsTokens(t, locked)
	resp, body = locked.do(t, "PATCH", "/api/v1/admin/settings", lockedAdmin, `{"general":{"lan_url":"http://x.lan"}}`)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, codeSettingLocked) {
		t.Fatalf("env lan_url = %d %s, want 409", resp.StatusCode, body)
	}
}

func TestServerAdvertisesAddresses(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil || !info.Capabilities["addresses"] {
		t.Fatalf("capabilities.addresses must be true: %s", body)
	}
}

// overTLS sends a request to e's handler over HTTPS (as a client of the
// self-signed port would) with the given Host, and returns the status and body.
func overTLS(t *testing.T, e *testEnv, method, path, token, body, host string) (int, string) {
	t.Helper()
	srv := httptest.NewTLSServer(e.api.Handler())
	defer srv.Close()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Home HTTP (tls.home_http, on by default on a self-signed server): a request
// that came over https on a home-network host still teaches the http home
// address, the one the native apps can use, in the pairing payload and its
// links, the exchange answer and GET /addresses. base_url, web_url and the
// uri's server= keep the request's own https.
func TestHomeHTTPAddresses(t *testing.T) {
	t.Parallel()
	const host = "192.168.1.205:8080"
	e := newTestEnvWith(t, func(c *config.Config) { c.ServerID = "srv-test-id" })
	status, body := overTLS(t, e, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`, host)
	if status != http.StatusOK {
		t.Fatalf("redeem = %d %s", status, body)
	}
	var p pairingWire
	_ = json.Unmarshal([]byte(body), &p)
	want := Addresses{Home: "http://" + host}
	if p.Addresses == nil || *p.Addresses != want {
		t.Fatalf("pairing addresses = %+v, want %+v (%s)", p.Addresses, want, body)
	}
	if p.BaseURL != "https://"+host || !strings.HasPrefix(p.WebURL, "https://"+host+"/web/connect?token=") {
		t.Fatalf("base_url %q / web_url %q must stay https", p.BaseURL, p.WebURL)
	}
	if q := linkQuery(t, p.WebURL); q.Get("home") != want.Home {
		t.Fatalf("web_url home = %q", q.Get("home"))
	}
	if q := linkQuery(t, p.URI); q.Get("server") != "https://"+host || q.Get("home") != want.Home || q.Get("sid") != "srv-test-id" {
		t.Fatalf("uri params = %v", q)
	}

	status, body = overTLS(t, e, "POST", "/api/v1/auth/exchange", "",
		`{"pairing_token":"`+p.PairingToken+`","device_name":"phone"}`, host)
	var ex struct {
		Token     string     `json:"token"`
		Addresses *Addresses `json:"addresses"`
	}
	_ = json.Unmarshal([]byte(body), &ex)
	if status != http.StatusOK || ex.Addresses == nil || *ex.Addresses != want {
		t.Fatalf("exchange = %d %s", status, body)
	}

	if status, body = overTLS(t, e, "GET", "/api/v1/addresses", ex.Token, "", host); status != http.StatusOK ||
		body != `{"home":"http://192.168.1.205:8080"}`+"\n" {
		t.Fatalf("GET /addresses = %d %q", status, body)
	}
	// https on its default port: the http home keeps 443, the port that answers it.
	if _, body = overTLS(t, e, "GET", "/api/v1/addresses", ex.Token, "", "192.168.1.205"); body != `{"home":"http://192.168.1.205:443"}`+"\n" {
		t.Fatalf("GET /addresses on 443 = %q", body)
	}
	// A public request over https derives no home.
	if _, body = overTLS(t, e, "GET", "/api/v1/addresses", ex.Token, "", "books.example.com"); body != "{}\n" {
		t.Fatalf("public GET /addresses = %q, want {}", body)
	}
}

// Without home HTTP (switched off, or another TLS mode) the derived home keeps
// the request's scheme; a configured lan_url is always used as it is.
func TestHomeHTTPAddressesOff(t *testing.T) {
	t.Parallel()
	const host = "192.168.1.205:8080"
	cases := []struct {
		name      string
		configure func(*config.Config)
		wantHome  string
	}{
		{"home_http off", func(c *config.Config) { c.TLS.HomeHTTP = false }, "https://" + host},
		{"autocert", func(c *config.Config) { c.TLS.Mode, c.TLS.Hosts = config.TLSAutocert, []string{"books.example.com"} },
			"https://" + host},
		{"a configured lan_url", func(c *config.Config) { c.LANURL = "https://nas.local:8080" }, "https://nas.local:8080"},
	}
	for _, tc := range cases {
		e := newTestEnvWith(t, tc.configure)
		_, member := opsTokens(t, e)
		_, body := overTLS(t, e, "GET", "/api/v1/addresses", member, "", host)
		if body != `{"home":"`+tc.wantHome+`"}`+"\n" {
			t.Errorf("%s: GET /addresses = %q, want home %q", tc.name, body, tc.wantHome)
		}
	}
}

// A server without an id (a bare config the launcher never touched) puts no
// sid= on its links; the payload's server_id is then "".
func TestPairingWithoutServerID(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	_, body := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`)
	var p pairingWire
	_ = json.Unmarshal([]byte(body), &p)
	if !strings.Contains(body, `"server_id":""`) || strings.Contains(p.WebURL, "sid=") || strings.Contains(p.URI, "sid=") {
		t.Fatalf("no server id: %s", body)
	}
}

// A pairing asked for over plain HTTP on a server that serves home HTTP keeps
// https links: the base is the https one on the same port (80 for a Host without
// one), and only home is http. With home HTTP off the base follows the request.
func TestHomeHTTPPairingOverPlainHTTP(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t) // plain HTTP; the default config serves home HTTP
	for host, wantBase := range map[string]string{
		"192.168.1.205:8080": "https://192.168.1.205:8080",
		"192.168.1.205":      "https://192.168.1.205:80",
	} {
		resp, body := e.doHeaders(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`, map[string]string{"Host": host})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("redeem = %d %s", resp.StatusCode, body)
		}
		var p pairingWire
		_ = json.Unmarshal([]byte(body), &p)
		if p.BaseURL != wantBase || !strings.HasPrefix(p.WebURL, wantBase+"/web/connect?") ||
			linkQuery(t, p.URI).Get("server") != wantBase {
			t.Fatalf("%s: base_url %q web_url %q, want %s", host, p.BaseURL, p.WebURL, wantBase)
		}
		if p.Addresses == nil || p.Addresses.Home != "http://"+host {
			t.Fatalf("%s: addresses = %+v, want the http home", host, p.Addresses)
		}
	}

	off := newTestEnvWith(t, func(c *config.Config) { c.TLS.HomeHTTP = false })
	resp, body := off.doHeaders(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+off.authCode+`"}`, map[string]string{"Host": "192.168.1.205:8080"})
	var p pairingWire
	_ = json.Unmarshal([]byte(body), &p)
	if resp.StatusCode != http.StatusOK || p.BaseURL != "http://192.168.1.205:8080" {
		t.Fatalf("home HTTP off: base_url = %q (%d)", p.BaseURL, resp.StatusCode)
	}
}
