// Package adminui embeds and serves the admin console single-page app: the
// React + Vite project in admin-ui/ at the repo root, built into dist/ here.
//
// The build output is not committed (only dist/.gitkeep is). CI, the Dockerfile
// and GoReleaser build it before compiling; a source build without it still
// compiles and serves a short "console not built" page instead of the console.
//
// Routing under /admin:
//
//	/admin/assets/...  hashed, immutable build assets (JS, CSS, fonts); 404 if missing
//	/admin/<file.ext>  other top-level files of the build (theme-init.js); 404 if missing
//	everything else    index.html, so client-side routes deep-link
//
// Every response carries the caller-supplied CSP and nosniff. The console needs
// no inline script or style (admin-ui/scripts/check-csp.mjs enforces that at
// build time and adminui_test.go over the embedded build).
package adminui

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded console build (the contents of dist/).
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// fs.Sub only fails on an invalid path literal, which "dist" is not.
		panic(err)
	}
	return sub
}

// Built reports whether fsys holds a console build (an index.html).
func Built(fsys fs.FS) bool {
	info, err := fs.Stat(fsys, "index.html")
	return err == nil && !info.IsDir()
}

// contentTypes pins the MIME type of everything the build emits. Go's mime
// table falls back to the OS registry, which on some Windows hosts maps .js to
// text/plain - and a module script served as text/plain doesn't run.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".map":   "application/json",
}

// ContentType returns the Content-Type to serve name with.
func ContentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if ct, ok := contentTypes[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// Handler serves the console in fsys under /admin with the given CSP. When fsys
// holds no build it serves the "not built" page for every request.
func Handler(fsys fs.FS, csp string) http.Handler {
	built := Built(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !built {
			notBuilt(w)
			return
		}

		rel := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/admin"), "/")
		switch {
		case strings.HasPrefix(rel, "assets/"):
			// Fingerprinted by Vite: a new build changes the name, so cache forever.
			serveFile(w, r, fsys, rel, "public, max-age=31536000, immutable")
		case !strings.Contains(rel, "/") && path.Ext(rel) != "":
			// A non-hashed root file (theme-init.js): revalidate every time. Deeper
			// paths with a dot are client routes and fall through to the SPA.
			serveFile(w, r, fsys, rel, "no-cache")
		default:
			// A client-side route: boot the SPA. no-cache so a new release's
			// index.html (pointing at new hashed assets) is picked up at once.
			serveFile(w, r, fsys, "index.html", "no-cache")
		}
	})
}

func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name, cacheControl string) {
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ContentType(name))
	w.Header().Set("Cache-Control", cacheControl)
	// Embedded files carry no modification time; ServeContent then omits
	// Last-Modified and still handles Range and HEAD.
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// notBuiltPage is what a source build without the console serves at /admin. It
// is self-contained: no inline style or script, so the strict CSP holds.
const notBuiltPage = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>AudioSilo - admin console not built</title></head>
<body>
<h1>The admin console isn't built</h1>
<p>This server was compiled without the admin console. Build it, then rebuild the server:</p>
<pre>npm --prefix admin-ui ci &amp;&amp; npm --prefix admin-ui run build
go build ./cmd/audiosilo</pre>
<p>Release binaries and the Docker image include it. Until then the classic console is at <a href="/admin/classic">/admin/classic</a>.</p>
</body>
</html>
`

func notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(notBuiltPage))
}
