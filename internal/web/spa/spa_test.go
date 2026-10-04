package spa

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
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
		rec := req(h, http.MethodGet, tc.path)
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
	doc := req(h, http.MethodGet, "/web/")
	if got, want := doc.Header().Get("Content-Security-Policy"), docCSP([]byte("<p>root</p>")); got != want {
		t.Errorf("document CSP = %q, want %q (computed from its bytes)", got, want)
	}
	if ct := doc.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("document Content-Type = %q", ct)
	}
	file := req(h, http.MethodGet, "/web/assets/app-1.js")
	if got := file.Header().Get("Content-Security-Policy"); got != fileCSP {
		t.Errorf("file CSP = %q, want %q", got, fileCSP)
	}
	if ct := file.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("file Content-Type = %q", ct)
	}

	noFileCSP := Handler(Config{FS: fstest.MapFS{"index.html": {}, "a.js": {}}, Prefix: "/x", DocumentCSP: docCSP})
	if got := req(noFileCSP, http.MethodGet, "/x/a.js").Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("an empty FileCSP should send none, got %q", got)
	}
}

func TestHead(t *testing.T) {
	rec := req(app(), http.MethodHead, "/web/assets/app-1.js")
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

var (
	bigJS   = strings.Repeat("console.log('a line of the bundle');\n", 200)
	bigHTML = "<html><body>" + strings.Repeat("<p>root</p>", 200) + "</body></html>"
	bigFont = strings.Repeat("wOF2", 1000) // compresses well, but fonts never are
)

func bigApp() http.Handler {
	return Handler(Config{
		FS: fstest.MapFS{
			"index.html":        {Data: []byte(bigHTML)},
			"assets/app-1.js":   {Data: []byte(bigJS)},
			"assets/font.woff2": {Data: []byte(bigFont)},
		},
		Prefix:      "/web",
		AssetDirs:   []string{"assets"},
		DocumentCSP: docCSP,
		FileCSP:     fileCSP,
	})
}

// req serves one request with the given headers (name, value pairs).
func req(h http.Handler, method, target string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestGzipNegotiated(t *testing.T) {
	h := bigApp()
	for _, ae := range []string{"gzip", "gzip, deflate, br", "br;q=1, gzip;q=0.5", "*", "x-gzip"} {
		rec := req(h, http.MethodGet, "/web/assets/app-1.js", "Accept-Encoding", ae)
		if rec.Code != 200 || rec.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("Accept-Encoding %q: %d, Content-Encoding %q, want gzip", ae, rec.Code, rec.Header().Get("Content-Encoding"))
		}
		if got := gunzip(t, rec.Body.Bytes()); got != bigJS {
			t.Fatalf("Accept-Encoding %q: gzip body doesn't decode to the file", ae)
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(rec.Body.Len()) {
			t.Errorf("Accept-Encoding %q: Content-Length %s, body %d", ae, cl, rec.Body.Len())
		}
		if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary = %q", ae, v)
		}
		if et := rec.Header().Get("ETag"); !strings.HasSuffix(et, `-gz"`) {
			t.Errorf("Accept-Encoding %q: ETag %q is not the gzip form's", ae, et)
		}
		// The headers the handler sets are kept.
		if rec.Header().Get("Cache-Control") != immutable || rec.Header().Get("Content-Security-Policy") != fileCSP ||
			rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
			rec.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
			t.Errorf("Accept-Encoding %q: headers changed: %v", ae, rec.Header())
		}
	}

	// Not accepted: the identity bytes, under the identity ETag, still with Vary.
	for _, ae := range []string{"", "identity", "gzip;q=0", "br", "*;q=0", "*, gzip;q=0"} {
		rec := req(h, http.MethodGet, "/web/assets/app-1.js", "Accept-Encoding", ae)
		if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != bigJS {
			t.Fatalf("Accept-Encoding %q: got Content-Encoding %q", ae, rec.Header().Get("Content-Encoding"))
		}
		if cl := rec.Header().Get("Content-Length"); cl != strconv.Itoa(len(bigJS)) {
			t.Errorf("Accept-Encoding %q: Content-Length %s, want %d", ae, cl, len(bigJS))
		}
		if v := rec.Header().Get("Vary"); v != "Accept-Encoding" {
			t.Errorf("Accept-Encoding %q: Vary = %q", ae, v)
		}
		if et := rec.Header().Get("ETag"); et == "" || strings.HasSuffix(et, `-gz"`) {
			t.Errorf("Accept-Encoding %q: ETag = %q, want the identity form's", ae, et)
		}
	}

	// HEAD: the gzip form's length, no body.
	rec := req(h, http.MethodHead, "/web/assets/app-1.js", "Accept-Encoding", "gzip")
	get := req(h, http.MethodGet, "/web/assets/app-1.js", "Accept-Encoding", "gzip")
	if rec.Code != 200 || rec.Body.Len() != 0 || rec.Header().Get("Content-Encoding") != "gzip" ||
		rec.Header().Get("Content-Length") != strconv.Itoa(get.Body.Len()) {
		t.Errorf("HEAD gzip = %d, %d body bytes, headers %v", rec.Code, rec.Body.Len(), rec.Header())
	}
}

