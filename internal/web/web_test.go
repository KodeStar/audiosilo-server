package web

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile is a tiny helper that creates name (with parents) under dir.
func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakePlayer lays out a minimal Expo-style export in a temp dir.
func fakePlayer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	html := `<html><head><script type="module">console.log(1)</script>` +
		`<script src="/web/_expo/entry.js" defer></script></head><body>app</body></html>`
	writeFile(t, dir, "index.html", html)
	writeFile(t, dir, "connect/index.html", html)
	writeFile(t, dir, "_expo/entry.js", "console.log('entry')")
	return dir
}

func TestHasPlayer(t *testing.T) {
	if HasPlayer("") {
		t.Error("empty web_dir should report no player")
	}
	if HasPlayer(t.TempDir()) {
		t.Error("empty dir should report no player")
	}
	if !HasPlayer(fakePlayer(t)) {
		t.Error("dir with index.html should report a player")
	}
}

// TestHTMLCSP checks inline <script> tags are hashed (no 'unsafe-inline' needed)
// and external scripts are excluded (covered by 'self').
func TestHTMLCSP(t *testing.T) {
	html := []byte(`<script type="module">console.log(1)</script><script src="/x.js"></script>`)
	csp := htmlCSP(html)

	sum := sha256.Sum256([]byte("console.log(1)"))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(csp, want) {
		t.Errorf("CSP missing inline-script hash %s:\n%s", want, csp)
	}
	if n := strings.Count(csp, "sha256-"); n != 1 {
		t.Errorf("want one hash (external excluded), got %d:\n%s", n, csp)
	}
	if !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP should allow inline styles:\n%s", csp)
	}
}

// TestHTMLCSPScriptSrcExcludesUnsafeInline is the security-load-bearing DENIED
// direction: hashing inline scripts is pointless if 'unsafe-inline' is also
// present (a single 'unsafe-inline' makes the browser ignore every hash), so the
// script-src directive must NOT carry 'unsafe-inline'. The style-src directive
// legitimately does, so this asserts on the script-src segment specifically.
func TestHTMLCSPScriptSrcExcludesUnsafeInline(t *testing.T) {
	html := []byte(`<script type="module">console.log(1)</script><script src="/x.js"></script>`)
	csp := htmlCSP(html)

	// Directives are joined with "; "; pull out the script-src one.
	var scriptSrc string
	for _, dir := range strings.Split(csp, "; ") {
		if strings.HasPrefix(dir, "script-src ") {
			scriptSrc = dir
			break
		}
	}
	if scriptSrc == "" {
		t.Fatalf("CSP has no script-src directive:\n%s", csp)
	}
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src must not allow 'unsafe-inline' (it nullifies the inline-script hashes): %q", scriptSrc)
	}
}

// TestI18nAssets checks the baked-in admin/connect i18n is served under the strict
// same-origin CSP: the engine + dictionary load as external assets, the admin/connect
// pages reference them, and those pages stay inline-script-free (an inline <script>
// would silently break under script-src 'self', which carries no hash for them).
func TestI18nAssets(t *testing.T) {
	mux := http.NewServeMux()
	if err := Register(mux, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// The engine and dictionary are served as static assets under the strict CSP.
	for _, p := range []string{"/assets/i18n.js", "/assets/i18n-dict.js"} {
		resp, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, resp.StatusCode)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); csp != contentSecurityPolicy {
			t.Errorf("GET %s CSP = %q, want the strict same-origin policy", p, csp)
		}
		if len(body) == 0 {
			t.Errorf("GET %s served an empty body", p)
		}
	}

	// The dictionary must define every supported language.
	resp, err := c.Get(ts.URL + "/assets/i18n-dict.js")
	if err != nil {
		t.Fatalf("GET dict: %v", err)
	}
	dict, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, lang := range []string{"en", "es", "fr", "de", "pt", "it"} {
		if !strings.Contains(string(dict), lang+": {") {
			t.Errorf("i18n-dict.js is missing the %q language block", lang)
		}
	}

	// The admin + connect pages keep the strict CSP, load the i18n scripts, and
	// carry no inline <script> (every <script> must be external 'self').
	for _, p := range []string{"/admin", "/"} {
		resp, err := c.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if csp := resp.Header.Get("Content-Security-Policy"); csp != contentSecurityPolicy {
			t.Errorf("GET %s CSP = %q, want the strict same-origin policy", p, csp)
		}
		html := string(body)
		if !strings.Contains(html, "/assets/i18n.js") || !strings.Contains(html, "/assets/i18n-dict.js") {
			t.Errorf("GET %s does not load the i18n scripts", p)
		}
		for _, m := range inlineScriptRE.FindAllStringSubmatch(html, -1) {
			if !strings.Contains(strings.ToLower(m[1]), "src=") {
				t.Errorf("GET %s has an inline <script> that breaks the strict CSP: %.40q", p, m[2])
			}
		}
	}
}

