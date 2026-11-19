// Package launcher holds the AudioSilo server's run loop: load config, open the
// database, wire the services, bootstrap first-run state, and serve until the
// context is cancelled. The headless `audiosilo` command is a thin wrapper over
// it. It is not an embedding API: the overrides an embedding launcher once set
// (bind, TLS mode, public URL, libraries, the URL callback; the archived desktop
// manager used them) were retired in v2.7.0, so config.yaml and the AUDIOSILO_*
// variables are the only configuration.
package launcher

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/bridge"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/logring"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/modules/builtin"
	"github.com/kodestar/audiosilo-server/internal/notify"
	"github.com/kodestar/audiosilo-server/internal/server"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/toolfetch"
	"github.com/kodestar/audiosilo-server/internal/updates"
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
	// admin and print the one-time credentials banner. true (CLI --setup): leave
	// the admin to the browser setup wizard - when no admin exists yet a
	// token-guarded /setup is enabled instead.
	Setup bool

	// metaReleases, set only by tests, is where the metadata mirror lists and
	// downloads data releases (a fake GitHub); zero means GitHub's real one.
	metaReleases releaseSource
	// listening, set only by tests, runs once the listener is up, before the
	// database opens: a long migration's stand-in.
	listening func()
	// banners, set only by tests, takes the first-run banners in place of
	// standard output.
	banners io.Writer
}

// dataFiles are the files in the data folder the server opens to write at
// start-up: the start checks each that exists. Not config.yaml: a read-only one
// (mounted so, or edited as root) starts, as before, when it names the server_id
// (one minted at start has to be saved), and only a setting saved from the
// console fails.
var dataFiles = []string{"audiosilo.db", "audiosilo.db-wal", "audiosilo.db-shm"}

