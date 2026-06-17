// Command audiosilo runs the AudioSilo audiobook server. It exposes a JSON API
// (no bundled web UI) designed to be safe to expose to the internet.
//
// On first run it generates an admin account and an auth code, printing both to
// stdout exactly once. Configuration lives in <data>/config.yaml.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/kodestar/audiosilo-server/internal/api"
	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/server"
	"github.com/kodestar/audiosilo-server/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	dataDir := flag.String("data", "./data", "data directory (config, database, certs)")
	ffprobePath := flag.String("ffprobe", "ffprobe", "path to ffprobe binary (\"\" to disable)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	abs, err := filepath.Abs(*dataDir)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, filepath.Join(abs, "audiosilo.db"))
	if err != nil {
		return err
	}
	defer db.Close()

	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)

	ffprobe := *ffprobePath
	if ffprobe != "" && !metadata.HasFFprobe(ffprobe) {
		log.Warn("ffprobe not found; durations and chapters will be unavailable", "path", ffprobe)
		ffprobe = ""
	}
	scanner := library.NewScanner(cat, ffprobe, log)

	if firstRun {
		if err := bootstrap(ctx, cfg, authSvc, log); err != nil {
			return err
		}
	}

	// Sync libraries declared in config into the database, then scan them in the
	// background so the filesystem view is available immediately.
	if err := syncLibraries(ctx, cfg, cat); err != nil {
		return err
	}
	go initialScan(ctx, cat, scanner, log)

	a := api.New(cfg, authSvc, cat, scanner, log)
	return server.Run(ctx, cfg, a.Handler(), log)
}

// bootstrap creates the admin account + an auth code on first run, prints the
// credentials once, and persists the config file.
func bootstrap(ctx context.Context, cfg *config.Config, authSvc *auth.Service, log *slog.Logger) error {
	exists, err := authSvc.AdminExists(ctx)
	if err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
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
			Name: l.Name, Root: root, Layout: l.Layout,
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

// randomSecret returns a URL-safe random string with at least nBytes of entropy.
func randomSecret(nBytes int) string {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}
