// Package web serves the small, dependency-free admin/connect UI that ships baked
// into the server binary, plus (optionally) the web player at /web.
//
// The admin/connect pages are plain HTML/CSS/JS (no build step) embedded in the
// binary; they talk to the JSON API and are static, so the real authorization
// always happens at the API. The web player is a separate project (the
// audiosilo-frontend Expo export) and is NOT vendored here: it is served at
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
	"time"
)

//go:embed assets
var assetsFS embed.FS

// contentSecurityPolicy locks the admin/connect UI down to same-origin resources.
// data: is allowed for images so the QR pairing PNG (a data URI) renders.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// Register mounts the web UI on mux:
//
//	GET /                 connect page (public)
//	GET /connect[/]       connect page (the copy-invite link target)
//	GET /admin            admin console (static; API enforces the admin role)
//	GET /assets/...       static CSS/JS
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
	mux.HandleFunc("GET /admin", page(sub, "admin.html"))
	mux.HandleFunc("GET /admin/", page(sub, "admin.html"))
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

	if webDir != "" {
		mux.Handle("GET /web/", playerHandler(os.DirFS(webDir)))
	}
	return nil
}

// HasPlayer reports whether webDir holds a usable web-player build (an
// index.html). Used to gate the web_player capability flag.
func HasPlayer(webDir string) bool {
	if webDir == "" {
		return false
	}
	info, err := os.Stat(webDir + "/index.html")
	return err == nil && !info.IsDir()
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

func noSniff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		next.ServeHTTP(w, r)
	})
}

// playerHandler serves the web player from fsys (an os.DirFS over web_dir) with an
// SPA fallback: a request that resolves to no file but looks like a navigation
// serves index.html so client-side routing (and deep links like
// /web/connect?token=) work. Missing static assets still 404.
func playerHandler(fsys fs.FS) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/web/")
		name, ok := resolvePlayerFile(fsys, rel)
		if !ok {
			if isAsset(rel) {
				http.NotFound(w, r)
				return
			}
			name = "index.html" // client-routed deep link: boot the SPA
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasSuffix(name, ".html") {
			// HTML carries the scoped CSP, with a hash of its own inline scripts so
			// the player boots without 'unsafe-inline' for scripts. Computed from the
			// served bytes so it stays correct after an image-rebuilt player swap.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", htmlCSP(data))
			w.Header().Set("Cache-Control", "no-cache")
		} else if strings.HasPrefix(rel, "_expo/") || strings.HasPrefix(rel, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		http.ServeContent(w, r, name, modTime(fsys, name), bytes.NewReader(data))
	}
}

func modTime(fsys fs.FS, name string) time.Time {
	if info, err := fs.Stat(fsys, name); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}

// resolvePlayerFile maps a request path to a file, trying the exact path, then
// "<path>.html", then "<path>/index.html" (Expo emits per-route HTML).
func resolvePlayerFile(fsys fs.FS, p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		p = "index.html"
	}
	for _, cand := range []string{p, p + ".html", p + "/index.html"} {
		if isFile(fsys, cand) {
			return cand, true
		}
	}
	return "", false
}

func isFile(fsys fs.FS, name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// isAsset reports whether a path should 404 rather than SPA-fallback when missing
// (fingerprinted bundles, media, anything with a non-HTML extension).
func isAsset(p string) bool {
	if strings.HasPrefix(p, "_expo/") || strings.HasPrefix(p, "assets/") {
		return true
	}
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	if dot := strings.LastIndexByte(base, '.'); dot >= 0 {
		return !strings.EqualFold(base[dot:], ".html")
	}
	return false
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
