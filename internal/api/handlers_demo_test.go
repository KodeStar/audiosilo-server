package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

func TestDemoSessionDisabled(t *testing.T) {
	e := newTestEnv(t)
	if resp, _ := e.do(t, "POST", "/api/v1/demo/session", "", ""); resp.StatusCode != 404 {
		t.Fatalf("expected 404 when demo mode is off, got %d", resp.StatusCode)
	}
}

func TestDemoRootRedirect(t *testing.T) {
	// A web player must be present for the redirect to register.
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newTestEnvWith(t, func(c *config.Config) {
		c.Demo.Enabled = true
		c.Demo.Library = "Demo"
		c.WebDir = webDir
	})

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/web/demo" {
		t.Fatalf("root = %d Location=%q, want 302 /web/demo", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The connect page is left untouched (not redirected).
	if resp, _ := e.do(t, "GET", "/connect", "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("/connect should still serve, got %d", resp.StatusCode)
	}
}

func TestDemoSessionFlow(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := e.cat.CreateLibrary(ctx, catalog.Library{
		Name: "Demo", Root: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	// Enable demo mode on the live config the handler reads.
	e.cfg.Demo.Enabled = true
	e.cfg.Demo.Library = "Demo"

	resp, body := e.do(t, "POST", "/api/v1/demo/session", "", "")
	if resp.StatusCode != 200 {
		t.Fatalf("demo session: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Token string `json:"token"`
		User  struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
		Pairing struct {
			PairingToken string `json:"pairing_token"`
			PNGDataURI   string `json:"qr_png_data_uri"`
		} `json:"pairing"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Token == "" {
		t.Fatalf("no session token: %s", body)
	}
	if !strings.HasPrefix(out.User.Username, "demo_") || out.User.Role != "user" {
		t.Fatalf("unexpected demo user: %+v", out.User)
	}
	if out.Pairing.PairingToken == "" || !strings.HasPrefix(out.Pairing.PNGDataURI, "data:image/png;base64,") {
		t.Fatalf("missing pairing payload/QR: %s", body)
	}

	// The session token authenticates and the demo user sees the granted library.
	if resp, _ := e.do(t, "GET", "/api/v1/me", out.Token, ""); resp.StatusCode != 200 {
		t.Fatalf("me with demo token: %d", resp.StatusCode)
	}
	if _, libs := e.do(t, "GET", "/api/v1/libraries", out.Token, ""); !strings.Contains(libs, "Demo") {
		t.Fatalf("demo user cannot see demo library: %s", libs)
	}

	// The pairing token exchanges for a second (phone) session as the same user.
	_, ex := e.do(t, "POST", "/api/v1/auth/exchange", "",
		`{"pairing_token":"`+out.Pairing.PairingToken+`","device_name":"phone"}`)
	var exr struct {
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(ex), &exr)
	if exr.Token == "" {
		t.Fatalf("pairing exchange failed: %s", ex)
	}
}