func TestNeverGzipped(t *testing.T) {
	h := bigApp()
	// A font is never compressed, and its answer doesn't vary.
	rec := req(h, http.MethodGet, "/web/assets/font.woff2", "Accept-Encoding", "gzip")
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != bigFont || rec.Header().Get("Vary") != "" {
		t.Errorf("woff2: Content-Encoding %q, Vary %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("woff2: no ETag")
	}
	// A Range request gets identity bytes (a 206 of the file itself).
	rec = req(h, http.MethodGet, "/web/assets/app-1.js", "Accept-Encoding", "gzip", "Range", "bytes=0-9")
	if rec.Code != http.StatusPartialContent || rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != bigJS[:10] {
		t.Errorf("Range: %d, Content-Encoding %q, body %q", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.String())
	}
	// A tiny file that gzip would only grow goes out as it is.
	small := Handler(Config{FS: fstest.MapFS{"index.html": {Data: []byte("<p>x</p>")}}, Prefix: "/x", DocumentCSP: docCSP})
	if rec := req(small, http.MethodGet, "/x/", "Accept-Encoding", "gzip"); rec.Header().Get("Content-Encoding") != "" || rec.Body.String() != "<p>x</p>" {
		t.Errorf("a tiny file was gzipped: %q", rec.Header().Get("Content-Encoding"))
	}
}

// Both an immutable asset and a no-cache document revalidate to a 304 on their
// ETag, in either representation, keeping their caching and CSP headers.
func TestETagNotModified(t *testing.T) {
	h := bigApp()
	for _, target := range []string{"/web/assets/app-1.js", "/web/", "/web/library/deep"} {
		for _, ae := range []string{"gzip", ""} {
			first := req(h, http.MethodGet, target, "Accept-Encoding", ae)
			etag := first.Header().Get("ETag")
			if first.Code != 200 || !strings.HasPrefix(etag, `"`) {
				t.Fatalf("GET %s (%q) = %d, ETag %q", target, ae, first.Code, etag)
			}
			for _, inm := range []string{etag, "W/" + etag, `"other", ` + etag} {
				rec := req(h, http.MethodGet, target, "Accept-Encoding", ae, "If-None-Match", inm)
				if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
					t.Fatalf("GET %s (%q) If-None-Match %s = %d", target, ae, inm, rec.Code)
				}
				if rec.Header().Get("ETag") != etag || rec.Header().Get("Content-Encoding") != "" {
					t.Errorf("GET %s (%q) 304 headers: %v", target, ae, rec.Header())
				}
				for _, k := range []string{"Cache-Control", "Content-Security-Policy", "Vary"} {
					if rec.Header().Get(k) != first.Header().Get(k) {
						t.Errorf("GET %s (%q) 304 %s = %q, want %q", target, ae, k, rec.Header().Get(k), first.Header().Get(k))
					}
				}
			}
			// The other representation's ETag doesn't match this one.
			other := strings.TrimSuffix(etag, `"`) + `-gz"`
			if base, ok := strings.CutSuffix(etag, `-gz"`); ok {
				other = base + `"`
			}
			if rec := req(h, http.MethodGet, target, "Accept-Encoding", ae, "If-None-Match", other); rec.Code != 200 {
				t.Errorf("GET %s (%q) with the other form's ETag = %d, want 200", target, ae, rec.Code)
			}
		}
	}
	// The gzip and identity forms have different ETags.
	gz := req(h, http.MethodGet, "/web/", "Accept-Encoding", "gzip").Header().Get("ETag")
	id := req(h, http.MethodGet, "/web/").Header().Get("ETag")
	if gz == id {
		t.Errorf("gzip and identity share the ETag %s", gz)
	}
}

// A file served from a directory gets a new ETag (and gzip form) when it changes.
func TestDiskFileChanges(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(bigJS)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(bigHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	h := Handler(Config{FS: os.DirFS(dir), Prefix: "/web", DocumentCSP: docCSP})
	before := req(h, http.MethodGet, "/web/app.js", "Accept-Encoding", "gzip")
	if before.Header().Get("Last-Modified") == "" {
		t.Error("a file on disk lost its Last-Modified")
	}
	changed := bigJS + "console.log('new');\n"
	write(changed)
	after := req(h, http.MethodGet, "/web/app.js", "Accept-Encoding", "gzip")
	if after.Header().Get("ETag") == before.Header().Get("ETag") || gunzip(t, after.Body.Bytes()) != changed {
		t.Errorf("the changed file kept its ETag %s or old bytes", after.Header().Get("ETag"))
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"": false, "gzip": true, "GZIP": true, "deflate, gzip;q=1.0": true, "gzip; q=0": false,
		"gzip;q=0.001": true, "*": true, "*;q=0": false, "br, *": true, "gzip;q=0, *": false, "identity": false,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

// A document's CSP (and its bytes) are worked out once per version of the file:
// deep links and revalidations don't read and hash it again, and a replaced
// document is.
func TestDocumentCSPOncePerVersion(t *testing.T) {
	fsys := fstest.MapFS{"index.html": {Data: []byte("<p>one</p>"), ModTime: time.Unix(1, 0)}}
	calls := 0
	h := Handler(Config{FS: fsys, Prefix: "/x", DocumentCSP: func(b []byte) string {
		calls++
		return "csp " + string(b)
	}})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, p := range []string{"/x/", "/x/a/deep/link", "/x/another"} {
		if got := get(p).Header().Get("Content-Security-Policy"); got != "csp <p>one</p>" {
			t.Fatalf("GET %s CSP = %q", p, got)
		}
	}
	if calls != 1 {
		t.Fatalf("CSP worked out %d times for one version, want 1", calls)
	}
	fsys["index.html"] = &fstest.MapFile{Data: []byte("<p>two!</p>"), ModTime: time.Unix(2, 0)}
	if rec := get("/x/"); rec.Header().Get("Content-Security-Policy") != "csp <p>two!</p>" || rec.Body.String() != "<p>two!</p>" {
		t.Fatalf("a replaced document = %q %q", rec.Header().Get("Content-Security-Policy"), rec.Body.String())
	}
	if calls != 2 {
		t.Fatalf("CSP worked out %d times for two versions, want 2", calls)
	}
}