// Run loads configuration from opts.DataDir, listens (answering "starting"
// until the rest is done), opens the store, wires the services, performs
// first-run bootstrap (admin + auth code banner), starts the background scan,
// and serves HTTP(S) until ctx is cancelled. It blocks until shutdown.
func Run(ctx context.Context, opts Options) (err error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	// Keep the newest lines for the admin console's Server > Logs; every line still
	// goes where it went before.
	logs := logring.NewRing(logRingSize)
	log = slog.New(logring.NewHandler(log.Handler(), logs, slog.LevelInfo))
	metadata.SetLogger(log) // the in-process probe's fallbacks, at debug

	abs, err := filepath.Abs(opts.DataDir)
	if err != nil {
		return err
	}
	// A data folder (or a database in it) the server may not write to, or a
	// config it may not read, stops the start here, saying so with the server's
	// user, the folder's owner and the fix, rather than as SQLite's "unable to
	// open database file" (server#368).
	unusable := func(err error) error { return fmt.Errorf("the data folder isn't usable: %w", err) }
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return unusable(fsaccess.Wrap(abs, true, err))
	}
	if err := fsaccess.CheckWrite(abs, dataFiles...); err != nil {
		return unusable(err)
	}

	cfg, firstRun, err := config.Load(abs)
	if fsaccess.IsPermission(err) {
		return unusable(fsaccess.Wrap(config.Path(abs), false, err))
	}
	if err != nil {
		return err
	}

	// Mint a stable per-install identity the first time (also self-heals an existing
	// install that predates server_id). Persisted to config.yaml below so it survives
	// a database rebuild; clients key their per-server state on it.
	mintedServerID := ensureServerID(cfg)

	// Persist a default config the first time (when none existed yet), or when we
	// just minted a server_id for an install that predates it: before the
	// listener, as the healthcheck reads the address there (Healthcheck).
	if firstRun || mintedServerID {
		if err := cfg.Save(); err != nil {
			// A read-only config.yaml starts only when it already names the
			// server_id: say which file refused the write (and, when the system
			// refused permission, the fix).
			return unusable(fsaccess.Wrap(config.Path(abs), true, err))
		}
	}

	// Listen now, before the database opens: a restore, the upgrade's copy, the
	// migrations and the re-keying below can take minutes on a large library,
	// and until gate opens every request (/healthz too) is answered 503
	// "starting", which the healthcheck reads as healthy, so an orchestrator
	// never restarts a start mid-migration (server#217). A port already taken
	// stops the start here, before any of it. The server stops when Run
	// returns (stopServing), after the deferred work below it: it is deferred
	// first. The "starting" answer goes out under the API's CORS policy, so a
	// player on an allowed origin reads it rather than a failed fetch.
	// The start marker (markStart) is written before the listener opens, over
	// any an earlier start left, and removed once the gate opens or the start
	// fails: a healthcheck that can only send a nameless TLS hello (autocert)
	// reads from it how long the start has run.
	mark := markStart(abs, log)
	defer mark.end()
	gate := server.NewStartGate(func(h http.Handler) http.Handler { return api.CORS(cfg, h) })
	serveCtx, stopServing := context.WithCancel(ctx)
	serving, err := server.Start(serveCtx, cfg, gate, log)
	if err != nil {
		stopServing()
		return err
	}
	defer func() {
		// Serving that stopped on its own (Done before Run's end asked it to)
		// is why the start or the run ended: its error is Run's.
		select {
		case <-serving.Done():
			if serr := serving.Wait(); serr != nil {
				err = serr
			}
		default:
		}
		stopServing()
		_ = serving.Wait()
	}()
	// A serve failure mid-start (the listener lost) ends the start there, not
	// only at its end: the rest runs under a context that ends with serving.
	ctx, endStart := context.WithCancel(ctx)
	defer endStart()
	go func() {
		select {
		case <-serving.Done():
			endStart()
		case <-ctx.Done():
		}
	}()
	if opts.listening != nil {
		opts.listening()
	}

	// A restore an admin asked for replaces the database before anything opens it.
	dbPath := filepath.Join(abs, "audiosilo.db")
	restored, err := backup.ApplyPendingRestore(ctx, abs, cfg.Backups.Dir, dbPath, log)
	if err != nil {
		return err
	}
	// An upgrade that brings new migrations copies the database first (logged), so
	// the database as the older server left it can be restored.
	_ = backup.BeforeUpgrade(ctx, abs, cfg.Backups.Dir, dbPath, log)

	db, err := store.Open(ctx, dbPath, store.WithLogger(log), store.WithReaderMmap(cfg.Database.Mmap))
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)
	if restored != nil {
		recordRestore(ctx, cat, restored, log)
	}
	renewed := renewProgressFeed(ctx, cat, abs, restored, log)
	// The feed's end, written down outside the database, tells a database that
	// went back with no restore recorded (one put in place by hand); saved a
	// last time once the work below has stopped.
	if feedMark := openProgressFeedMark(ctx, cat, abs, renewed, log); feedMark != nil {
		defer feedMark.keep(ctx)()
	}
	// Books indexed before series keys were stored (or keyed by older rules or
	// Unicode tables) get them now, before anything reads a series filter (about a
	// second per 50,000 books). Every series filter reads only the keys, so a failure
	// stops the start, as a migration's does.
	if n, err := cat.KeySeries(ctx); err != nil {
		return fmt.Errorf("keying the books' series: %w", err)
	} else if n > 0 {
		log.Info("keyed the books' series", "books", n)
	}
	// Every book's path in NFC (books.path_key, server#210), keyed for the books
	// that have none and again when the Unicode tables change, before a request
	// in another spelling of a path is answered (catalog.Spelling reads only the
	// keys). A failure stops the start, as KeySeries' does; the books keyed by
	// then stay keyed, and the next start goes on from there.
	if n, err := cat.KeyPaths(ctx, keyPathsProgress(log)); err != nil {
		return fmt.Errorf("keying the books' paths: %w", err)
	} else if n > 0 {
		log.Info("keyed the books' paths", "books", n)
	}
	// The search index's rows written before Chinese and Japanese text was set
	// apart a character a token (or with older Unicode tables) are rewritten
	// once, so a search finds a word inside such a title.
	if n, err := cat.IndexSearchText(ctx); err != nil {
		return fmt.Errorf("rewriting the search index: %w", err)
	} else if n > 0 {
		log.Info("rewrote the search index's Chinese and Japanese text", "books", n)
	}

	// Notifications: the event feed and its deliveries. Messages name the server as
	// the API's live settings have it (a is set before Run starts sending).
	var a *api.API
	ntf := notify.New(cat, api.Version, func() notify.Server { return a.NotifyIdentity() }, log)

	ffmpeg, ffprobe := resolveTools(ctx, abs, opts, log)
	scanner := library.NewScanner(cat, ffprobe, log)
	// The library folders kept open for the next read are closed as the
	// server stops (an embedding app's process lives on).
	defer metadata.CloseFolders()
	scanner.OnRunFinished = rekeyAfterScan(ctx, cat, log, ntf.ScanFinished)
	cat.SetPodcastNotice(ntf.PodcastListener())
	scanner.SetConcurrency(cfg.Scan.Concurrency)

	// First-run bootstrap - two mutually-exclusive paths:
	//   - default (headless/docker): auto-create the admin + print the one-time
	//     credentials banner, keyed off the database (not config-file existence).
	//   - setup mode (desktop / --setup): leave the admin to the browser wizard;
	//     when none exists yet, mint a one-time setup token and enable /setup.
	// Either way, a later start while the admin the banner named has never
	// signed in says so again, with the way to a new password (server#369).
	//
	// Each banner names an address on this server, so it is printed once the
	// gate opens (a link followed at once would be answered "starting"): the
	// two written here are held until then, or until a start that fails
	// returns, as the credentials banner is the only place the admin's
	// password is ever printed.
	stdout := opts.banners
	if stdout == nil {
		stdout = os.Stdout
	}
	banners := &heldWriter{w: stdout}
	defer banners.release()
	var setupToken string
	created := false
	if opts.Setup {
		exists, err := authSvc.AdminExists(ctx)
		if err != nil {
			return err
		}
		if !exists {
			setupToken = randomSecret(18)
		}
	} else if created, err = ensureAdmin(ctx, cfg, authSvc, banners, log); err != nil {
		return err
	}
	if !created {
		signInHint(ctx, cfg, authSvc, banners, log)
	}

	// Sync libraries declared in config into the database, then scan them in the
	// background so the filesystem view is available immediately.
	if err := syncLibraries(ctx, cfg, cat, log); err != nil {
		return err
	}
	// setup.skip: the console's first-run flow is marked finished, after the
	// config's libraries exist so its folder step reads as done.
	if cfg.Setup.Skip {
		if skipped, err := cat.SkipSetup(ctx); err != nil {
			log.Warn("skipping the first-run setup failed", "err", err)
		} else if skipped {
			log.Info("first-run setup marked finished (setup.skip)")
		}
	}
	// Listeners' rows stored under a track path, a disc folder or another
	// spelling move to the book's path, each logged whole first (server#210):
	// after KeyPaths (it resolves spellings by the keys) and after the config's
	// libraries are synced (Spelling looks at each library's root as it is now),
	// once per key version, before the scanner starts; a library whose folder
	// doesn't answer now (the scanner's root check) is left for the next start.
	rekeyAtStart(ctx, cat, rootAnswers(ctx, cat, scanner, log), log)
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
	backups.OnFailure = func(r backup.Result) { ntf.BackupFailed(ctx, r.Trigger, r.Error, r.Detail) }
	go func() {
		// Off the start path: the folder may be a network share slow to answer.
		if err := backups.CheckFolder(); err != nil {
			log.Warn("backups will fail", "err", err)
		}
	}()
	go backups.Run(ctx)

	a = api.New(cfg, authSvc, cat, scanner, ffmpeg, log)
	// The Bridge's status and job queue (BRIDGE-PLAN.md): the server keeps them;
	// the Bridge calls in.
	a.SetBridge(bridge.New(db, time.Now))
	// The managed modules (Settings > Modules); a registry that doesn't check out
	// is a programming error, so the server refuses to start.
	if err := a.SetModules(builtin.Modules()); err != nil {
		return fmt.Errorf("modules: %w", err)
	}
	// Mirror mode's local copy of the community metadata (nil in remote mode),
	// in front of the metadata service before the background jobs below use it.
	// Its checks stop while the live metadata.enabled is off.
	mirror := metaMirror(cfg, abs, a.MetadataOn, opts.metaReleases, log)
	a.SetMetaMirror(mirror)
	stopMirror := runMetaMirror(ctx, mirror, log)
	defer stopMirror()
	// Bulk match runs a stopped server left working: matching ones are
	// interrupted, applying ones go back to ready (what they applied is marked).
	// A failure here only warns: the first Start, Apply, Stop or refused Unlink
	// settles them too (matchrun.Runner).
	if err := cat.InterruptMatchRuns(ctx); err != nil {
		log.Warn("settle interrupted match runs failed", "err", err)
	}
	// Listening imports a stopped server left: a fetch fails (its token died with
	// the process), an apply goes back to review (it is one transaction).
	if err := cat.InterruptImports(ctx); err != nil {
		log.Warn("settle interrupted imports failed", "err", err)
	}
	// Books indexed before a series' title gave its untagged books their
	// position get it now, once (a scan leaves unchanged books alone); in the
	// background, beside the startup scan, so the listener doesn't wait on it.
	go func() {
		if n, err := cat.PlaceByTitles(ctx); err != nil && ctx.Err() == nil {
			log.Warn("placing books by their titles failed", "err", err)
		} else if n > 0 {
			log.Info("placed books in their series by their titles", "books", n)
		}
	}()
	go retention(ctx, cat, authSvc, a.SessionRetention, log)
	// Bind work detached from a request (a book's re-read, a remux) to the
	// server's lifetime: ended with Run even when the server fails while ctx
	// lives on (an embedding app's), and waited for, so no ffmpeg outlives it.
	baseCtx, stopBase := context.WithCancel(ctx)
	a.SetBaseContext(baseCtx)
	defer func() {
		stopBase()
		a.WaitMediaCaches()
	}()
	a.StartSourceResolves()
	a.StartChapterChecks(ctx)
	a.StartModules(ctx)
	// Before the database closes, and even when the server fails while ctx
	// lives on (an embedding app's).
	defer a.StopModules()
	a.StartCoverColors(ctx)
	a.SetRuntime(api.Runtime{FFprobe: ffprobe, Logs: logs, Updates: upd, Backups: backups, Notify: ntf})
	// Upcoming releases: after SetRuntime, as its announcements go to ntf.
	a.StartUpcoming(ctx)
	// The opt-in unknown-ASIN report, after SetMetaMirror (its lookups go to
	// the local copy in mirror mode).
	a.StartCoverage(ctx)
	ntf.Run(ctx)
	if setupToken != "" {
		a.EnableSetup(setupToken)
	}

	gate.Open(a.Handler())
	mark.end()
	// The banners' links answer from here on.
	banners.release()
	if setupToken != "" {
		setupBanner(stdout, cfg, setupToken)
	}
	return serving.Wait()
}

