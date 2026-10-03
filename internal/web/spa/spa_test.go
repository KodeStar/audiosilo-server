package spa

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const fileCSP = "default-src 'self'"

// docCSP echoes the document length so a test can tell the policy was computed
// from the served bytes (as the player's per-document hash is).
func docCSP(html []byte) string { return "doc-csp len=" + strconv.Itoa(len(html)) }

func app() http.Handler {
	return Handler(Config{
		FS: fstest.MapFS{
			"index.html":         {Data: []byte("<p>root</p>")},
			"connect/index.html": {Data: []byte("<p>connect</p>")},
			"about.html":         {Data: []byte("<p>about</p>")},
			"favicon.ico":        {Data: []byte("ico")},
			"theme-init.js":      {Data: []byte("theme")},
			"assets/app-1.js":    {Data: []byte("app")},
			"_expo/entry-2.js":   {Data: []byte("entry")},
		},
		Prefix:      "/web",
		AssetDirs:   []string{"assets", "_expo"},
		DocumentCSP: docCSP,
		FileCSP:     fileCSP,
	})
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestRouting(t *testing.T) {
	h := app()
	cases := []struct {
		path, body, cache string
		status            int
	}{
		{"/web", "root", "no-cache", 200},
		{"/web/", "root", "no-cache", 200},
		{"/web/connect", "connect", "no-cache", 200},     // <p>/index.html
		{"/web/about", "about", "no-cache", 200},         // <p>.html
		{"/web/library/1/deep", "root", "no-cache", 200}, // client route
		{"/web/library/v1.2", "root", "no-cache", 200},   // a deeper dotted segment is a route too
		{"/web/theme-init.js", "theme", "no-cache", 200},
		{"/web/favicon.ico", "ico", "no-cache", 200},
		{"/web/assets/app-1.js", "app", immutable, 200},
		{"/web/_expo/entry-2.js", "entry", immutable, 200},
		// Missing files that must fail loudly rather than boot the app.
		{"/web/assets/gone.js", "", "", 404},
		{"/web/_expo/static/gone.js", "", "", 404},
		{"/web/missing.png", "", "", 404},
		// No escaping the build.
		{"/web/assets/../../spa.go", "", "", 404},
	}
	for _, tc := range cases {
		rec := get(h, http.MethodGet, tc.path)
		if rec.Code != tc.status {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.status)
			continue
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s: missing nosniff", tc.path)
		}
		if tc.status != 200 {
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("GET %s body = %q, want %q", tc.path, rec.Body.String(), tc.body)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, got, tc.cache)
		}
	}
}

func TestCSPPerKind(t *testing.T) {
	h := app()
	doc := get(h, http.MethodGet, "/web/")
	if got, want := doc.Header().Get("Content-Security-Policy"), docCSP([]byte("<p>root</p>")); got != want {
		t.Errorf("document CSP = %q, want %q (computed from its bytes)", got, want)
	}
	if ct := doc.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("document Content-Type = %q", ct)
	}
	file := get(h, http.MethodGet, "/web/assets/app-1.js")
	if got := file.Header().Get("Content-Security-Policy"); got != fileCSP {
		t.Errorf("file CSP = %q, want %q", got, fileCSP)
	}
	if ct := file.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("file Content-Type = %q", ct)
	}

	noFileCSP := Handler(Config{FS: fstest.MapFS{"index.html": {}, "a.js": {}}, Prefix: "/x", DocumentCSP: docCSP})
	if got := get(noFileCSP, http.MethodGet, "/x/a.js").Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("an empty FileCSP should send none, got %q", got)
	}
}

func TestHead(t *testing.T) {
	rec := get(app(), http.MethodHead, "/web/assets/app-1.js")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d with %d body bytes, want 200 and none", rec.Code, rec.Body.Len())
	}
}

func TestContentType(t *testing.T) {
	for name, want := range map[string]string{
		"a.js":                 "text/javascript; charset=utf-8",
		"a.MJS":                "text/javascript; charset=utf-8",
		"a.woff2":              "font/woff2",
		"manifest.webmanifest": "application/manifest+json",
		"a.bin":                "application/octet-stream",
	} {
		if got := ContentType(name); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", name, got, want)
		}
	}
}
