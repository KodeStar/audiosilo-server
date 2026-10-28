package library

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// BenchmarkRescanNoChange times a scan of a library nothing changed in since its
// last scan (the benchmark harness's no-change rescan), with ffprobe when it is
// on PATH (AUDIOSILO_RESCAN_FFPROBE=off: without), AUDIOSILO_RESCAN_PAUSE apart. The library is AUDIOSILO_RESCAN_ROOT, or a generated one of
// AUDIOSILO_RESCAN_BOOKS folder books (200) of 3-23 small parts and a cover.
//
//	go test ./internal/library -run '^$' -bench BenchmarkRescanNoChange -benchtime 5x
func BenchmarkRescanNoChange(b *testing.B) {
	ctx := context.Background()
	root := os.Getenv("AUDIOSILO_RESCAN_ROOT")
	if root == "" {
		n := 200
		if v, err := strconv.Atoi(os.Getenv("AUDIOSILO_RESCAN_BOOKS")); err == nil && v > 0 {
			n = v
		}
		root = b.TempDir()
		part, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
		if err != nil {
			b.Fatal(err)
		}
		for i := range n {
			dir := filepath.Join(root, fmt.Sprintf("Author %03d", i/7), fmt.Sprintf("Book %05d", i))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				b.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "cover.jpg"), []byte("jpg"), 0o644); err != nil {
				b.Fatal(err)
			}
			for f := range 3 + i%21 {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d - Part.m4b", f+1)), part, 0o644); err != nil {
					b.Fatal(err)
				}
			}
		}
	}
	ffprobe, _ := exec.LookPath("ffprobe")
	if os.Getenv("AUDIOSILO_RESCAN_FFPROBE") == "off" {
		ffprobe = ""
	}
	db := storetest.Open(b)
	cat := catalog.New(db, time.Now)
	s := NewScanner(cat, ffprobe, slog.New(slog.DiscardHandler))
	s.SetConcurrency(8)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		b.Fatal(err)
	}
	// The first scan indexes; the second settles any backfill.
	for range 2 {
		if _, err := s.Scan(ctx, *lib); err != nil {
			b.Fatal(err)
		}
	}
	// A pause between scans lets a FUSE mount's attribute cache (a second by
	// default) expire, as it has between real scans.
	pause, _ := time.ParseDuration(os.Getenv("AUDIOSILO_RESCAN_PAUSE"))
	for b.Loop() {
		if pause > 0 {
			b.StopTimer()
			time.Sleep(pause)
			b.StartTimer()
		}
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			b.Fatal(err)
		}
		if res.Added+res.Updated+res.Removed+res.Moved != 0 {
			b.Fatalf("not a no-change scan: %+v", res.ScanCounts)
		}
	}
}
