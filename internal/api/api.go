// Package api wires the HTTP routes, middleware and handlers for the AudioSilo
// API. It is transport-only: business logic lives in the auth, catalog, library
// and media packages.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/meta"
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
	// boot is the config the server started with; live (read through config())
	// is the one it works with now, swapped whole by a settings save
	// (handlers_settings.go). Neither is ever mutated in place.
	boot    *config.Config
	live    atomic.Pointer[liveConfig]
	auth    *auth.Service
	cat     *catalog.Catalog
	scanner *library.Scanner
	ffmpeg  string // path to ffmpeg for on-the-fly transcoding; "" disables it
	// meta resolves book asin/isbn against the community metadata API (Phase 1.5).
	// It is constructed whenever metadata.base_url is a valid absolute http(s) URL
	// (regardless of metadata.enabled), so the runtime admin toggle can flip the
	// feature on without a restart; nil only when base_url is empty/invalid, in
	// which case the feature is unavailable and cannot be enabled. The live
	// config's metadata.enabled is the on/off switch: the handler and the
	// `metadata` capability flag gate on meta != nil AND it (metadataOn).
	meta *meta.Service
	// settingsMu serializes settings saves (read, change, write config.yaml, swap).
	settingsMu sync.Mutex
	log        *slog.Logger

	// rt is what the launcher reports about the running process (SetRuntime):
	// tool paths, start time, the log ring and the update checker.
	rt           Runtime
	toolVersions toolVersions
	// playerSource is where /web is served from ("embedded", "dir", ""), fixed
	// for the life of the process like the mount itself.
	playerSource string

	// baseCtx is the server lifecycle context; work detached from a request (a
	// book's re-read, which may outlast the request timeout) derives from it so it's
	// cancelled on shutdown instead of running on. Defaults to context.Background(); the app wires
	// the real one via SetBaseContext.
	baseCtx context.Context

	// indexPath reads a path's book on demand (bookForPath's fallback):
	// scanner.IndexPathWithin, a field so tests can count the re-reads.
	indexPath func(ctx context.Context, lib catalog.Library, rel string, allow func(string) bool) (*catalog.Book, error)

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

	loginLimiter   *limiter     // per-IP lockout for password login
	redeemLimiter  *limiter     // per-IP lockout for auth-code redemption
	demoLimiter    *limiter     // per-IP cap on demo account creation
	accountLimiter *limiter     // per-IP cap on self-service password/recovery mutations
	ipLimiter      *rateLimiter // general request rate, per IP (see rateLimit)
	mediaLimiter   *rateLimiter // authenticated media rate, per credential (see requireMediaAuth)

	// transcodeSem bounds the number of concurrent ffmpeg transcodes. Each transcode
	// is a long-lived process pinning roughly a core; without a cap a single client
	// (or any demo visitor) opening many ?transcode=1 streams could exhaust CPU on a
	// small self-hosted box. A full channel returns 503 rather than forking more.
	transcodeSem chan struct{}

	// thumbs caches cover thumbnails (GET cover ?size= and the admin console's
	// batch); coverReads bounds the art being read or waiting to be decoded and
	// thumbSem how many are decoded at once, both across requests (see
	// handlers_covers.go).
	thumbs     *media.ThumbCache
	coverReads chan struct{}
	thumbSem   chan struct{}

	// streams remembers recent transcoded streams per token, so the progress saves
	// that follow mark the listening session as transcoded.
	streams *catalog.StreamMarks
}

