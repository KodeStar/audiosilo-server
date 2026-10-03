// Package adminui embeds and serves the admin console single-page app: the
// React + Vite project in admin-ui/ at the repo root, built into dist/ here.
//
// The build output is not committed (only dist/.gitkeep is). CI, the Dockerfile
// and GoReleaser build it before compiling; a source build without it still
// compiles and serves a short "console not built" page at /admin instead.
//
// Serving rules (package spa, shared with the web player): /admin/assets/... are
// fingerprinted and cached immutably (404 when missing), other top-level files
// (theme-init.js) revalidate, and everything else is index.html so client-side
// routes deep-link. Every response carries the caller-supplied CSP and nosniff.
// The console needs no inline script or style (admin-ui/scripts/check-csp.mjs
// enforces that at build time and adminui_test.go over the embedded build).
package adminui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/kodestar/audiosilo-server/internal/web/spa"
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

// built reports whether fsys holds a console build (an index.html), like
// web.HasPlayer does for the player.
func built(fsys fs.FS) bool { return spa.IsFile(fsys, "index.html") }

// Handler serves the console in fsys under /admin with the given CSP. When fsys
// holds no build it serves the "not built" page for every request.
func Handler(fsys fs.FS, csp string) http.Handler {
	if !built(fsys) {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Security-Policy", csp)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			notBuilt(w)
		})
	}
	return spa.Handler(spa.Config{
		FS:          fsys,
		Prefix:      "/admin",
		AssetDirs:   []string{"assets"},
		DocumentCSP: func([]byte) string { return csp },
		FileCSP:     csp,
	})
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
<pre>scripts/build-admin.sh
go build ./cmd/audiosilo</pre>
<p>Release binaries and the Docker image include it.</p>
</body>
</html>
`

func notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(notBuiltPage))
}
