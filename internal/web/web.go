// Package web serves the small, dependency-free admin/connect UI that ships
// baked into the server binary. It is plain HTML/CSS/JS (no build step) that
// talks to the JSON API; the audiobook player frontend remains a separate
// future project. The pages are static, so the real authorization always
// happens at the API — serving the HTML to anyone is harmless.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets
var assetsFS embed.FS

// contentSecurityPolicy locks the UI down to same-origin resources. data: is
// allowed for images so the QR pairing PNG (a data URI) renders.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'"

// Register mounts the web UI on mux:
//
//	GET /            connect page (public)
//	GET /admin       admin console (static; API enforces the admin role)
//	GET /assets/...  static CSS/JS
//
// API routes registered on the same mux take precedence because ServeMux
// prefers more specific patterns.
func Register(mux *http.ServeMux) error {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		return err
	}
	assets := http.StripPrefix("/assets/", http.FileServerFS(sub))
	mux.Handle("GET /assets/", noSniff(assets))
	mux.HandleFunc("GET /admin", page(sub, "admin.html"))
	mux.HandleFunc("GET /admin/", page(sub, "admin.html"))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// "/" is the catch-all; only the exact root serves the connect page.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		page(sub, "index.html")(w, r)
	})
	return nil
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