// New constructs an API. ffmpeg is the path to an ffmpeg binary used for
// on-the-fly transcoding ("" disables it).
func New(cfg *config.Config, authSvc *auth.Service, cat *catalog.Catalog, scanner *library.Scanner, ffmpeg string, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	// Construct the metadata lookup service whenever base_url is a valid absolute
	// http(s) URL - independent of metadata.enabled - so the admin runtime toggle
	// can turn it on without a restart. nil only when base_url is empty/invalid, in
	// which case the feature is permanently unavailable until the config gains one.
	var metaSvc *meta.Service
	if cfg.Metadata.ValidBaseURL() {
		metaSvc = meta.NewService(cfg.Metadata.BaseURL, nil)
		// Its cache's persistent second level, in the server's own database. No
		// config key of its own: it is read and written only through the
		// service, which only runs while metadata.enabled is on.
		metaSvc.SetStore(metaStore{cat: cat, log: log})
	}
	a := &API{
		boot:           cfg,
		auth:           authSvc,
		cat:            cat,
		scanner:        scanner,
		indexPath:      scanner.IndexPathWithin,
		ffmpeg:         ffmpeg,
		meta:           metaSvc,
		log:            log,
		baseCtx:        context.Background(),
		timeoutDur:     requestTimeout,
		loginLimiter:   newLimiter(10, 15*time.Minute),
		redeemLimiter:  newLimiter(10, 15*time.Minute),
		demoLimiter:    newLimiter(5, 15*time.Minute),  // ≤5 demo accounts per IP / 15 min
		accountLimiter: newLimiter(10, 15*time.Minute), // ≤10 password/recovery mutations per IP / 15 min
		ipLimiter:      newRateLimiter(50, 200),        // ~50 req/s, burst 200, per IP
		mediaLimiter:   newRateLimiter(200, 2000),      // ~200 req/s, burst 2000, per credential
		transcodeSem:   make(chan struct{}, maxConcurrentTranscodes),
		thumbs:         media.NewThumbCache(thumbCacheBytes),
		coverReads:     make(chan struct{}, maxCoverReads),
		thumbSem:       make(chan struct{}, maxConcurrentThumbnails),
		streams:        catalog.NewStreamMarks(),
	}
	a.live.Store(newLiveConfig(cfg, cfg))
	a.playerSource = web.PlayerSource(cfg.WebDir)
	a.rt.StartedAt = time.Now()
	return a
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
	mux.Handle("GET /api/v1/libraries/{id}/authors", a.requireAuth(a.handleBrowsePeople(catalog.PeopleAuthors, "authors")))
	mux.Handle("GET /api/v1/libraries/{id}/narrators", a.requireAuth(a.handleBrowsePeople(catalog.PeopleNarrators, "narrators")))
	mux.Handle("GET /api/v1/libraries/{id}/series", a.requireAuth(http.HandlerFunc(a.handleBrowseSeries)))
	mux.Handle("GET /api/v1/libraries/{id}/item", a.requireAuth(http.HandlerFunc(a.handleItem)))
	mux.Handle("GET /api/v1/libraries/{id}/next", a.requireAuth(http.HandlerFunc(a.handleNext)))
	mux.Handle("GET /api/v1/libraries/{id}/chapters", a.requireAuth(http.HandlerFunc(a.handleChapters)))
	mux.Handle("GET /api/v1/libraries/{id}/meta", a.requireAuth(http.HandlerFunc(a.handleMeta)))
	// Community work lookup by metadata-site work id - global, read-only data
	// (no library, nothing to scope), so plain auth. The id is a query param
	// because work-id slugs are not path-segment safe.
	mux.Handle("GET /api/v1/meta/work", a.requireAuth(http.HandlerFunc(a.handleMetaWork)))
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

	// Phase 1b: up next + collections
	mux.Handle("GET /api/v1/me/queue", a.requireAuth(http.HandlerFunc(a.handleGetQueue)))
	mux.Handle("PUT /api/v1/me/queue", a.requireAuth(http.HandlerFunc(a.handleSetQueue)))
	mux.Handle("POST /api/v1/me/queue", a.requireAuth(http.HandlerFunc(a.handleAddToQueue)))
	mux.Handle("DELETE /api/v1/me/queue", a.requireAuth(http.HandlerFunc(a.handleRemoveFromQueue)))
	mux.Handle("GET /api/v1/me/collections", a.requireAuth(http.HandlerFunc(a.handleListCollections)))
	mux.Handle("POST /api/v1/me/collections", a.requireAuth(http.HandlerFunc(a.handleCreateCollection)))
	mux.Handle("GET /api/v1/me/collections/{id}", a.requireAuth(http.HandlerFunc(a.handleGetCollection)))
	mux.Handle("PATCH /api/v1/me/collections/{id}", a.requireAuth(http.HandlerFunc(a.handleUpdateCollection)))
	mux.Handle("DELETE /api/v1/me/collections/{id}", a.requireAuth(http.HandlerFunc(a.handleDeleteCollection)))
	mux.Handle("PUT /api/v1/me/collections/{id}/items", a.requireAuth(http.HandlerFunc(a.handleSetCollectionItems)))
	mux.Handle("POST /api/v1/me/collections/{id}/items", a.requireAuth(http.HandlerFunc(a.handleAddCollectionItem)))
	mux.Handle("DELETE /api/v1/me/collections/{id}/items", a.requireAuth(http.HandlerFunc(a.handleRemoveCollectionItem)))
	mux.Handle("PUT /api/v1/me/collections/{id}/shares", a.requireAuth(http.HandlerFunc(a.handleSetCollectionShares)))
	mux.Handle("GET /api/v1/me/share-targets", a.requireAuth(http.HandlerFunc(a.handleShareTargets)))

	// Phase 1b: your listening (capability user_stats): the caller's own stats,
	// listening per day and yearly goal.
	mux.Handle("GET /api/v1/me/stats", a.requireAuth(http.HandlerFunc(a.handleMyStats)))
	mux.Handle("GET /api/v1/me/listening", a.requireAuth(http.HandlerFunc(a.handleMyListening)))
	mux.Handle("GET /api/v1/me/goal", a.requireAuth(http.HandlerFunc(a.handleGetGoal)))
	mux.Handle("PUT /api/v1/me/goal", a.requireAuth(http.HandlerFunc(a.handlePutGoal)))
	mux.Handle("DELETE /api/v1/me/goal", a.requireAuth(http.HandlerFunc(a.handleDeleteGoal)))

	// Phase 1b: ratings, progress edits, my devices
	mux.Handle("GET /api/v1/libraries/{id}/rating", a.requireAuth(http.HandlerFunc(a.handleGetRating)))
	mux.Handle("PUT /api/v1/libraries/{id}/rating", a.requireAuth(http.HandlerFunc(a.handlePutRating)))
	mux.Handle("DELETE /api/v1/libraries/{id}/rating", a.requireAuth(http.HandlerFunc(a.handleDeleteRating)))
	mux.Handle("GET /api/v1/me/ratings", a.requireAuth(http.HandlerFunc(a.handleListRatings)))
	mux.Handle("PATCH /api/v1/libraries/{id}/progress", a.requireAuth(http.HandlerFunc(a.handleEditMyProgress)))
	mux.Handle("GET /api/v1/me/devices", a.requireAuth(http.HandlerFunc(a.handleListMyDevices)))
	mux.Handle("DELETE /api/v1/me/devices/{id}", a.requireAuth(http.HandlerFunc(a.handleRevokeMyDevice)))

	// Admin.
	mux.Handle("GET /api/v1/admin/stats", a.requireAdmin(http.HandlerFunc(a.handleStats)))
	mux.Handle("GET /api/v1/admin/settings", a.requireAdmin(http.HandlerFunc(a.handleGetSettings)))
	mux.Handle("PATCH /api/v1/admin/settings", a.requireAdmin(http.HandlerFunc(a.handleUpdateSettings)))
	mux.Handle("GET /api/v1/admin/system", a.requireAdmin(http.HandlerFunc(a.handleSystem)))
	mux.Handle("GET /api/v1/admin/update", a.requireAdmin(http.HandlerFunc(a.handleUpdateStatus)))
	mux.Handle("POST /api/v1/admin/update/check", a.requireAdmin(http.HandlerFunc(a.handleUpdateCheck)))
	mux.Handle("GET /api/v1/admin/logs", a.requireAdmin(http.HandlerFunc(a.handleLogs)))
	mux.Handle("GET /api/v1/admin/audit", a.requireAdmin(http.HandlerFunc(a.handleAudit)))
	mux.Handle("GET /api/v1/admin/events", a.requireAdmin(http.HandlerFunc(a.handleServerEvents)))

	// Backups: the database copied into the backups folder; a restore applies at the
	// next start. Downloads stream (see isStreamingPath).
	mux.Handle("GET /api/v1/admin/backups", a.requireAdmin(http.HandlerFunc(a.handleListBackups)))
	mux.Handle("POST /api/v1/admin/backups", a.requireAdmin(http.HandlerFunc(a.handleCreateBackup)))
	mux.Handle("GET /api/v1/admin/backups/{name}", a.requireAdmin(http.HandlerFunc(a.handleDownloadBackup)))
	mux.Handle("DELETE /api/v1/admin/backups/{name}", a.requireAdmin(http.HandlerFunc(a.handleDeleteBackup)))
	mux.Handle("POST /api/v1/admin/backups/{name}/restore", a.requireAdmin(http.HandlerFunc(a.handleRestoreBackup)))
	mux.Handle("DELETE /api/v1/admin/restore", a.requireAdmin(http.HandlerFunc(a.handleCancelRestore)))

	// Notification destinations.
	mux.Handle("GET /api/v1/admin/notifications", a.requireAdmin(http.HandlerFunc(a.handleListNotifyTargets)))
	mux.Handle("POST /api/v1/admin/notifications", a.requireAdmin(http.HandlerFunc(a.handleCreateNotifyTarget)))
	mux.Handle("PATCH /api/v1/admin/notifications/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateNotifyTarget)))
	mux.Handle("DELETE /api/v1/admin/notifications/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteNotifyTarget)))
	mux.Handle("POST /api/v1/admin/notifications/{id}/test", a.requireAdmin(http.HandlerFunc(a.handleTestNotifyTarget)))
	mux.Handle("GET /api/v1/admin/users", a.requireAdmin(http.HandlerFunc(a.handleListUsers)))
	mux.Handle("POST /api/v1/admin/users", a.requireAdmin(http.HandlerFunc(a.handleCreateUser)))
	mux.Handle("GET /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleGetUserDetail)))
	mux.Handle("PATCH /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateUser)))
	mux.Handle("DELETE /api/v1/admin/users/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteUser)))
	mux.Handle("POST /api/v1/admin/users/{id}/authcode", a.requireAdmin(http.HandlerFunc(a.handleCreateAuthCode)))
	mux.Handle("DELETE /api/v1/admin/users/{id}/recovery", a.requireAdmin(http.HandlerFunc(a.handleAdminClearRecovery)))
	mux.Handle("GET /api/v1/admin/invites", a.requireAdmin(http.HandlerFunc(a.handleListInvites)))
	mux.Handle("POST /api/v1/admin/authcodes/{id}/rotate", a.requireAdmin(http.HandlerFunc(a.handleRotateAuthCode)))
	mux.Handle("DELETE /api/v1/admin/authcodes/{id}", a.requireAdmin(http.HandlerFunc(a.handleRevokeAuthCode)))
	mux.Handle("GET /api/v1/admin/libraries", a.requireAdmin(http.HandlerFunc(a.handleAdminListLibraries)))
	mux.Handle("GET /api/v1/admin/fs/dirs", a.requireAdmin(http.HandlerFunc(a.handleListDirs)))
	mux.Handle("POST /api/v1/admin/libraries", a.requireAdmin(http.HandlerFunc(a.handleCreateLibrary)))
	mux.Handle("PUT /api/v1/admin/libraries/order", a.requireAdmin(http.HandlerFunc(a.handleReorderLibraries)))
	mux.Handle("PATCH /api/v1/admin/libraries/{id}", a.requireAdmin(http.HandlerFunc(a.handleUpdateLibrary)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}", a.requireAdmin(http.HandlerFunc(a.handleDeleteLibrary)))
	mux.Handle("PUT /api/v1/admin/libraries/{id}/folder-override", a.requireAdmin(http.HandlerFunc(a.handleSetFolderOverride)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}/folder-override", a.requireAdmin(http.HandlerFunc(a.handleDeleteFolderOverride)))
	mux.Handle("PUT /api/v1/admin/libraries/{id}/enrichment", a.requireAdmin(http.HandlerFunc(a.handleSetEnrichment)))
	mux.Handle("GET /api/v1/admin/libraries/{id}/export", a.requireAdmin(http.HandlerFunc(a.handleExportLibrary)))
	mux.Handle("POST /api/v1/admin/libraries/{id}/scan", a.requireAdmin(http.HandlerFunc(a.handleScanLibrary)))
	mux.Handle("GET /api/v1/admin/libraries/{id}/scan", a.requireAdmin(http.HandlerFunc(a.handleScanStatus)))
	mux.Handle("POST /api/v1/admin/scan", a.requireAdmin(http.HandlerFunc(a.handleScanAll)))
	mux.Handle("POST /api/v1/admin/libraries/{id}/book/rescan", a.requireAdmin(http.HandlerFunc(a.handleRescanBook)))
	mux.Handle("GET /api/v1/admin/issues", a.requireAdmin(http.HandlerFunc(a.handleIssues)))
	mux.Handle("GET /api/v1/admin/issues/duplicates", a.requireAdmin(http.HandlerFunc(a.handleDuplicates)))
	mux.Handle("POST /api/v1/admin/issues/ignore", a.requireAdmin(http.HandlerFunc(a.handleIgnoreIssue)))
	mux.Handle("DELETE /api/v1/admin/issues/ignore", a.requireAdmin(http.HandlerFunc(a.handleUnignoreIssue)))
	mux.Handle("GET /api/v1/admin/jobs", a.requireAdmin(http.HandlerFunc(a.handleJobs)))
	mux.Handle("DELETE /api/v1/admin/jobs/{id}", a.requireAdmin(http.HandlerFunc(a.handleCancelJob)))
	mux.Handle("GET /api/v1/admin/scan-runs", a.requireAdmin(http.HandlerFunc(a.handleScanRuns)))
	mux.Handle("GET /api/v1/admin/scan-runs/{id}", a.requireAdmin(http.HandlerFunc(a.handleScanRun)))

	// Activity: listening sessions (derived from progress saves), devices (signed-in
	// tokens) and an admin's edits of someone's progress. Stats are GET
	// /admin/stats?range=.
	mux.Handle("GET /api/v1/admin/listening", a.requireAdmin(http.HandlerFunc(a.handleListeningDays)))
	mux.Handle("GET /api/v1/admin/sessions/live", a.requireAdmin(http.HandlerFunc(a.handleLiveSessions)))
	mux.Handle("GET /api/v1/admin/sessions", a.requireAdmin(http.HandlerFunc(a.handleListSessions)))
	mux.Handle("GET /api/v1/admin/devices", a.requireAdmin(http.HandlerFunc(a.handleListDevices)))
	mux.Handle("DELETE /api/v1/admin/devices/{id}", a.requireAdmin(http.HandlerFunc(a.handleRevokeDevice)))
	mux.Handle("GET /api/v1/admin/users/{id}/progress", a.requireAdmin(http.HandlerFunc(a.handleUserProgress)))
	mux.Handle("PATCH /api/v1/admin/libraries/{id}/progress", a.requireAdmin(http.HandlerFunc(a.handleEditProgress)))

	// Admin catalog: the console's Library and Book screens. Metadata edits are
	// path-keyed overrides in the database; no file on disk is ever modified.
	mux.Handle("GET /api/v1/admin/books", a.requireAdmin(http.HandlerFunc(a.handleAdminListBooks)))
	mux.Handle("GET /api/v1/admin/books/facets", a.requireAdmin(http.HandlerFunc(a.handleAdminBookFacets)))
	mux.Handle("POST /api/v1/admin/books/bulk", a.requireAdmin(http.HandlerFunc(a.handleAdminBulkEdit)))
	mux.Handle("POST /api/v1/admin/covers", a.requireAdmin(http.HandlerFunc(a.handleAdminCovers)))
	mux.Handle("POST /api/v1/admin/books/works", a.requireAdmin(http.HandlerFunc(a.handleAdminBookWorks)))
	mux.Handle("GET /api/v1/admin/authors", a.requireAdmin(a.handleAdminPeople(catalog.PeopleAuthors, "authors")))
	mux.Handle("GET /api/v1/admin/narrators", a.requireAdmin(a.handleAdminPeople(catalog.PeopleNarrators, "narrators")))
	mux.Handle("GET /api/v1/admin/series", a.requireAdmin(http.HandlerFunc(a.handleAdminSeries)))
	mux.Handle("GET /api/v1/admin/libraries/{id}/book", a.requireAdmin(http.HandlerFunc(a.handleAdminBook)))
	mux.Handle("PATCH /api/v1/admin/libraries/{id}/book", a.requireAdmin(http.HandlerFunc(a.handleAdminEditBook)))
	mux.Handle("GET /api/v1/admin/libraries/{id}/book/match", a.requireAdmin(http.HandlerFunc(a.handleAdminMatch)))
	mux.Handle("PUT /api/v1/admin/libraries/{id}/cover", a.requireAdmin(http.HandlerFunc(a.handleAdminSetCover)))
	mux.Handle("DELETE /api/v1/admin/libraries/{id}/cover", a.requireAdmin(http.HandlerFunc(a.handleAdminDeleteCover)))

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
	if err := web.Register(mux, a.config().WebDir); err != nil {
		a.log.Error("failed to register web UI", "err", err)
	}

	// In demo mode, send the exact site root to the web player's demo screen so a
	// visitor to demo.audiosilo.app lands straight on the instant-demo flow - no
	// reverse-proxy rewrite required. `/{$}` matches only "/" and outranks the web
	// package's "/" catch-all, leaving /connect, /admin and the rest untouched.
	if a.config().Demo.Enabled && a.playerSource != "" {
		mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, webDemoPath, http.StatusFound)
		})
	}

	// Global middleware (outermost first): security headers, CORS, real-IP,
	// rate limiting by route class, then a per-request timeout. timeout is innermost so it
	// bounds only the handler/DB work (not the rate-limit/CORS layers) and so a
	// stuck DB connection fails fast with 503 instead of hanging forever.
	var h http.Handler = mux
	h = a.timeout(h)
	h = a.rateLimit(mux, h)
	h = a.realIP(h)
	h = a.cors(h)
	h = a.secureHeaders(h)
	return h
}
