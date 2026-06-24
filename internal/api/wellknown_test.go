package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/config"
)

// TestWellKnownUnsetReturns404: with no AppLinks configured (the secure default),
// both association endpoints 404 so clients fall back to the embedded web player.
func TestWellKnownUnsetReturns404(t *testing.T) {
	e := newTestEnv(t)
	if resp, body := e.do(t, "GET", "/.well-known/apple-app-site-association", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("apple-app-site-association unset = %d %s, want 404", resp.StatusCode, body)
	}
	if resp, body := e.do(t, "GET", "/.well-known/assetlinks.json", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("assetlinks unset = %d %s, want 404", resp.StatusCode, body)
	}
}

// TestAppleAppSiteAssociationConfigured: with apple_app_ids set, the file is
// served with the appIDs and the path components clients deep-link against.
func TestAppleAppSiteAssociationConfigured(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) {
		c.AppLinks.AppleAppIDs = []string{"ABCDE12345.com.anonymous.audiosilo"}
	})
	resp, body := e.do(t, "GET", "/.well-known/apple-app-site-association", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("apple-app-site-association = %d %s, want 200", resp.StatusCode, body)
	}
	var out struct {
		AppLinks struct {
			Apps    []string `json:"apps"`
			Details []struct {
				AppIDs     []string            `json:"appIDs"`
				Components []map[string]string `json:"components"`
			} `json:"details"`
		} `json:"applinks"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out.AppLinks.Details) != 1 {
		t.Fatalf("expected one details entry: %s", body)
	}
	d := out.AppLinks.Details[0]
	if len(d.AppIDs) != 1 || d.AppIDs[0] != "ABCDE12345.com.anonymous.audiosilo" {
		t.Fatalf("appIDs not echoed: %s", body)
	}
	// Components carry the deep-linkable paths (one "/" key each).
	if len(d.Components) != len(appLinkPaths) {
		t.Fatalf("expected %d components, got %d (%s)", len(appLinkPaths), len(d.Components), body)
	}
	got := map[string]bool{}
	for _, c := range d.Components {
		got[c["/"]] = true
	}
	for _, want := range appLinkPaths {
		if !got[want] {
			t.Fatalf("component %q missing: %s", want, body)
		}
	}
}

// TestAssetLinksConfigured: with android_package + android_sha256 set, the file
// is served with the package name and cert fingerprints Android verifies.
func TestAssetLinksConfigured(t *testing.T) {
	e := newTestEnvWith(t, func(c *config.Config) {
		c.AppLinks.AndroidPackage = "com.anonymous.audiosilo"
		c.AppLinks.AndroidSHA256 = []string{"AA:BB:CC:DD"}
	})
	resp, body := e.do(t, "GET", "/.well-known/assetlinks.json", "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("assetlinks = %d %s, want 200", resp.StatusCode, body)
	}
	var out []struct {
		Relation []string `json:"relation"`
		Target   struct {
			Namespace   string   `json:"namespace"`
			PackageName string   `json:"package_name"`
			SHA256      []string `json:"sha256_cert_fingerprints"`
		} `json:"target"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(out) != 1 {
		t.Fatalf("expected one statement: %s", body)
	}
	tgt := out[0].Target
	if tgt.Namespace != "android_app" {
		t.Fatalf("namespace = %q, want android_app (%s)", tgt.Namespace, body)
	}
	if tgt.PackageName != "com.anonymous.audiosilo" {
		t.Fatalf("package_name = %q (%s)", tgt.PackageName, body)
	}
	if len(tgt.SHA256) != 1 || tgt.SHA256[0] != "AA:BB:CC:DD" {
		t.Fatalf("sha256_cert_fingerprints not echoed: %s", body)
	}

	// Android-only config does NOT enable the Apple endpoint.
	if resp, _ := e.do(t, "GET", "/.well-known/apple-app-site-association", "", ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("apple endpoint should stay 404 with only Android config, got %d", resp.StatusCode)
	}
}
