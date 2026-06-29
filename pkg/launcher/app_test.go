package launcher

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ensureAdmin must create the admin from DATABASE state, not config-file
// existence — so dropping in a config.yaml before the first start can't suppress
// first-run admin creation — and it must be idempotent on later starts.
func TestEnsureAdminKeysOffDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	authSvc := auth.New(db, time.Now)
	cfg := config.Default(t.TempDir())

	if err := ensureAdmin(ctx, cfg, authSvc); err != nil {
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
	if err := ensureAdmin(ctx, cfg, authSvc); err != nil {
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
	cat := catalog.New(testDB(t), time.Now)

	cfg := &config.Config{Libraries: []config.Library{{Name: "Main", Root: filepath.Join("rel", "audiobooks")}}}
	if err := syncLibraries(ctx, cfg, cat); err != nil {
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
	if err := syncLibraries(ctx, cfg, cat); err != nil {
		t.Fatal(err)
	}
	if libs, _ := cat.ListLibraries(ctx); len(libs) != 1 {
		t.Fatalf("re-sync duplicated libraries: got %d", len(libs))
	}
}

// initialScan must warn-and-continue past a broken library root, still scanning
// the healthy ones.
func TestInitialScanWarnsAndContinues(t *testing.T) {
	ctx := context.Background()
	cat := catalog.New(testDB(t), time.Now)
	scanner := library.NewScanner(cat, "", discardLog())

	good, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	goodLib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Good", Root: good})
	// A missing root makes Scan return ErrLibraryUnavailable; initialScan must log
	// and carry on rather than abort the whole startup scan.
	cat.CreateLibrary(ctx, catalog.Library{Name: "Missing", Root: filepath.Join(t.TempDir(), "does-not-exist")})

	initialScan(ctx, cat, scanner, discardLog())

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
	authSvc := auth.New(testDB(t), time.Now)

	demo, err := authSvc.CreateDemoUser(ctx, auth.DemoUsernamePrefix+"throwaway")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		// A negative idle TTL puts the cutoff in the future, so the startup sweep
		// reaps every demo account — proving the sweep runs before the loop.
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
