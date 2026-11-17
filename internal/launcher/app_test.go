package launcher

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/backup"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/fsaccess"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ensureAdmin must create the admin from DATABASE state, not config-file
// existence - so dropping in a config.yaml before the first start can't suppress
// first-run admin creation - and it must be idempotent on later starts.
func TestEnsureAdminKeysOffDatabase(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	authSvc := auth.New(db, time.Now)
	cfg := config.Default(t.TempDir())

	if _, err := ensureAdmin(ctx, cfg, authSvc, io.Discard, discardLog()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := authSvc.AdminExists(ctx); !ok {
		t.Fatal("expected an admin to be created on first call")
	}
	users, _ := authSvc.ListUsers(ctx)
	if len(users) != 1 {
		t.Fatalf("expected exactly 1 user after bootstrap, got %d", len(users))
	}

	// Idempotent: a second call (e.g. a normal restart) must not create another.
	if _, err := ensureAdmin(ctx, cfg, authSvc, io.Discard, discardLog()); err != nil {
		t.Fatal(err)
	}
	if users, _ := authSvc.ListUsers(ctx); len(users) != 1 {
		t.Fatalf("ensureAdmin should be idempotent, got %d users", len(users))
	}
}

// TestRandomSecret pins randomSecret's entropy and encoding: it carries exactly
// nBytes of decoded entropy, is URL-safe (no '+', '/' or '=' padding), and is
// unpredictable (two calls differ).
func TestRandomSecret(t *testing.T) {
	for _, n := range []int{1, 16, 32} {
		s := randomSecret(n)

		decoded, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("randomSecret(%d) = %q does not base64url-decode: %v", n, s, err)
		}
		if len(decoded) != n {
			t.Fatalf("randomSecret(%d) decoded to %d bytes, want %d", n, len(decoded), n)
		}
		if strings.ContainsAny(s, "+/=") {
			t.Fatalf("randomSecret(%d) = %q contains non-URL-safe characters", n, s)
		}
	}

	// Two calls must not collide (vanishingly unlikely for crypto/rand entropy).
	if a, b := randomSecret(32), randomSecret(32); a == b {
		t.Fatalf("two randomSecret calls returned the same value %q", a)
	}
}

// TestEnsureServerID covers minting once and self-healing: an empty ServerID is
// filled and reported minted; an existing one is preserved and reported not-minted
// (so the caller doesn't needlessly rewrite config, and the id never changes).
func TestEnsureServerID(t *testing.T) {
	cfg := config.Default(t.TempDir())
	if !ensureServerID(cfg) || cfg.ServerID == "" {
		t.Fatalf("first call must mint a non-empty id, got %q", cfg.ServerID)
	}
	id := cfg.ServerID
	if strings.ContainsAny(id, "+/=") {
		t.Fatalf("server id %q must be URL-safe (it becomes a route segment + dir name)", id)
	}
	if ensureServerID(cfg) {
		t.Fatal("second call must not re-mint an existing id")
	}
	if cfg.ServerID != id {
		t.Fatalf("existing server id must be preserved, got %q want %q", cfg.ServerID, id)
	}
}

// TestResolveTool covers the bundled-ffmpeg lookup: empty/explicit paths pass
// through; a bare name resolves to a tool sitting next to the executable, else
// falls back to the bare name (PATH lookup happens later).
func TestResolveTool(t *testing.T) {
	if got := resolveTool(""); got != "" {
		t.Errorf(`resolveTool("") = %q, want ""`, got)
	}
	explicit := filepath.Join(string(os.PathSeparator)+"usr", "bin", "ffmpeg")
	if got := resolveTool(explicit); got != explicit {
		t.Errorf("resolveTool(explicit) = %q, want unchanged %q", got, explicit)
	}
	if got := resolveTool("definitely-not-bundled-xyz"); got != "definitely-not-bundled-xyz" {
		t.Errorf("resolveTool(bare, none bundled) = %q, want the bare name", got)
	}

	// A bare name that exists next to the executable resolves to it.
	exe, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable")
	}
	name := "as-test-tool"
	cand := filepath.Join(filepath.Dir(exe), name)
	if runtime.GOOS == "windows" {
		cand += ".exe"
	}
	if err := os.WriteFile(cand, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Skipf("cannot write next to test binary: %v", err)
	}
	defer os.Remove(cand)
	if got := resolveTool(name); got != cand {
		t.Errorf("resolveTool(bundled bare) = %q, want %q", got, cand)
	}
}

