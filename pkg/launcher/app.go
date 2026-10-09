// Package launcher holds the AudioSilo server's run loop: load config, open the
// database, wire the services, bootstrap first-run state, and serve until the
// context is cancelled. It is shared by the headless `audiosilo` command and the
// audiosilo-manager desktop app, which runs it in-process, so both behave
// identically. This package is public (under pkg/) precisely so the manager (a
// separate module) can import Run/Options.
package launcher

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/logring"
	"github.com/kodestar/audiosilo-server/internal/notify"
	"github.com/kodestar/audiosilo-server/internal/server"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/toolfetch"
	"github.com/kodestar/audiosilo-server/internal/updates"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// Options configures a server run.
type Options struct {
	// DataDir holds config, the database and certs.
	DataDir string
	// FFprobePath / FFmpegPath are the configured tool paths ("" disables the
	// tool). A bare command name is resolved next to the executable first (so a
	// bundled ffmpeg is found), then on PATH - see resolveTool.
	FFprobePath string
	FFmpegPath  string
	// Log is the logger to use; if nil a default stderr text logger is created.
	Log *slog.Logger
	// Setup selects the first-run flow. false (default, headless): auto-create the
	// admin and print the one-time credentials banner. true (CLI --setup, or a
	// future GUI launcher): leave the admin to the browser setup wizard - when no
	// admin exists yet a token-guarded /setup is enabled instead.
	Setup bool
	// OnURL, if set, is called once at startup with the URL the user should open:
	// the token-carrying /setup URL when first-run setup is pending, otherwise the
	// web player (or admin console). A GUI launcher can use it to open a browser.
	OnURL func(url string)

	// Optional config overrides, applied on top of the loaded config.yaml for an
	// embedding launcher (the audiosilo-manager desktop app) that controls the
	// server in-process without hand-writing YAML. Empty/zero fields are ignored,
	// so the headless `audiosilo` command (which sets none) is unaffected. When the
	// config is first created these are persisted to config.yaml.
	Bind      string    // host:port to listen on, e.g. "127.0.0.1:8080"
	TLSMode   string    // "off" | "selfsigned" | "autocert"
	PublicURL string    // externally reachable base URL (e.g. a Cloudflare Tunnel URL)
	Libraries []Library // when non-nil, replaces the configured libraries
}

// Library mirrors config.Library for the public Options override API (so an
// external module can declare a library without naming an internal type).
type Library struct {
	Name string
	Root string
}

