// Package web serves the browser UI baked into the server: the small,
// dependency-free connect and setup pages, the admin console at /admin (package
// adminui), and optionally the web player at /web.
//
// The connect/setup pages are plain HTML/CSS/JS (no build step) embedded in the
// binary; like the console they talk to the JSON API and are static, so the real
// authorization always happens at the API. The web player is a separate project
// (the audiosilo-frontend Expo export) and is NOT vendored here: it is served at
// runtime from a directory (config web_dir / AUDIOSILO_WEB_DIR) that the Docker
// image bakes in. When web_dir is unset or empty, /web is simply not mounted.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/kodestar/audiosilo-server/internal/web/adminui"
	"github.com/kodestar/audiosilo-server/internal/web/spa"
)

//go:embed assets
var assetsFS embed.FS

// contentSecurityPolicy locks the admin console and connect/setup pages down to
// same-origin resources.
// data: is allowed for images so the QR pairing PNG (a data URI) renders.
// manifest-src and worker-src ('self') let the admin console install as a PWA:
// fetch its web manifest and register its same-origin service worker (/sw.js).
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'; " +
	"manifest-src 'self'; worker-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// ContentSecurityPolicy is the strict same-origin CSP applied to the admin console
// and the baked-in connect pages. Exported so the api package can apply the identical policy
// to the first-run setup page it serves (the setup flow lives in api because it
// creates the admin account).
const ContentSecurityPolicy = contentSecurityPolicy

// Asset returns an embedded UI asset by name (e.g. "setup.html"). Used by the api
// package to serve the first-run setup page; callers set the content type + CSP.
func Asset(name string) ([]byte, error) {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return nil, err
	}
	return fs.ReadFile(sub, name)
}

// Register mounts the web UI on mux:
//
//	GET /                 connect page (public)
//	GET /connect[/]       connect page (the copy-invite link target)
//	GET /admin[/...]      admin console (package adminui; the API enforces the admin role)
//	GET /assets/...       static CSS/JS of the connect and setup pages
//	GET /web/...          web player, served from webDir (only if non-empty)
//
// API routes registered on the same mux take precedence because ServeMux prefers
// more specific patterns.
func Register(mux *http.ServeMux, webDir string) error {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return err
	}
	assets := http.StripPrefix("/assets/", http.FileServerFS(sub))
	mux.Handle("GET /assets/", noSniff(assets))
	// Browsers request /favicon.ico at the site root by default; point it at the
	// embedded SVG mark (the HTML pages also link it explicitly via <link rel=icon>).
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/assets/favicon.svg", http.StatusMovedPermanently)
	})
	// The admin console's PWA service worker and web manifest are served from the
	// site root: a service worker can only control pages at or below its own URL,
	// so /sw.js (scope "/") is what lets it control /admin.
	mux.HandleFunc("GET /sw.js", rootAsset(sub, "sw.js", true))
	mux.HandleFunc("GET /manifest.webmanifest", rootAsset(sub, "manifest.webmanifest", false))
	admin := adminui.Handler(adminui.FS(), contentSecurityPolicy)
	mux.Handle("GET /admin", admin)
	mux.Handle("GET /admin/", admin)
	mux.HandleFunc("GET /connect", page(sub, "index.html"))
	mux.HandleFunc("GET /connect/", page(sub, "index.html"))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// "/" is the catch-all; only the exact root serves the connect page.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page(sub, "index.html")(w, r)
	})

	if fsys, ok := playerFS(webDir); ok && spa.IsFile(fsys, "index.html") {
		mux.Handle("GET /web/", spa.Handler(spa.Config{
			FS:          fsys,
			Prefix:      "/web",
			AssetDirs:   []string{"_expo", "assets"},
			DocumentCSP: htmlCSP,
		}))
	}
	return nil
}

// playerFS chooses where to serve the web player from: a player embedded in the
// binary (release builds, -tags embedplayer) takes precedence; otherwise web_dir
// on disk (env AUDIOSILO_WEB_DIR / config web_dir). Returns (nil, false) when
// neither is available.
func playerFS(webDir string) (fs.FS, bool) {
	if fsys, ok := embeddedPlayer(); ok {
		return fsys, true
	}
	if webDir != "" {
		return os.DirFS(webDir), true
	}
	return nil, false
}

// HasPlayer reports whether a usable web-player build (an index.html) is
// available - embedded or under webDir. Used to gate the web_player capability
// flag and to mount /web.
func HasPlayer(webDir string) bool {
	fsys, ok := playerFS(webDir)
	return ok && spa.IsFile(fsys, "index.html")
}

// PlayerSource says where the web player comes from: "embedded" (baked into
// this build), "dir" (served from webDir), or "" (none, so /web isn't mounted).
func PlayerSource(webDir string) string {
	if !HasPlayer(webDir) {
		return ""
	}
	if _, ok := embeddedPlayer(); ok {
		return "embedded"
	}
	return "dir"
}

// rootAsset serves one embedded asset from the site root (not under /assets/),
// with the strict same-origin CSP. Used for the PWA service worker and web
// manifest, which must live at the root for the worker's scope to cover /admin.
// noCache disables HTTP caching (so an updated worker is picked up promptly).
func rootAsset(fsys fs.FS, name string, noCache bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", spa.ContentType(name))
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if noCache {
			w.Header().Set("Cache-Control", "no-cache")
		}
		_, _ = w.Write(data)
	}
}

// page returns a handler that serves a single HTML file with a strict CSP.
func page(fsys fs.FS, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		_, _ = w.Write(data)
	}
}

// noSniff wraps the static /assets/ file server with the site-wide CSP and the
// X-Content-Type-Options: nosniff header, so the served CSS/JS get MIME-sniffing
// protection consistent with the player assets.
func noSniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

var inlineScriptRE = regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)

// htmlCSP builds the player's Content-Security-Policy for one HTML document.
// Scripts stay strict ('self' plus a sha256 hash of each inline <script> in the
// doc, so no 'unsafe-inline'); styles allow 'unsafe-inline' because
// react-native-web/nativewind inject styles at runtime, which cannot be hashed
// ahead of time. Everything else is same-origin.
func htmlCSP(html []byte) string {
	hashes := map[string]struct{}{}
	for _, m := range inlineScriptRE.FindAllSubmatch(html, -1) {
		if bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
			continue // external script; covered by 'self'
		}
		sum := sha256.Sum256(m[2])
		hashes["'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'"] = struct{}{}
	}
	scriptSrc := append([]string{"'self'"}, sortedKeys(hashes)...)
	return strings.Join([]string{
		"default-src 'self'",
		"img-src 'self' data: blob:",
		"media-src 'self' blob:",
		"font-src 'self' data:",
		"style-src 'self' 'unsafe-inline'",
		"script-src " + strings.Join(scriptSrc, " "),
		"connect-src 'self'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
