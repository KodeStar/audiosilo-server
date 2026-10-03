package adminui

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

const testCSP = "default-src 'self'; script-src 'self'; style-src 'self'"

func fakeBuild() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           {Data: []byte(`<!doctype html><script src="/admin/theme-init.js"></script><div id="root"></div>`)},
		"theme-init.js":        {Data: []byte("/* theme */")},
		"assets/index-abc.js":  {Data: []byte("console.log(1)")},
		"assets/index-abc.css": {Data: []byte("body{}")},
		"assets/font.woff2":    {Data: []byte("wOF2")},
	}
}

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerRouting(t *testing.T) {
	h := Handler(fakeBuild(), testCSP)
	cases := []struct {
		path, wantBody, wantType, wantCache string
		wantStatus                          int
	}{
		{"/admin", "<div id=\"root\">", "text/html; charset=utf-8", "no-cache", 200},
		{"/admin/", "<div id=\"root\">", "text/html; charset=utf-8", "no-cache", 200},
		{"/admin/library/authors", "<div id=\"root\">", "text/html; charset=utf-8", "no-cache", 200},
		// A dotted segment deeper than the root is a client route, not a file.
		{"/admin/library/v1.2", "<div id=\"root\">", "text/html; charset=utf-8", "no-cache", 200},
		{"/admin/assets/index-abc.js", "console.log(1)", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", 200},
		{"/admin/assets/index-abc.css", "body{}", "text/css; charset=utf-8", "public, max-age=31536000, immutable", 200},
		{"/admin/assets/font.woff2", "wOF2", "font/woff2", "public, max-age=31536000, immutable", 200},
		{"/admin/theme-init.js", "/* theme */", "text/javascript; charset=utf-8", "no-cache", 200},
	}
	for _, tc := range cases {
		rec := get(t, h, http.MethodGet, tc.path)
		if rec.Code != tc.wantStatus {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.wantStatus)
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("GET %s body = %q, want it to contain %q", tc.path, rec.Body.String(), tc.wantBody)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.wantType {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.path, got, tc.wantType)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.wantCache {
			t.Errorf("GET %s Cache-Control = %q, want %q", tc.path, got, tc.wantCache)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != testCSP {
			t.Errorf("GET %s CSP = %q, want the caller's policy", tc.path, got)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s nosniff = %q", tc.path, got)
		}
	}
}

// Missing files 404 rather than booting the SPA (a broken asset reference must
// fail loudly), and paths can't escape the build.
func TestHandlerRejectsMissingAndTraversal(t *testing.T) {
	h := Handler(fakeBuild(), testCSP)
	for _, p := range []string{
		"/admin/assets/missing.js",
		"/admin/missing.png",
		"/admin/assets/../../web.go",
		"/admin/assets/%2e%2e/index.html",
	} {
		if rec := get(t, h, http.MethodGet, p); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

func TestHandlerHead(t *testing.T) {
	rec := get(t, Handler(fakeBuild(), testCSP), http.MethodHead, "/admin/assets/index-abc.js")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("HEAD = %d with %d body bytes, want 200 and none", rec.Code, rec.Body.Len())
	}
}

func TestHandlerNotBuilt(t *testing.T) {
	empty := fstest.MapFS{".gitkeep": {Data: nil}}
	if built(empty) {
		t.Fatal("Built reported a build for an empty dist")
	}
	for _, p := range []string{"/admin", "/admin/assets/index-abc.js"} {
		rec := get(t, Handler(empty, testCSP), http.MethodGet, p)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", p, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "scripts/build-admin.sh") {
			t.Errorf("GET %s: the not-built page should say how to build the console", p)
		}
		if rec.Header().Get("Content-Security-Policy") != testCSP {
			t.Errorf("GET %s: the not-built page must carry the CSP", p)
		}
	}
	if v := cspViolations(notBuiltPage); len(v) > 0 {
		t.Errorf("not-built page breaks the CSP: %v", v)
	}
}

var (
	scriptRE    = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	tagRE       = regexp.MustCompile(`(?i)<[a-z][^>]*>`)
	styleAttrRE = regexp.MustCompile(`(?i)\sstyle\s*=`)
	handlerRE   = regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	assetRefRE  = regexp.MustCompile(`(?:src|href)="/admin/([^"]+)"`)
)

// cspViolations mirrors admin-ui/scripts/check-csp.mjs: what the console's
// `script-src 'self'; style-src 'self'` policy (no nonce) would block.
func cspViolations(html string) []string {
	var out []string
	for _, m := range scriptRE.FindAllStringSubmatch(html, -1) {
		if !strings.Contains(strings.ToLower(m[1]), "src=") {
			out = append(out, "inline <script>")
		}
	}
	if strings.Contains(strings.ToLower(html), "<style") {
		out = append(out, "<style> element")
	}
	for _, tag := range tagRE.FindAllString(html, -1) {
		if styleAttrRE.MatchString(tag) {
			out = append(out, "style attribute: "+tag)
		}
		if handlerRE.MatchString(tag) {
			out = append(out, "inline event handler: "+tag)
		}
	}
	return out
}

func TestCSPViolationsDetector(t *testing.T) {
	bad := `<script>x()</script><style>a{}</style><p style="x"></p><img onerror="y()">`
	if got := cspViolations(bad); len(got) != 4 {
		t.Fatalf("detector found %v, want 4 violations", got)
	}
	if got := cspViolations(`<script type="module" src="/admin/assets/a.js"></script>`); len(got) != 0 {
		t.Fatalf("external script flagged: %v", got)
	}
}

// TestEmbeddedBuild checks the console actually compiled into this binary (CI
// builds it before `go test`; a source build without it skips): index.html
// needs nothing the CSP blocks, and every /admin/ file it references exists.
func TestEmbeddedBuild(t *testing.T) {
	fsys := FS()
	if !built(fsys) {
		t.Skip("admin console not built (run npm --prefix admin-ui run build)")
	}
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(data)
	if v := cspViolations(html); len(v) > 0 {
		t.Fatalf("embedded index.html breaks the CSP: %v", v)
	}
	refs := assetRefRE.FindAllStringSubmatch(html, -1)
	if len(refs) == 0 {
		t.Fatal("index.html references no /admin/ assets; is the Vite base still /admin/?")
	}
	h := Handler(fsys, testCSP)
	for _, m := range refs {
		rec := get(t, h, http.MethodGet, "/admin/"+m[1])
		body, _ := io.ReadAll(rec.Result().Body)
		if rec.Code != http.StatusOK || len(body) == 0 {
			t.Errorf("index.html references /admin/%s, which serves %d", m[1], rec.Code)
		}
	}
}