// syncLibraries must resolve each config root to an absolute path and upsert by
// name, so re-running it doesn't duplicate libraries.
func TestSyncLibraries(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(storetest.Open(t), time.Now)

	cfg := &config.Config{Libraries: []config.Library{{Name: "Main", Root: filepath.Join("rel", "audiobooks")}}}
	if err := syncLibraries(ctx, cfg, cat, discardLog()); err != nil {
		t.Fatal(err)
	}

	libs, err := cat.ListLibraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(libs) != 1 {
		t.Fatalf("want 1 library, got %d", len(libs))
	}
	wantAbs, _ := filepath.Abs(filepath.Join("rel", "audiobooks"))
	if libs[0].Root != wantAbs {
		t.Fatalf("root = %q, want absolute %q", libs[0].Root, wantAbs)
	}

	// Idempotent: a second sync upserts by name rather than duplicating.
	if err := syncLibraries(ctx, cfg, cat, discardLog()); err != nil {
		t.Fatal(err)
	}
	if libs, _ := cat.ListLibraries(ctx); len(libs) != 1 {
		t.Fatalf("re-sync duplicated libraries: got %d", len(libs))
	}
}

func TestSyncLibrariesContinuesAfterRootOverlap(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			cat := catalog.New(storetest.Open(t), time.Now)
			root := t.TempDir()
			podcasts, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Podcasts", Root: filepath.Join(root, "podcasts"), Kind: catalog.KindPodcasts})
			if err != nil {
				t.Fatal(err)
			}
			oldRoot := filepath.Join(root, "books")
			if existing {
				if _, err := cat.CreateLibrary(ctx, catalog.Library{Name: "Clash", Root: oldRoot}); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{Libraries: []config.Library{
				{Name: "Clash", Root: filepath.Join(podcasts.Root, "nested")},
				{Name: "Healthy", Root: filepath.Join(root, "healthy")},
			}}
			var logs bytes.Buffer
			if err := syncLibraries(ctx, cfg, cat, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
				t.Fatalf("startup stopped at overlapping config library: %v", err)
			}
			if !strings.Contains(logs.String(), "Clash") || !strings.Contains(logs.String(), "overlaps") {
				t.Fatalf("missing overlap warning: %s", logs.String())
			}
			if _, err := cat.GetLibraryByName(ctx, "Healthy"); err != nil {
				t.Fatalf("later library was not synced: %v", err)
			}
			clash, err := cat.GetLibraryByName(ctx, "Clash")
			if existing {
				if err != nil || clash.Root != oldRoot {
					t.Fatalf("existing library changed: %+v %v", clash, err)
				}
			} else if !errors.Is(err, catalog.ErrNotFound) {
				t.Fatalf("overlapping library was created: %+v %v", clash, err)
			}
		})
	}
}

// The startup scans carry on past a broken library root, still scanning the
// healthy ones, and record each outcome.
func TestStartupScansWarnAndContinue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cat := catalog.New(storetest.Open(t), time.Now)
	scanner := library.NewScanner(cat, "", discardLog())

	good, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	// A missing root makes its scan stop at the unavailable-root guard; the queue
	// must record that and carry on rather than abort the startup scans.
	missing, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Missing", Root: filepath.Join(t.TempDir(), "does-not-exist")})
	goodLib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Good", Root: good})

	scanner.Start(ctx)
	if _, err := scanner.EnqueueAll(ctx, library.TriggerStartup, nil); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		runs, _ := cat.ListScanRuns(ctx, 0, 0, 10)
		if len(runs) == 2 && runs[0].FinishedAt != nil && runs[1].FinishedAt != nil {
			status := map[int64]string{runs[0].LibraryID: runs[0].Status, runs[1].LibraryID: runs[1].Status}
			if status[missing.ID] != catalog.RunUnavailable || status[goodLib.ID] != catalog.RunOK {
				t.Fatalf("statuses = %v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the startup scans never finished: %+v", runs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	counts, err := cat.CountBooksByLibrary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[goodLib.ID] == 0 {
		t.Fatalf("the healthy library should be scanned despite the broken one: %+v", counts)
	}
}

// demoReaper must sweep idle demo accounts once at startup and then exit promptly
// when its context is cancelled.
func TestDemoReaperSweepsThenExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	authSvc := auth.New(storetest.Open(t), time.Now)

	demo, err := authSvc.CreateDemoUser(ctx, auth.DemoUsernamePrefix+"throwaway")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		// A negative idle TTL puts the cutoff in the future, so the startup sweep
		// reaps every demo account - proving the sweep runs before the loop.
		demoReaper(ctx, authSvc, -time.Minute, discardLog())
		close(done)
	}()

	// The startup sweep should remove the idle demo user.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := authSvc.GetUser(ctx, demo.ID); err != nil {
			break // reaped
		}
		if time.Now().After(deadline) {
			t.Fatal("demoReaper startup sweep did not reap the idle demo user")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("demoReaper did not exit after ctx cancel")
	}
}

