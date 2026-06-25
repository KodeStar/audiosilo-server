// Package app holds the AudioSilo server's run loop: load config, open the
// database, wire the services, bootstrap first-run state, and serve until the
// context is cancelled. It is shared by the headless `audiosilo` command and the
// desktop tray build (`audiosilo-desktop`) so both behave identically.
package app

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
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/server"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/toolfetch"
	"github.com/kodestar/audiosilo-server/internal/web"
)

// Options configures a server run.
type Options struct {
	// DataDir holds config, the database and certs.
	DataDir string
	// FFprobePath / FFmpegPath are the configured tool paths ("" disables the
	// tool). A bare command name is resolved next to the executable first (so a
	// bundled ffmpeg is found), then on PATH — see resolveTool.
	FFprobePath string
	FFmpegPath  string
	// Log is the logger to use; if nil a default stderr text logger is created.
	Log *slog.Logger
	// Setup selects the first-run flow. false (default, headless): auto-create the
	// admin and print the one-time credentials banner. true (CLI --setup, or a
	// future GUI launcher): leave the admin to the browser setup wizard — when no
	// admin exists yet a token-guarded /setup is enabled instead.
	Setup bool
	// OnURL, if set, is called once at startup with the URL the user should open:
	// the token-carrying /setup URL when first-run setup is pending, otherwise the
	// web player (or admin console). A GUI launcher can use it to open a browser.
	OnURL func(url string)
}

// Run loads configuration from opts.DataDir, opens the store, wires the services,
// performs first-run bootstrap (admin + auth code banner), starts the background
// scan, and serves HTTP(S) until ctx is cancelled. It blocks until shutdown.
func Run(ctx context.Context, opts Options) error {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

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

	db, err := store.Open(ctx, filepath.Join(abs, "audiosilo.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)

	ffmpeg, ffprobe := resolveTools(ctx, abs, opts, log)
	scanner := library.NewScanner(cat, ffprobe, log)

	// Persist a default config the first time (when none existed yet).
	if firstRun {
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	// First-run bootstrap — two mutually-exclusive paths:
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
	go initialScan(ctx, cat, scanner, log)

	// In demo mode, reap idle throwaway accounts in the background.
	if cfg.Demo.Enabled {
		// Surface a misconfigured demo.library at boot rather than only as a 500 on
		// the first visitor — the library may not be declared/seeded yet.
		if _, err := cat.GetLibraryByName(ctx, cfg.Demo.Library); err != nil {
			log.Warn("demo mode enabled but demo.library not found; demo sessions will fail until it exists",
				"library", cfg.Demo.Library, "err", err)
		}
		go demoReaper(ctx, authSvc, cfg.Demo.IdleTTLDuration(), log)
	}

	a := api.New(cfg, authSvc, cat, scanner, ffmpeg, log)
	if setupToken != "" {
		a.EnableSetup(setupToken)
		setupBanner(cfg, setupToken)
	}

	// Tell the caller (the desktop tray) which URL to open: the setup wizard while
	// first-run setup is pending, otherwise the player (or admin console).
	if opts.OnURL != nil {
		opts.OnURL(openURL(cfg, setupToken))
	}

	return server.Run(ctx, cfg, a.Handler(), log)
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
// the machine running the server — and a secure context for the admin PWA).
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
		dlMpeg, dlProbe := toolfetch.Ensure(ctx, filepath.Join(dataDir, "tools"), log)
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
// copy sitting next to the running executable, if present — so a tool dropped
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
	fmt.Println(" AudioSilo first-run setup — store these now, shown once")
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

// initialScan scans every library once at startup.
func initialScan(ctx context.Context, cat *catalog.Catalog, scanner *library.Scanner, log *slog.Logger) {
	libs, err := cat.ListLibraries(ctx)
	if err != nil {
		log.Warn("initial scan: list libraries failed", "err", err)
		return
	}
	for _, l := range libs {
		if _, err := scanner.Scan(ctx, l); err != nil {
			log.Warn("initial scan failed", "library", l.Name, "err", err)
		}
	}
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

// randomSecret returns a URL-safe random string carrying nBytes of entropy (the
// encoded string is longer than nBytes characters).
func randomSecret(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
