// Package api wires the HTTP routes, middleware and handlers for the AudioSilo
// API. It is transport-only: business logic lives in the auth, catalog, library
// and media packages.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// Version is the server version reported by GET /api/v1/server (and shown in the
// admin console + web player). It is overridden at build time from the release
// git tag via -ldflags "-X .../internal/api.Version=<tag>" (see Dockerfile +
// image.yml); an un-stamped build (e.g. local `go build`/`go run`) reports "dev".
var Version = "dev"

// webDemoPath is the web player's instant-demo screen, under the /web mount. The
// site root redirects here in demo mode. It is a route in the separately-shipped
// frontend, so this is the single point of coupling - keep it in sync with the
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

	// baseCtx is the server lifecycle context; background work detached from a
	// request (e.g. backgroundScan) derives from it so it's cancelled on shutdown
	// instead of running detached. Defaults to context.Background(); the app wires
	// the real one via SetBaseContext.
	baseCtx context.Context

	// timeoutDur bounds non-streaming requests (see the timeout middleware).
	// Defaults to requestTimeout; a field so tests can shorten it.
	timeoutDur time.Duration

	// setupToken, when non-empty, enables the first-run web setup wizard (GET/POST
	// /setup): the wizard creates the first admin + a library. It is a one-time
	// secret the caller must present (carried in the URL fragment, never logged) so
	// a remote visitor can't seize an un-set-up server. The wizard also self-closes
	// once an admin exists. Empty = wizard disabled (the headless default, which
	// bootstraps the admin via the printed first-run banner instead).
	setupToken string

	loginLimiter   *limiter // per-IP lockout for password login
	redeemLimiter  *limiter // per-IP lockout for auth-code redemption
	demoLimiter    *limiter // per-IP cap on demo account creation
	accountLimiter *limiter // per-IP cap on self-service password/recovery mutations
	ipLimiter      *ipRateLimiter

	// transcodeSem bounds the number of concurrent ffmpeg transcodes. Each transcode
	// is a long-lived process pinning roughly a core; without a cap a single client
	// (or any demo visitor) opening many ?transcode=1 streams could exhaust CPU on a
	// small self-hosted box. A full channel returns 503 rather than forking more.
	transcodeSem chan struct{}
}

// New constructs an API. ffmpeg is the path to an ffmpeg binary used for
// on-the-fly transcoding ("" disables it).
func New(cfg *config.Config, authSvc *auth.Service, cat *catalog.Catalog, scanner *library.Scanner, ffmpeg string, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{
		cfg:            cfg,
		auth:           authSvc,
		cat:            cat,
		scanner:        scanner,
		ffmpeg:         ffmpeg,
		log:            log,
		baseCtx:        context.Background(),
		timeoutDur:     requestTimeout,
		loginLimiter:   newLimiter(10, 15*time.Minute),
		redeemLimiter:  newLimiter(10, 15*time.Minute),
		demoLimiter:    newLimiter(5, 15*time.Minute),  // ≤5 demo accounts per IP / 15 min
		accountLimiter: newLimiter(10, 15*time.Minute), // ≤10 password/recovery mutations per IP / 15 min
		ipLimiter:      newIPRateLimiter(20, 40),       // ~20 req/s, burst 40, per IP
		transcodeSem:   make(chan struct{}, maxConcurrentTranscodes),
	}
}

// maxConcurrentTranscodes caps simultaneous ffmpeg transcodes across all clients.
const maxConcurrentTranscodes = 4

// EnableSetup turns on the first-run setup wizard, guarded by token (a one-time
// secret carried in the /setup URL fragment). Call before Handler(). The wizard
// still refuses to run once an admin exists, so this is safe to leave enabled.
func (a *API) EnableSetup(token string) { a.setupToken = token }

// SetBaseContext sets the server lifecycle context that detached background work
// derives from, so it's cancelled on shutdown. Call before Handler().
func (a *API) SetBaseContext(ctx context.Context) { a.baseCtx = ctx }