// Run loads configuration from opts.DataDir, opens the store, wires the services,
// performs first-run bootstrap (admin + auth code banner), starts the background
// scan, and serves HTTP(S) until ctx is cancelled. It blocks until shutdown.
func Run(ctx context.Context, opts Options) error {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	// Keep the newest lines for the admin console's Server > Logs; every line still
	// goes where it went before.
	logs := logring.NewRing(logRingSize)
	log = slog.New(logring.NewHandler(log.Handler(), logs, slog.LevelInfo))

	abs, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return err
	}

	cfg, firstRun, err := config.Load(abs)
	if err != nil {
		return err
	}
	applyOverrides(cfg, opts)
	// Overrides are applied after Load's validation, so re-validate before the
	// config is used (and, on firstRun, persisted): an embedding launcher can pass
	// a malformed Bind/TLSMode or a library with an empty/duplicate name, and an
	// unchecked value would otherwise scan the wrong root or bake an unbootable
	// config.yaml that fails Load on the next start.
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration after applying overrides: %w", err)
	}

	// Mint a stable per-install identity the first time (also self-heals an existing
	// install that predates server_id). Persisted to config.yaml below so it survives
	// a database rebuild; clients key their per-server state on it.
	mintedServerID := ensureServerID(cfg)

	// A restore an admin asked for replaces the database before anything opens it.
	dbPath := filepath.Join(abs, "audiosilo.db")
	restored, err := backup.ApplyPendingRestore(ctx, abs, cfg.Backups.Dir, dbPath, log)
	if err != nil {
		return err
	}

	db, err := store.Open(ctx, dbPath, store.WithLogger(log))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)
	if restored != nil {
		recordRestore(ctx, cat, restored, log)
	}

	// Notifications: the event feed and its deliveries. Messages name the server as
	// the API's live settings have it (a is set before Run starts sending).
	var a *api.API
	ntf := notify.New(cat, api.Version, func() notify.Server { return a.NotifyIdentity() }, log)

	ffmpeg, ffprobe := resolveTools(ctx, abs, opts, log)
	scanner := library.NewScanner(cat, ffprobe, log)
	scanner.OnRunFinished = ntf.ScanFinished

	// Persist a default config the first time (when none existed yet), or when we
	// just minted a server_id for an install that predates it.
	if firstRun || mintedServerID {
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	// First-run bootstrap - two mutually-exclusive paths:
	//   - default (headless/docker): auto-create the admin + print the one-time
	//     credentials banner, keyed off the database (not config-file existence).
	//   - setup mode (desktop / --setup): leave the admin to the browser wizard;
	//     when none exists yet, mint a one-time setup token and enable /setup.
	var setupToken string
	if opts.Setup {
		exists, err := authSvc.AdminExists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			setupToken = randomSecret(18)
		}
	} else if err := ensureAdmin(ctx, cfg, authSvc); err != nil {
		return err
	}

	// Sync libraries declared in config into the database, then scan them in the
	// background so the filesystem view is available immediately.
	if err := syncLibraries(ctx, cfg, cat); err != nil {
		return err
	}
	scanner.Start(ctx)
	if _, err := scanner.EnqueueAll(ctx, library.TriggerStartup, nil); err != nil {
		log.Warn("startup scan: list libraries failed", "err", err)
	}

	// In demo mode, reap idle throwaway accounts in the background.
	if cfg.Demo.Enabled {
		// Surface a misconfigured demo.library at boot rather than only as a 500 on
		// the first visitor - the library may not be declared/seeded yet.
		if _, err := cat.GetLibraryByName(ctx, cfg.Demo.Library); err != nil {
			log.Warn("demo mode enabled but demo.library not found; demo sessions will fail until it exists",
				"library", cfg.Demo.Library, "err", err)
		}
		go demoReaper(ctx, authSvc, cfg.Demo.IdleTTLDuration(), log)
	}

	// The update check (Settings > General): once a day while on, never while off.
	upd := updates.New(api.Version, "", cfg.UpdateCheck, log)
	upd.OnAvailable = func(r updates.Release) { ntf.UpdateFound(ctx, r.Version, r.Name, r.URL) }
	go upd.Run(ctx)

	// Backups (Settings > Backups): on their schedule, and when an admin asks.
	backups := backup.New(db, abs, cfg.Backups.Dir, log)
	backups.SetSettings(cfg.Backups.Schedule, cfg.Backups.Keep)
	backups.OnFailure = func(r backup.Result) { ntf.BackupFailed(ctx, r.Trigger, r.Error) }
	go backups.Run(ctx)

	// Mirror mode's local copy of the community metadata (nil in remote mode).
	// Its checks read the live metadata.enabled through a, set just below and
	// before they start.
	mirror := metaMirror(cfg, abs, func() bool { return a.MetadataOn() }, log)

	a = api.New(cfg, authSvc, cat, scanner, ffmpeg, log)
	// Before the background jobs below: it puts the mirror in front of the
	// metadata service they use.
	a.SetRuntime(api.Runtime{FFprobe: ffprobe, Logs: logs, Updates: upd, Backups: backups, Notify: ntf, MetaMirror: mirror})
	stopMirror := runMetaMirror(ctx, mirror, log)
	defer stopMirror()
	// Bulk match runs a stopped server left working: matching ones are
	// interrupted, applying ones go back to ready (what they applied is marked).
	if err := cat.InterruptMatchRuns(ctx); err != nil {
		log.Warn("settle interrupted match runs failed", "err", err)
	}
	// Listening imports a stopped server left: a fetch fails (its token died with
	// the process), an apply goes back to review (it is one transaction).
	if err := cat.InterruptImports(ctx); err != nil {
		log.Warn("settle interrupted imports failed", "err", err)
	}
	go retention(ctx, cat, authSvc, a.SessionRetention, log)
	a.SetBaseContext(ctx) // bind work detached from a request (a book's re-read) to the server lifecycle
	a.StartChapterChecks(ctx)
	a.StartCoverColors(ctx)
	ntf.Run(ctx)
	if setupToken != "" {
		a.EnableSetup(setupToken)
		setupBanner(cfg, setupToken)
	}

	// Tell the caller (the audiosilo-manager desktop app) which URL to open: the
	// setup wizard while first-run setup is pending, otherwise the player (or admin
	// console).
	if opts.OnURL != nil {
		opts.OnURL(openURL(cfg, setupToken))
	}

	return server.Run(ctx, cfg, a.Handler(), log)
}

