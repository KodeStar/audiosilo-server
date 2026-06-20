// Package api wires the HTTP routes, middleware and handlers for the AudioSilo
// API. It is transport-only: business logic lives in the auth, catalog, library
// and media packages.
package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// Version is the API/server version reported by GET /api/v1/server.
const Version = "0.1.0"

// webDemoPath is the web player's instant-demo screen, under the /web mount. The
// site root redirects here in demo mode. It is a route in the separately-shipped
// frontend, so this is the single point of coupling — keep it in sync with the
// player's router (the Docker image pins a matching web build).
const webDemoPath = "/web/demo"

// API holds handler dependencies.
type API struct {
	cfg     *config.Config
	auth    *auth.Service
	cat     *catalog.Catalog
	scanner *library.Scanner
	ffmpeg  string // path to ffmpeg for on-the-fly transcoding; "" disables it
	log     *slog.Logger

	loginLimiter  *limiter // per-IP lockout for password login
	redeemLimiter *limiter // per-IP lockout for auth-code redemption
	demoLimiter   *limiter // per-IP cap on demo account creation
	ipLimiter     *ipRateLimiter
}

// New constructs an API. ffmpeg is the path to an ffmpeg binary used for
// on-the-fly transcoding ("" disables it).
func New(cfg *config.Config, authSvc *auth.Service, cat *catalog.Catalog, scanner *library.Scanner, ffmpeg string, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{
		cfg:           cfg,
		auth:          authSvc,
		cat:           cat,
		scanner:       scanner,
		ffmpeg:        ffmpeg,
		log:           log,
		loginLimiter:  newLimiter(10, 15*time.Minute),
		redeemLimiter: newLimiter(10, 15*time.Minute),
		demoLimiter:   newLimiter(5, 15*time.Minute), // ≤5 demo accounts per IP / 15 min
		ipLimiter:     newIPRateLimiter(20, 40),      // ~20 req/s, burst 40, per IP
	}
}

