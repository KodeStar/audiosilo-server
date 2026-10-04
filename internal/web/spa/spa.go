// Package spa serves a static single-page app from an fs.FS under a URL prefix.
// Both browser apps the server ships use it: the admin console at /admin (embedded,
// package adminui) and the web player at /web (web_dir on disk, or embedded with
// -tags embedplayer). One handler means one set of rules for caching, MIME types,
// deep links and missing files, instead of two that drift apart.
//
// Routing, for a request path relative to the prefix:
//
//	an existing file           served; "<p>.html" and "<p>/index.html" are tried too
//	                           (Expo exports one HTML file per route)
//	missing, under AssetDirs   404: a fingerprinted bundle that isn't there must fail loudly
//	missing, a top-level name  404 when it has an extension other than .html (favicon.ico)
//	  with an extension
//	anything else              index.html, so client-side routes deep-link (a deeper
//	                           dotted segment such as /library/v1.2 is a route, not a file)
//
// Caching: files under AssetDirs are fingerprinted and cached for a year; HTML and
// every other file revalidate on each request (no-cache), so a new release is
// picked up at once. Every file carries a strong ETag, so a revalidation is a 304,
// and text goes out gzipped to clients that accept it (see Files). Every response
// carries nosniff; HTML gets DocumentCSP and other files FileCSP.
package spa

import (
	"bytes"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// Config describes one app mount.
type Config struct {
	// FS holds the build, with index.html at its root.
	FS fs.FS
	// Prefix is the URL path the app is mounted under, without a trailing slash
	// ("/admin", "/web").
	Prefix string
	// AssetDirs are the top-level directories of fingerprinted build output
	// ("assets", "_expo"): cached immutably, and a missing file there is a 404.
	AssetDirs []string
	// DocumentCSP returns the Content-Security-Policy for an HTML document, given
	// its bytes (the player hashes its inline scripts; the console's is fixed).
	DocumentCSP func(html []byte) string
	// FileCSP is sent with every non-HTML file; empty sends none.
	FileCSP string
}

const immutable = "public, max-age=31536000, immutable"

// Go's mime table falls back to the OS registry, which on some Windows hosts maps
// .js to text/plain - and a module script served as text/plain never runs. Pin
// the types the server's static files use. mime is process-wide, so this also
// covers the classic /assets/ files of the connect and setup pages.
func init() {
	for ext, typ := range map[string]string{
		".js":          "text/javascript; charset=utf-8",
		".mjs":         "text/javascript; charset=utf-8",
		".css":         "text/css; charset=utf-8",
		".html":        "text/html; charset=utf-8",
		".json":        "application/json",
		".svg":         "image/svg+xml",
		".woff":        "font/woff",
		".woff2":       "font/woff2",
		".webmanifest": "application/manifest+json",
	} {
		if err := mime.AddExtensionType(ext, typ); err != nil {
			panic(err) // only fails on a malformed literal above
		}
	}
}

// ContentType returns the Content-Type to serve name with.
func ContentType(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// IsFile reports whether name is a regular file in fsys (and a valid fs path).
func IsFile(fsys fs.FS, name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// Handler serves the app described by c.
func Handler(c Config) http.Handler {
	files := NewFiles()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rel := strings.Trim(strings.TrimPrefix(r.URL.Path, c.Prefix), "/")
		name, ok := resolve(c.FS, rel)
		switch {
		case ok:
		case c.isAsset(rel):
			http.NotFound(w, r)
			return
		default:
			name = "index.html" // a client-routed deep link: boot the SPA
		}
		c.serve(w, r, files, name, rel)
	})
}

// resolve maps a request path to a file, trying the exact path, then
// "<p>.html", then "<p>/index.html".
func resolve(fsys fs.FS, p string) (string, bool) {
	if p == "" {
		p = "index.html"
	}
	for _, cand := range []string{p, p + ".html", p + "/index.html"} {
		if IsFile(fsys, cand) {
			return cand, true
		}
	}
	return "", false
}

// inAssetDir reports whether rel is fingerprinted build output.
func (c Config) inAssetDir(rel string) bool {
	for _, dir := range c.AssetDirs {
		if strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// isAsset reports whether a missing path should 404 rather than boot the SPA.
func (c Config) isAsset(rel string) bool {
	if c.inAssetDir(rel) {
		return true
	}
	if strings.Contains(rel, "/") {
		return false
	}
	ext := path.Ext(rel)
	return ext != "" && !strings.EqualFold(ext, ".html")
}

func (c Config) serve(w http.ResponseWriter, r *http.Request, files *Files, name, rel string) {
	info, content, done, ok := openFile(c.FS, name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	defer done()

	h := w.Header()
	h.Set("Content-Type", ContentType(name))
	h.Set("Cache-Control", "no-cache")
	if strings.HasSuffix(name, ".html") {
		// The document's bytes feed its CSP (the player hashes inline scripts);
		// both are kept per version of the file.
		data, csp, err := files.Document(name, info, content, c.DocumentCSP)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h.Set("Content-Security-Policy", csp)
		content = bytes.NewReader(data)
	} else {
		if c.FileCSP != "" {
			h.Set("Content-Security-Policy", c.FileCSP)
		}
		if c.inAssetDir(rel) {
			h.Set("Cache-Control", immutable)
		}
	}
	files.Serve(w, r, name, info, content)
}