// logRingSize is how many log lines the admin console's log viewer keeps.
const logRingSize = 2000

// applyOverrides layers the (optional) Options overrides on top of the loaded
// config. Empty/zero fields are left untouched, so a caller that sets none (the
// headless command) gets the file's configuration verbatim.
func applyOverrides(cfg *config.Config, opts Options) {
	// Each override is pinned, so the admin console shows it as managed by the
	// launcher instead of offering a change the next start would undo.
	if opts.Bind != "" {
		cfg.Bind = opts.Bind
		cfg.Pin("bind")
	}
	if opts.TLSMode != "" {
		cfg.TLS.Mode = config.TLSMode(opts.TLSMode)
		cfg.Pin("tls.mode")
	}
	if opts.PublicURL != "" {
		cfg.PublicURL = opts.PublicURL
		cfg.Pin("public_url")
	}
	if opts.Libraries != nil {
		cfg.Libraries = make([]config.Library, len(opts.Libraries))
		for i, l := range opts.Libraries {
			cfg.Libraries[i] = config.Library{Name: l.Name, Root: l.Root}
		}
		cfg.Pin("libraries")
	}
}

// openURL is the URL the user should open in a browser: the token-carrying setup
// wizard when first-run setup is pending, else the web player if one is available,
// else the admin console.
func openURL(cfg *config.Config, setupToken string) string {
	base := baseURL(cfg)
	switch {
	case setupToken != "":
		return base + "/setup#token=" + setupToken
	case web.HasPlayer(cfg.WebDir):
		return base + "/web"
	default:
		return base + "/admin"
	}
}