// Handler returns the root http.Handler with all routes and global middleware.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /api/v1/server", a.handleServerInfo)
	mux.HandleFunc("POST /api/v1/auth/redeem", a.handleRedeem)
	mux.HandleFunc("POST /api/v1/auth/exchange", a.handleExchange)
	mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/v1/demo/session", a.handleDemoSession)

	// Native deep-link association files (public; 404 unless configured).
	mux.HandleFunc("GET /.well-known/apple-app-site-association", a.handleAppleAppSiteAssociation)
	mux.HandleFunc("GET /.well-known/assetlinks.json", a.handleAssetLinks)

	// Authenticated (session token).
	mux.Handle("POST /api/v1/auth/pair", a.requireAuth(http.HandlerFunc(a.handlePair)))
	mux.Handle("POST /api/v1/auth/logout", a.requireAuth(http.HandlerFunc(a.handleLogout)))
	mux.Handle("GET /api/v1/me", a.requireAuth(http.HandlerFunc(a.handleMe)))

	// Content is addressed by (library, path) via ?path= — the path is the
	// identity. The filesystem view is filtered to the caller's share scope.
	mux.Handle("GET /api/v1/libraries", a.requireAuth(http.HandlerFunc(a.handleListLibraries)))
	mux.Handle("GET /api/v1/libraries/{id}/fs", a.requireAuth(http.HandlerFunc(a.handleBrowseFS)))
	mux.Handle("GET /api/v1/libraries/{id}/books", a.requireAuth(http.HandlerFunc(a.handleListBooks)))
	mux.Handle("GET /api/v1/libraries/{id}/item", a.requireAuth(http.HandlerFunc(a.handleItem)))
	mux.Handle("GET /api/v1/libraries/{id}/chapters", a.requireAuth(http.HandlerFunc(a.handleChapters)))
	mux.Handle("GET /api/v1/libraries/{id}/cover", a.requireAuth(http.HandlerFunc(a.handleCover)))
	mux.Handle("GET /api/v1/libraries/{id}/stream", a.requireAuth(http.HandlerFunc(a.handleStream)))
	mux.Handle("GET /api/v1/search", a.requireAuth(http.HandlerFunc(a.handleSearch)))
	mux.Handle("GET /api/v1/books/recent", a.requireAuth(http.HandlerFunc(a.handleRecentBooks)))

	// Per-user listening state, addressed by (library, path).
	mux.Handle("GET /api/v1/me/progress", a.requireAuth(http.HandlerFunc(a.handleListProgress)))
	mux.Handle("GET /api/v1/libraries/{id}/progress", a.requireAuth(http.HandlerFunc(a.handleGetProgress)))
	mux.Handle("PUT /api/v1/libraries/{id}/progress", a.requireAuth(http.HandlerFunc(a.handlePutProgress)))
	mux.Handle("GET /api/v1/libraries/{id}/bookmarks", a.requireAuth(http.HandlerFunc(a.handleListBookmarks)))
	mux.Handle("POST /api/v1/libraries/{id}/bookmarks", a.requireAuth(http.HandlerFunc(a.handleAddBookmark)))
	mux.Handle("DELETE /api/v1/bookmarks/{id}", a.requireAuth(http.HandlerFunc(a.handleDeleteBookmark)))
	mux.Handle("GET /api/v1/libraries/{id}/notes", a.requireAuth(http.HandlerFunc(a.handleListNotes)))
	mux.Handle("POST /api/v1/libraries/{id}/notes", a.requireAuth(http.HandlerFunc(a.handleAddNote)))
	mux.Handle("DELETE /api/v1/notes/{id}", a.requireAuth(http.HandlerFunc(a.handleDeleteNote)))
	mux.Handle("GET /api/v1/me/history", a.requireAuth(http.HandlerFunc(a.handleListAllHistory)))
	mux.Handle("GET /api/v1/libraries/{id}/history", a.requireAuth(http.HandlerFunc(a.handleListHistory)))
	mux.Handle("POST /api/v1/libraries/{id}/history", a.requireAuth(http.HandlerFunc(a.handleAddHistory)))

	// Admin.
	mux.Handle("GET /api/v1/admin/stats", a.requireAdmin(http.HandlerFunc(a.handleStats)))
	mux.Handle("GET /api/v1/admin/users", a.requireAdmin(http.HandlerFunc(a.handleListUsers)))
	mux.Handle("POST /api/v1/admin/users", a.requireAdmin(http.HandlerFunc(a.handleCreateUser)))
	mux.Handle("GET /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleGetUserDetail)))
	mux.Handle("PATCH /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateUser)))
	mux.Handle("POST /api/v1/admin/users/{id}/authcode", a.requireAdmin(http.HandlerFunc(a.handleCreateAuthCode)))
	mux.Handle("DELETE /api/v1/admin/authcodes/{id}", a.requireAdmin(http.HandlerFunc(a.handleRevokeAuthCode)))
	mux.Handle("GET /api/v1/admin/libraries", a.requireAdmin(http.HandlerFunc(a.handleAdminListLibraries)))
	mux.Handle("POST /api/v1/admin/libraries", a.requireAdmin(http.HandlerFunc(a.handleCreateLibrary)))
	mux.Handle("PATCH /api/v1/admin/libraries/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateLibrary)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteLibrary)))
	mux.Handle("PUT /api/v1/admin/libraries/{id}/folder-override", a.requireAdmin(http.HandlerFunc(a.handleSetFolderOverride)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}/folder-override", a.requireAdmin(http.HandlerFunc(a.handleDeleteFolderOverride)))
	mux.Handle("POST /api/v1/admin/libraries/{id}/scan", a.requireAdmin(http.HandlerFunc(a.handleScanLibrary)))
	mux.Handle("GET /api/v1/admin/libraries/{id}/scan", a.requireAdmin(http.HandlerFunc(a.handleScanStatus)))

	// Filesystem-based shares: named sets of path rules, granted to users.
	mux.Handle("GET /api/v1/admin/shares", a.requireAdmin(http.HandlerFunc(a.handleListShares)))
	mux.Handle("POST /api/v1/admin/shares", a.requireAdmin(http.HandlerFunc(a.handleCreateShare)))
	mux.Handle("GET /api/v1/admin/shares/{id}", a.requireAdmin(http.HandlerFunc(a.handleGetShare)))
	mux.Handle("PATCH /api/v1/admin/shares/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateShare)))
	mux.Handle("DELETE /api/v1/admin/shares/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteShare)))
	mux.Handle("POST /api/v1/admin/shares/{id}/paths", a.requireAdmin(http.HandlerFunc(a.handleAddSharePath)))
	mux.Handle("DELETE /api/v1/admin/shares/{id}/paths", a.requireAdmin(http.HandlerFunc(a.handleRemoveSharePath)))
	mux.Handle("POST /api/v1/admin/share-access", a.requireAdmin(http.HandlerFunc(a.handleGrantShare)))
	mux.Handle("DELETE /api/v1/admin/share-access", a.requireAdmin(http.HandlerFunc(a.handleRevokeShare)))
	mux.Handle("POST /api/v1/admin/library-access", a.requireAdmin(http.HandlerFunc(a.handleGrantWholeLibrary)))

	// Baked-in web UI: the public connect page and the admin console. API routes
	// above are more specific, so ServeMux still prefers them over the "/"
	// catch-all the web package registers.
	if err := web.Register(mux, a.cfg.WebDir); err != nil {
		a.log.Error("failed to register web UI", "err", err)
	}

	// In demo mode, send the exact site root to the web player's demo screen so a
	// visitor to demo.audiosilo.app lands straight on the instant-demo flow — no
	// reverse-proxy rewrite required. `/{$}` matches only "/" and outranks the web
	// package's "/" catch-all, leaving /connect, /admin and the rest untouched.
	if a.cfg.Demo.Enabled && web.HasPlayer(a.cfg.WebDir) {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, webDemoPath, http.StatusFound)
		})
	}

	// Global middleware (outermost first): security headers, CORS, real-IP,
	// then per-IP rate limiting.
	var h http.Handler = mux
	h = a.rateLimit(h)
	h = a.realIP(h)
	h = a.cors(h)
	h = a.secureHeaders(h)
	return h
}