// TestAdminPWA checks the admin console is installable: the service worker is
// served from the site root (so its scope covers /admin) with a JS content type,
// the web manifest is served with the correct content type and names the app, and
// the admin page links the manifest. The strict CSP must permit both (manifest-src
// + worker-src 'self').
func TestAdminPWA(t *testing.T) {
	mux := http.NewServeMux()
	if err := Register(mux, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// The service worker is served at the ROOT (not /assets/) so scope = "/".
	resp, err := c.Get(ts.URL + "/sw.js")
	if err != nil {
		t.Fatalf("GET /sw.js: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /sw.js = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("GET /sw.js Content-Type = %q, want a javascript type", ct)
	}
	if len(body) == 0 {
		t.Error("GET /sw.js served an empty body")
	}

	// The web manifest gets the explicit application/manifest+json type and names
	// the app (Go's mime table doesn't know .webmanifest, so this must be set).
	resp, err = c.Get(ts.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatalf("GET /manifest.webmanifest: %v", err)
	}
	mani, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /manifest.webmanifest = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/manifest+json" {
		t.Errorf("manifest Content-Type = %q, want application/manifest+json", ct)
	}
	if !strings.Contains(string(mani), "AudioSilo Admin") {
		t.Errorf("manifest does not name the admin app:\n%s", mani)
	}

	// The strict CSP must allow the SW + manifest to load.
	for _, want := range []string{"worker-src 'self'", "manifest-src 'self'"} {
		if !strings.Contains(contentSecurityPolicy, want) {
			t.Errorf("contentSecurityPolicy missing %q for the admin PWA:\n%s", want, contentSecurityPolicy)
		}
	}

	// The admin page links the manifest so the browser can offer "Install".
	resp, err = c.Get(ts.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin: %v", err)
	}
	admin, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(admin), `rel="manifest"`) {
		t.Error("/admin does not link the web manifest")
	}
}

// TestPlayerServing exercises /web routing: per-route HTML, the SPA fallback for
// client-routed deep links, 404 for missing assets, and a disabled /web when
// web_dir is empty.
func TestPlayerServing(t *testing.T) {
	mux := http.NewServeMux()
	if err := Register(mux, fakePlayer(t)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	cases := []struct {
		path string
		want int
	}{
		{"/web/", http.StatusOK},
		{"/web", http.StatusMovedPermanently},               // subtree redirect
		{"/web/connect", http.StatusOK},                     // per-route HTML
		{"/web/library/1/deep/client/route", http.StatusOK}, // dynamic route → SPA fallback
		{"/web/_expo/missing.js", http.StatusNotFound},
		{"/connect", http.StatusOK}, // server connect page
		{"/", http.StatusOK},
	}
	for _, tc := range cases {
		resp, err := c.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, tc.want)
		}
	}

	// The player index carries the scoped CSP (with a hash), not the strict one.
	resp, err := c.Get(ts.URL + "/web/")
	if err != nil {
		t.Fatalf("GET /web/: %v", err)
	}
	resp.Body.Close()
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "media-src") || !strings.Contains(csp, "sha256-") {
		t.Errorf("/web/ missing scoped CSP with hash, got %q", csp)
	}

	// With no web_dir, /web is not mounted (falls through to 404).
	mux2 := http.NewServeMux()
	if err := Register(mux2, ""); err != nil {
		t.Fatalf("Register(empty): %v", err)
	}
	ts2 := httptest.NewServer(mux2)
	defer ts2.Close()
	if resp, _ := ts2.Client().Get(ts2.URL + "/web/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/web/ with no web_dir = %d, want 404", resp.StatusCode)
	}
}