// Handler returns the root http.Handler with all routes and global middleware.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /api/v1/server", a.handleServerInfo)
	// Liveness/readiness probe (public): reports DB read-reachability for a
	// container healthcheck. Both /healthz and the API-prefixed form are served.
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("GET /api/v1/healthz", a.handleHealth)
	mux.HandleFunc("POST /api/v1/auth/redeem", a.handleRedeem)
	mux.HandleFunc("POST /api/v1/auth/exchange", a.handleExchange)
	mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/v1/demo/session", a.handleDemoSession)

	// First-run setup wizard (public; self-disables once an admin exists and 404s
	// unless the launcher enabled it via EnableSetup). Token-guarded on POST.
	mux.HandleFunc("GET /setup", a.handleSetupPage)
	mux.HandleFunc("POST /setup", a.handleSetup)

	// Native deep-link association files (public; 404 unless configured).
	mux.HandleFunc("GET /.well-known/apple-app-site-association", a.handleAppleAppSiteAssociation)
	mux.HandleFunc("GET /.well-known/assetlinks.json", a.handleAssetLinks)

	// Authenticated (session token).
	mux.Handle("POST /api/v1/auth/pair", a.requireAuth(http.HandlerFunc(a.handlePair)))
	mux.Handle("POST /api/v1/auth/logout", a.requireAuth(http.HandlerFunc(a.handleLogout)))
	// Self-service recovery: set your own password and/or mint a durable recovery
	// code so you can get back in after signing out without an admin.
	mux.Handle("POST /api/v1/auth/password", a.requireAuth(http.HandlerFunc(a.handleSetPassword)))
	mux.Handle("POST /api/v1/auth/recovery", a.requireAuth(http.HandlerFunc(a.handleGenerateRecovery)))
	mux.Handle("DELETE /api/v1/auth/recovery", a.requireAuth(http.HandlerFunc(a.handleDeleteRecovery)))
	// Personal API keys: user-minted, non-expiring bearer credentials for headless
	// integrations. Owner-scoped mint/list/revoke; each key acts as its owner.
	mux.Handle("POST /api/v1/auth/tokens", a.requireAuth(http.HandlerFunc(a.handleCreateAPIToken)))
	mux.Handle("GET /api/v1/auth/tokens", a.requireAuth(http.HandlerFunc(a.handleListAPITokens)))
	mux.Handle("DELETE /api/v1/auth/tokens/{id}", a.requireAuth(http.HandlerFunc(a.handleRevokeAPIToken)))
	mux.Handle("GET /api/v1/me", a.requireAuth(http.HandlerFunc(a.handleMe)))

	// Content is addressed by (library, path) via ?path= - the path is the
	// identity. The filesystem view is filtered to the caller's share scope.
	mux.Handle("GET /api/v1/libraries", a.requireAuth(http.HandlerFunc(a.handleListLibraries)))
	mux.Handle("GET /api/v1/libraries/{id}/fs", a.requireAuth(http.HandlerFunc(a.handleBrowseFS)))
	mux.Handle("GET /api/v1/libraries/{id}/books", a.requireAuth(http.HandlerFunc(a.handleListBooks)))
	mux.Handle("GET /api/v1/libraries/{id}/item", a.requireAuth(http.HandlerFunc(a.handleItem)))
	mux.Handle("GET /api/v1/libraries/{id}/chapters", a.requireAuth(http.HandlerFunc(a.handleChapters)))
	// Media GETs accept the session token as a ?token= query param (browser
	// <img>/<audio> can't set headers); other routes do not (see requireMediaAuth).
	mux.Handle("GET /api/v1/libraries/{id}/cover", a.requireMediaAuth(http.HandlerFunc(a.handleCover)))
	mux.Handle("GET /api/v1/libraries/{id}/stream", a.requireMediaAuth(http.HandlerFunc(a.handleStream)))
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
	mux.Handle("GET /api/v1/me/favourites", a.requireAuth(http.HandlerFunc(a.handleListFavourites)))
	mux.Handle("POST /api/v1/libraries/{id}/favourites", a.requireAuth(http.HandlerFunc(a.handleAddFavourite)))
	mux.Handle("DELETE /api/v1/libraries/{id}/favourites", a.requireAuth(http.HandlerFunc(a.handleRemoveFavourite)))

	// Admin.
	mux.Handle("GET /api/v1/admin/stats", a.requireAdmin(http.HandlerFunc(a.handleStats)))
	mux.Handle("GET /api/v1/admin/users", a.requireAdmin(http.HandlerFunc(a.handleListUsers)))
	mux.Handle("POST /api/v1/admin/users", a.requireAdmin(http.HandlerFunc(a.handleCreateUser)))
	mux.Handle("GET /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleGetUserDetail)))
	mux.Handle("PATCH /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateUser)))
	mux.Handle("DELETE /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteUser)))
	mux.Handle("POST /api/v1/admin/users/{id}/authcode", a.requireAdmin(http.HandlerFunc(a.handleCreateAuthCode)))
	mux.Handle("DELETE /api/v1/admin/users/{id}/recovery", a.requireAdmin(http.HandlerFunc(a.handleAdminClearRecovery)))
	mux.Handle("POST /api/v1/admin/authcodes/{id}/rotate", a.requireAdmin(http.HandlerFunc(a.handleRotateAuthCode)))
	mux.Handle("DELETE /api/v1/admin/authcodes/{id}", a.requireAdmin(http.HandlerFunc(a.handleRevokeAuthCode)))
	mux.Handle("GET /api/v1/admin/libraries", a.requireAdmin(http.HandlerFunc(a.handleAdminListLibraries)))
	mux.Handle("POST /api/v1/admin/libraries", a.requireAdmin(http.HandlerFunc(a.handleCreateLibrary)))
	mux.Handle("PUT /api/v1/admin/libraries/order", a.requireAdmin(http.HandlerFunc(a.handleReorderLibraries)))
	mux.Handle("PATCH /api/v1/admin/libraries/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateLibrary)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteLibrary)))
	mux.Handle("PUT /api/v1/admin/libraries/{id}/folder-override", a.requireAdmin(http.HandlerFunc(a.handleSetFolderOverride)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}/folder-override", a.requireAdmin(http.HandlerFunc(a.handleDeleteFolderOverride)))
	mux.Handle("PUT /api/v1/admin/libraries/{id}/enrichment", a.requireAdmin(http.HandlerFunc(a.handleSetEnrichment)))
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
	// visitor to demo.audiosilo.app lands straight on the instant-demo flow - no
	// reverse-proxy rewrite required. `/{$}` matches only "/" and outranks the web
	// package's "/" catch-all, leaving /connect, /admin and the rest untouched.
	if a.cfg.Demo.Enabled && web.HasPlayer(a.cfg.WebDir) {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, webDemoPath, http.StatusFound)
		})
	}

	// Global middleware (outermost first): security headers, CORS, real-IP,
	// per-IP rate limiting, then a per-request timeout. timeout is innermost so it
	// bounds only the handler/DB work (not the rate-limit/CORS layers) and so a
	// stuck DB connection fails fast with 503 instead of hanging forever.
	var h http.Handler = mux
	h = a.timeout(h)
	h = a.rateLimit(h)
	h = a.realIP(h)
	h = a.cors(h)
	h = a.secureHeaders(h)
	return h
}