// A restore applied at start is in the audit log of the database it put in place,
// as the server's own act; a refused one too, with why.
func TestRecordRestore(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	recordRestore(ctx, cat, &backup.RestoreResult{Name: "audiosilo-a.db", RequestedBy: "chris", OK: true, SafetyCopy: "audiosilo-b.db"}, slog.Default())
	recordRestore(ctx, cat, &backup.RestoreResult{Name: "audiosilo-c.db", RequestedBy: "chris", Error: "newer"}, slog.Default())
	events, _, err := cat.ListAudit(ctx, catalog.AuditFilter{})
	if err != nil || len(events) != 2 {
		t.Fatalf("audit = %+v, %v", events, err)
	}
	failed, applied := events[0], events[1]
	if applied.Action != "backup.restore_applied" || applied.Via != catalog.ViaSystem || applied.ActorID != nil ||
		applied.Details["safety_copy"] != "audiosilo-b.db" || applied.Details["requested_by"] != "chris" {
		t.Fatalf("applied = %+v", applied)
	}
	if failed.Action != "backup.restore_failed" || failed.Details["error"] != "newer" {
		t.Fatalf("failed = %+v", failed)
	}
}

// TestBannerNamesConsoleAndConnectPage (CONNECT-05): the first-run banner sends
// a new admin to the console (where "Set up your server" lives) and names the
// connect page for a device, with the credentials it shows once.
func TestBannerNamesConsoleAndConnectPage(t *testing.T) {
	cfg := config.Default(t.TempDir())
	cfg.PublicURL = "https://books.example.com/"
	var out bytes.Buffer
	writeBanner(&out, cfg, "s3cret-pass", "CODE-1234")
	got := out.String()
	for _, want := range []string{
		"open https://books.example.com/admin and sign in as admin",
		"open https://books.example.com/ and enter the auth code",
		"Admin password : s3cret-pass",
		"Auth code      : CODE-1234",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("banner lacks %q:\n%s", want, got)
		}
	}
}

// A data folder the server may not write to, or a database in it the server may
// not open to write, stops the start with the folder, the server's user and the
// fix, before SQLite's "unable to open database file" (server#368). A writable
// one starts: TestRunMirrorMode.
func TestRunRefusesAnUnwritableDataFolder(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes don't refuse this user")
	}
	ctx := context.Background()

	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	err := Run(ctx, Options{DataDir: ro, Log: discardLog()})
	want := "the data folder isn't usable: the server can't write to " + ro + ": permission denied. The server runs as user " + fsaccess.User()
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("read-only folder: err = %v, want it to start %q", err, want)
	}

	data := t.TempDir()
	db := filepath.Join(data, "audiosilo.db")
	if err := os.WriteFile(db, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Options{DataDir: data, Log: discardLog()})
	if err == nil || !strings.Contains(err.Error(), "the server can't write to "+db+": permission denied") {
		t.Fatalf("read-only database: err = %v", err)
	}
	if _, statErr := os.Stat(config.Path(data)); statErr == nil {
		t.Fatal("a config was written before the check")
	}

	// A read-only config that names no server_id can't take the one minted for it.
	noID := t.TempDir()
	if err := os.WriteFile(config.Path(noID), []byte("bind: 127.0.0.1:0\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Options{DataDir: noID, Log: discardLog()})
	if err == nil || !strings.Contains(err.Error(), "the data folder isn't usable: the server can't write to "+config.Path(noID)+": permission denied. The server runs as user") {
		t.Fatalf("read-only config without a server_id: err = %v", err)
	}

	// A config the server may not read is named the same way.
	locked := t.TempDir()
	if err := os.WriteFile(config.Path(locked), []byte("bind: 127.0.0.1:0\n"), 0o200); err != nil {
		t.Fatal(err)
	}
	err = Run(ctx, Options{DataDir: locked, Log: discardLog()})
	if err == nil || !strings.Contains(err.Error(), "the server can't read "+config.Path(locked)+": permission denied. The server runs as user") {
		t.Fatalf("unreadable config: err = %v", err)
	}
}

// A read-only config.yaml (mounted so, or edited as root) still starts: only
// a setting saved from the console needs to write it.
func TestRunStartsWithAReadOnlyConfig(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes don't refuse this user")
	}
	data := t.TempDir()
	addr := freeAddr(t)
	cfgYAML := fmt.Sprintf("server_id: %q\nbind: %q\ntls:\n  mode: \"off\"\nupdate_check: false\n", "srv_readonly_config_test", addr)
	if err := os.WriteFile(config.Path(data), []byte(cfgYAML), 0o400); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{DataDir: data, Log: discardLog()}) }()
	waitFor(t, "the server", func() bool {
		resp, err := http.Get("http://" + addr + "/api/v1/healthz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
}