// baseURL builds a best-effort browser base URL from config: the configured
// public_url wins; otherwise scheme is derived from the TLS mode and the host
// from bind (a wildcard bind becomes localhost, which is the reachable address on
// the machine running the server - and a secure context for the admin PWA).
func baseURL(cfg *config.Config) string {
	if cfg.PublicURL != "" {
		return strings.TrimRight(cfg.PublicURL, "/")
	}
	scheme := "https"
	if cfg.TLS.Mode == config.TLSOff {
		scheme = "http"
	}
	host, port, err := net.SplitHostPort(cfg.Bind)
	if err != nil {
		host, port = "localhost", "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// setupBanner prints where to finish first-run setup (the token rides in the URL
// fragment, so it never reaches request logs).
func setupBanner(cfg *config.Config, token string) {
	fmt.Println("\n========================================================")
	fmt.Println(" AudioSilo first-run setup")
	fmt.Println("========================================================")
	fmt.Println(" Open this URL in your browser to finish setting up:")
	fmt.Printf("   %s\n", openURL(cfg, token))
	fmt.Println("--------------------------------------------------------")
	fmt.Println(" You'll choose an admin password and your books folder.")
	fmt.Println("========================================================")
}

// resolveTools picks the ffmpeg/ffprobe paths to use. For each enabled tool it
// prefers a local copy (explicit path, next to the executable, or $PATH); failing
// that it auto-downloads a cached static build into <data>/tools (internal/
// toolfetch). An empty configured path means the tool is disabled. ffmpeg is only
// needed for transcoding and ffprobe for chapters/durations, so an unavailable
// tool degrades gracefully (warned, not fatal).
func resolveTools(ctx context.Context, dataDir string, opts Options, log *slog.Logger) (ffmpeg, ffprobe string) {
	ffmpeg = localTool(opts.FFmpegPath)
	ffprobe = localTool(opts.FFprobePath)

	needMpeg := opts.FFmpegPath != "" && ffmpeg == ""
	needProbe := opts.FFprobePath != "" && ffprobe == ""
	if needMpeg || needProbe {
		// One download yields both tools; take whichever ones we were missing.
		dlMpeg, dlProbe := toolfetch.Ensure(ctx, toolfetch.Dir(dataDir), log)
		if needMpeg {
			ffmpeg = dlMpeg
		}
		if needProbe {
			ffprobe = dlProbe
		}
	}
	if opts.FFprobePath != "" && ffprobe == "" {
		log.Warn("ffprobe unavailable; durations and chapter extraction are disabled")
	}
	if opts.FFmpegPath != "" && ffmpeg == "" {
		log.Warn("ffmpeg unavailable; on-the-fly transcoding is disabled")
	}
	return ffmpeg, ffprobe
}

// localTool resolves a configured tool to a runnable local path, or "" if it is
// disabled ("") or not found locally. exec.LookPath handles all three local cases:
// an explicit path, the next-to-executable path resolveTool returns, or a bare
// name on $PATH.
func localTool(configured string) string {
	if configured == "" {
		return ""
	}
	if p := resolveTool(configured); p != "" {
		if _, err := exec.LookPath(p); err == nil {
			return p
		}
	}
	return ""
}

// resolveTool maps a bare ffmpeg/ffprobe command name (no path separator) to a
// copy sitting next to the running executable, if present - so a tool dropped
// beside the binary is found without touching PATH. An empty string (disabled), an
// explicit path, or a bare name with no neighbour is returned unchanged (the
// caller then resolves it via PATH).
func resolveTool(name string) string {
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsRune(name, os.PathSeparator) {
		return name
	}
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	cand := filepath.Join(filepath.Dir(exe), name)
	if runtime.GOOS == "windows" {
		cand += ".exe"
	}
	if info, err := os.Stat(cand); err == nil && !info.IsDir() {
		return cand
	}
	return name
}

// ensureAdmin creates the admin account + an initial auth code when none exists
// yet, printing the credentials exactly once. It keys off the database (whether an
// admin exists), not config-file existence, so a pre-supplied config.yaml does not
// suppress first-run admin creation.
func ensureAdmin(ctx context.Context, cfg *config.Config, authSvc *auth.Service) error {
	exists, err := authSvc.AdminExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	password := randomSecret(18)
	admin, err := authSvc.CreateUser(ctx, "admin", password, auth.RoleAdmin)
	if err != nil {
		return err
	}
	code, err := authSvc.CreateAuthCode(ctx, admin.ID, "initial admin code", 0, 0)
	if err != nil {
		return err
	}
	banner(cfg, password, code)
	return nil
}

func banner(cfg *config.Config, password, code string) {
	fmt.Println("\n========================================================")
	fmt.Println(" AudioSilo first-run setup - store these now, shown once")
	fmt.Println("========================================================")
	fmt.Printf("  Admin username : admin\n")
	fmt.Printf("  Admin password : %s\n", password)
	fmt.Printf("  Auth code      : %s\n", code)
	fmt.Printf("  Config file    : %s\n", config.Path(cfg.DataDir))
	fmt.Println("--------------------------------------------------------")
	fmt.Println(" Redeem the auth code at POST /api/v1/auth/redeem to get")
	fmt.Println(" a QR pairing code, or log in at POST /api/v1/auth/login.")
	fmt.Println("========================================================")
}

// syncLibraries upserts config-declared libraries into the catalog.
func syncLibraries(ctx context.Context, cfg *config.Config, cat *catalog.Catalog) error {
	for _, l := range cfg.Libraries {
		root, err := filepath.Abs(l.Root)
		if err != nil {
			return err
		}
		if _, err := cat.UpsertLibraryByName(ctx, catalog.Library{
			Name: l.Name, Root: root,
		}); err != nil {
			return err
		}
	}
	return nil
}

// demoReaper periodically deletes demo accounts idle longer than idleTTL, keeping
// a public demo instance clean. It sweeps once at startup and then on a fixed
// interval until ctx is cancelled.
func demoReaper(ctx context.Context, authSvc *auth.Service, idleTTL time.Duration, log *slog.Logger) {
	const interval = 15 * time.Minute
	reap := func() {
		n, err := authSvc.ReapIdleDemoUsers(ctx, time.Now().Add(-idleTTL))
		if err != nil {
			log.Warn("demo reaper failed", "err", err)
			return
		}
		if n > 0 {
			log.Info("demo reaper removed idle accounts", "count", n)
		}
	}
	reap()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reap()
		}
	}
}

