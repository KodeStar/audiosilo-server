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

// BenchmarkFirstScan times a first scan: a fresh database each run, every book
// read and written. The library is AUDIOSILO_FIRSTSCAN_ROOT, or a generated one of
// AUDIOSILO_FIRSTSCAN_BOOKS folder books (200) of 3-23 small parts and a cover,
// as BenchmarkRescanNoChange makes. ffprobe is used when it is on PATH
// (AUDIOSILO_FIRSTSCAN_FFPROBE=off: without); the in-process probe follows its
// release's rules, so give it a 7.1 or 8.1 ffprobe to time the probe rather
// than ffprobe.
//
//	go test ./internal/library -run '^$' -bench BenchmarkFirstScan -benchtime 5x
func BenchmarkFirstScan(b *testing.B) {
	ctx := context.Background()
	root := os.Getenv("AUDIOSILO_FIRSTSCAN_ROOT")
	if root == "" {
		n := 200
		if v, err := strconv.Atoi(os.Getenv("AUDIOSILO_FIRSTSCAN_BOOKS")); err == nil && v > 0 {
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
	if os.Getenv("AUDIOSILO_FIRSTSCAN_FFPROBE") == "off" {
		ffprobe = ""
	}
	books := 0
	for b.Loop() {
		b.StopTimer()
		db := storetest.Open(b)
		cat := catalog.New(db, time.Now)
		s := NewScanner(cat, ffprobe, slog.New(slog.DiscardHandler))
		lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		books = res.Added
		_ = db.Close()
		b.StartTimer()
	}
	b.ReportMetric(float64(books), "books")
}