// heldWriter keeps what is written to it until release, which writes it to w;
// later writes go straight there.
type heldWriter struct {
	w        io.Writer
	held     bytes.Buffer
	released bool
}

func (h *heldWriter) Write(p []byte) (int, error) {
	if h.released {
		return h.w.Write(p)
	}
	return h.held.Write(p)
}

// release writes what was held, once: a second call writes nothing.
func (h *heldWriter) release() {
	if h.released {
		return
	}
	h.released = true
	_, _ = h.w.Write(h.held.Bytes())
	h.held.Reset()
}

// logRingSize is how many log lines the admin console's log viewer keeps.
const logRingSize = 2000

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
	if unspecifiedHost(host) {
		host = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

// setupBanner prints where to finish first-run setup (the token rides in the URL
// fragment, so it never reaches request logs).
func setupBanner(w io.Writer, cfg *config.Config, token string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\n========================================================\n")
	p(" AudioSilo first-run setup\n")
	p("========================================================\n")
	p(" Open this URL in your browser to finish setting up:\n")
	p("   %s/setup#token=%s\n", baseURL(cfg), token)
	p("--------------------------------------------------------\n")
	p(" You'll choose an admin password, then the console walks you through the rest.\n")
	p("========================================================\n")
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
		log.Warn("ffprobe unavailable; durations and chapters come from the in-process probe alone, so a file it is unsure of has none")
	}
	if opts.FFmpegPath != "" && ffmpeg == "" {
		log.Warn("ffmpeg unavailable; on-the-fly transcoding is disabled")
	}
	switch rel := metadata.ProbeRelease(ctx, ffprobe); {
	case ffprobe == "":
		log.Info("in-process probe on, with no ffprobe: a file it is unsure of gets no duration", "rules", rel)
	case rel != "":
		log.Info("in-process probe on", "ffprobe", ffprobe, "rules", rel)
	default:
		log.Info("in-process probe off: ffprobe is not a release it knows, so ffprobe reads every file", "ffprobe", ffprobe)
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
func ensureAdmin(ctx context.Context, cfg *config.Config, authSvc *auth.Service, w io.Writer, log *slog.Logger) (created bool, err error) {
	exists, err := authSvc.AdminExists(ctx)
	if err != nil || exists {
		return false, err
	}
	password := randomSecret(18)
	admin, err := authSvc.CreateUser(ctx, "admin", password, auth.RoleAdmin)
	if err != nil {
		return false, err
	}
	code, err := authSvc.CreateAuthCode(ctx, admin.ID, auth.InitialAdminCodeLabel, 0, 0)
	if err != nil {
		// The admin exists and its password was printed nowhere: record it
		// all the same, so the next start points at reset-admin (signInHint)
		// rather than saying nothing about an account nobody can sign in to.
		if markErr := authSvc.MarkFirstAdmin(ctx, admin.ID, ""); markErr != nil {
			log.Warn("recording the first-run admin failed: later starts won't repeat the sign-in hint", "err", markErr)
		}
		return false, err
	}
	writeBanner(w, cfg, password, code)
	// Until an admin signs in with a password, later starts say the password
	// was printed here (signInHint); the row names the code printed beside it,
	// which the admin's new password ends. After the banner, and never a
	// reason not to start: the admin exists now, and a start that stopped here
	// would leave its password printed nowhere.
	if err := authSvc.MarkFirstAdmin(ctx, admin.ID, code); err != nil {
		log.Warn("recording the first-run admin failed: later starts won't repeat the sign-in hint", "err", err)
	}
	return true, nil
}

// writeBanner writes the first-run credentials banner: the console first (where
// the "Set up your server" flow lives, named plainly under setup.skip, which
// finishes the flow later in this start), then the connect page for a device.
func writeBanner(w io.Writer, cfg *config.Config, password, code string) {
	base := baseURL(cfg)
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\n========================================================\n")
	p(" AudioSilo first-run setup - store these now, shown once\n")
	p("========================================================\n")
	p("  Admin username : admin\n")
	p("  Admin password : %s\n", password)
	p("  Auth code      : %s\n", code)
	p("  Config file    : %s\n", config.Path(cfg.DataDir))
	p("--------------------------------------------------------\n")
	console := "Finish setting up"
	if cfg.Setup.Skip {
		console = "Admin console"
	}
	p(" %s: open %s/admin and sign in as admin.\n", console, base)
	p(" Connect a device: open %s/ and enter the auth code.\n", base)
	p(" (API: POST /api/v1/auth/redeem or /api/v1/auth/login.)\n")
	p("========================================================\n")
}

// syncLibraries upserts config-declared libraries into the catalog.
func syncLibraries(ctx context.Context, cfg *config.Config, cat *catalog.Catalog, log *slog.Logger) error {
	for _, l := range cfg.Libraries {
		root, err := filepath.Abs(l.Root)
		if err != nil {
			return err
		}
		if _, err := cat.UpsertLibraryByName(ctx, catalog.Library{
			Name: l.Name, Root: root,
		}); err != nil {
			if errors.Is(err, catalog.ErrRootOverlaps) {
				log.Warn("config library root overlaps a podcast library; keeping the existing library if present", "library", l.Name, "root", root, "err", err)
				continue
			}
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

// renewProgressFeed renews the Bridge's progress feed (catalog.RenewProgressFeed)
// once for the last backup restored into dataDir, and says whether it did: the
// backup's feed is behind the cursor the Bridge holds, which must be refused
// rather than read as a place in it (bridge#50). The restore is restored when
// this start applied or refused one, else the one restore-result.json names:
// the marker is gone before the database opens (kept, the next start would
// swap the backup in again over what this one did), so a start that ended
// between the two (the backup's migrations failing, a stop while they ran) or
// a renewal that failed leaves the renewal to the next start, and the database
// records which restore it renewed for. A failure is logged and the start goes
// on (a cursor past the feed's end is refused either way).
func renewProgressFeed(ctx context.Context, cat *catalog.Catalog, dataDir string, restored *backup.RestoreResult, log *slog.Logger) bool {
	last := restored
	if last == nil {
		var err error
		if last, err = backup.LastRestore(dataDir); err != nil {
			log.Warn("reading the last restore's outcome failed; the Bridge's progress feed keeps its id", "err", err)
			return false
		}
	}
	if last == nil || !last.OK {
		return false
	}
	renewed, err := cat.RenewProgressFeed(ctx, last.AppliedAt.UTC().Format(time.RFC3339Nano))
	switch {
	case err != nil:
		log.Warn("renewing the Bridge's progress feed after the restore failed; the next start tries again", "backup", last.Name, "err", err)
	case renewed:
		log.Info("renewed the Bridge's progress feed after the restore", "backup", last.Name)
	}
	return renewed
}

// retention, once at startup and then daily until ctx is cancelled: rolls
// listening sessions older than sessions() (Settings > General, read at each run)
// up into per-day totals (dropping their device, app and time of day), blanks the
// address of signed-out devices, deletes dead cast tokens (expired, or their
// sender signed out) with the book each was for and spent or expired pairing
// tokens, drops audit events, feed events and the notification outbox past
// their retention, deletes listening sync events past catalog.SyncRetention,
// and trims the community metadata cache to its newest rows.
func retention(ctx context.Context, cat *catalog.Catalog, authSvc *auth.Service, sessions func() time.Duration, log *slog.Logger) {
	prune := func() {
		now := time.Now()
		if _, err := cat.PruneAudit(ctx, now.Add(-catalog.AuditRetention)); err != nil && ctx.Err() == nil {
			log.Warn("audit log retention failed", "err", err)
		}
		if _, err := cat.PruneServerEvents(ctx, now.Add(-catalog.ServerEventRetention)); err != nil && ctx.Err() == nil {
			log.Warn("event feed retention failed", "err", err)
		}
		if _, err := cat.PruneNotifyOutbox(ctx, now.Add(-catalog.NotifyOutboxRetention)); err != nil && ctx.Err() == nil {
			log.Warn("notification outbox retention failed", "err", err)
		}
		if err := authSvc.ForgetRevokedAddresses(ctx); err != nil && ctx.Err() == nil {
			log.Warn("forgetting signed-out device addresses failed", "err", err)
		}
		if err := authSvc.PruneCastTokens(ctx); err != nil && ctx.Err() == nil {
			log.Warn("cast token retention failed", "err", err)
		}
		if _, err := authSvc.PruneSingleUseTokens(ctx); err != nil && ctx.Err() == nil {
			log.Warn("single-use token retention failed", "err", err)
		}
		if _, err := cat.PruneMetaCache(ctx, catalog.MetaCacheRows, catalog.MetaCacheWorkRows); err != nil && ctx.Err() == nil {
			log.Warn("meta cache retention failed", "err", err)
		}
		// Sync v2's event log keeps 120 days (catalog.SyncRetention): longer than
		// the 90 a sync accepts, so a replay can't apply twice.
		if _, err := cat.PruneListeningEvents(ctx, now.Add(-catalog.SyncRetention)); err != nil && ctx.Err() == nil {
			log.Warn("listening event retention failed", "err", err)
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