// recordRestore puts a restore applied (or refused) at this start in the audit log
// of the database the server now runs on.
func recordRestore(ctx context.Context, cat *catalog.Catalog, r *backup.RestoreResult, log *slog.Logger) {
	action, details := "backup.restore_applied", map[string]any{"requested_by": r.RequestedBy}
	if r.OK {
		details["safety_copy"] = r.SafetyCopy
	} else {
		action, details["error"] = "backup.restore_failed", r.Error
	}
	if err := cat.RecordAudit(ctx, catalog.AuditEvent{Via: catalog.ViaSystem, Action: action, Target: r.Name, Details: details}); err != nil {
		log.Warn("recording the restore in the audit log failed", "err", err)
	}
}

// retention, once at startup and then daily until ctx is cancelled: rolls
// listening sessions older than sessions() (Settings > General, read at each run)
// up into per-day totals (dropping their device, app and time of day), blanks the
// address of signed-out devices, drops audit events and feed events past their
// retention, and trims the community metadata cache to its newest rows.
func retention(ctx context.Context, cat *catalog.Catalog, authSvc *auth.Service, sessions func() time.Duration, log *slog.Logger) {
	prune := func() {
		now := time.Now()
		if _, err := cat.PruneAudit(ctx, now.Add(-catalog.AuditRetention)); err != nil && ctx.Err() == nil {
			log.Warn("audit log retention failed", "err", err)
		}
		if _, err := cat.PruneServerEvents(ctx, now.Add(-catalog.ServerEventRetention)); err != nil && ctx.Err() == nil {
			log.Warn("event feed retention failed", "err", err)
		}
		if err := authSvc.ForgetRevokedAddresses(ctx); err != nil && ctx.Err() == nil {
			log.Warn("forgetting signed-out device addresses failed", "err", err)
		}
		if _, err := cat.PruneMetaCache(ctx, catalog.MetaCacheRows, catalog.MetaCacheWorkRows); err != nil && ctx.Err() == nil {
			log.Warn("meta cache retention failed", "err", err)
		}
		n, err := cat.PruneSessions(ctx, now.Add(-sessions()), time.Local)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("listening session retention failed", "err", err)
			}
			return
		}
		if n > 0 {
			log.Info("rolled old listening sessions up into daily totals", "count", n)
		}
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}

// ensureServerID mints a stable per-install identity into cfg the first time (and
// self-heals an install that predates server_id), returning whether it minted one so
// the caller persists config.yaml. The id lives in config (not the rebuildable
// database) so it survives a rescan/rebuild; clients key their per-server state on it,
// so it must never change. URL-safe so it can be both a route segment and a directory
// name on the client.
func ensureServerID(cfg *config.Config) bool {
	if cfg.ServerID != "" {
		return false
	}
	cfg.ServerID = randomSecret(16)
	return true
}

// randomSecret returns a URL-safe random string carrying nBytes of entropy (the
// encoded string is longer than nBytes characters).
func randomSecret(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
