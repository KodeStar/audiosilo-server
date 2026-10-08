package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

type pairingWire struct {
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
	e := newTestEnvWith(t, func(c *config.Config) {
		c.PublicURL = "https://books.example.com"
		c.LANURL = "http://192.168.1.20:8080"
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
	if q.Get("token") != p.PairingToken || q.Get("home") != want.Home || q.Get("away") != want.Away {
		t.Fatalf("web_url params = %v", q)
	}

	if !strings.HasPrefix(p.URI, "audiosilo://connect?server=") {
		t.Fatalf("uri %q must keep its server= first", p.URI)
	}
	q = linkQuery(t, p.URI)
	if q.Get("server") != "https://books.example.com" || q.Get("token") != p.PairingToken ||
		q.Get("home") != want.Home || q.Get("away") != want.Away {
		t.Fatalf("uri params = %v", q)
	}
}

// With no lan_url, a request that arrived on a home-network address teaches the
// device that address as home; with no public_url there is no away.
func TestPairingAddressesDerivedFromLANRequest(t *testing.T) {
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
	e := newTestEnv(t)
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
		if q := linkQuery(t, p.WebURL); len(q) != 1 || q.Get("token") != p.PairingToken {
			t.Fatalf("%q: web_url %q must carry only token", host, p.WebURL)
		}
		if q := linkQuery(t, p.URI); len(q) != 2 {
			t.Fatalf("%q: uri %q must carry only server and token", host, p.URI)
		}
	}
}

// /auth/pair (an existing session adding a device) carries them too.
func TestAuthPairCarriesAddresses(t *testing.T) {
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
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	var info struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil || !info.Capabilities["addresses"] {
		t.Fatalf("capabilities.addresses must be true: %s", body)
	}
}
