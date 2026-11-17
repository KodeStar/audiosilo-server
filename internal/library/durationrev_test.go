package library

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A book holding an MP3 that an older server indexed (duration_rev 0, its VBR
// file's length a bitrate's estimate) is indexed once more by the next scan,
// its files unchanged, so the frames are counted (SRV-16); the scan after
// leaves it be.
func TestScanRereadsLengthsOfAnOlderRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	// A header-less VBR file (lame -t) and the length ffmpeg decodes it to
	// (metadata's countedFixtures).
	copyFixture(t, filepath.Join("..", "metadata", "testdata", "probe", "lame-vbr-noxing.mp3"), filepath.Join(root, "Author", "Book.mp3"))
	const secs = 5.328980
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	duration := func() float64 {
		t.Helper()
		var d float64
		if err := db.QueryRowContext(ctx, `SELECT duration FROM books`).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	if d := duration(); d < secs-0.001 || d > secs+0.001 {
		t.Fatalf("duration %v, want the frames counted, %v", d, secs)
	}

	// As an older server left it: the estimate, and no revision.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration = 5.0799, duration_rev = 0`); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan = %+v, %v; want the book read again", res, err)
	}
	if d := duration(); d < secs-0.001 || d > secs+0.001 {
		t.Errorf("after the rescan: duration %v, want %v", d, secs)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Errorf("third scan = %+v, %v; want the book left be", res, err)
	}
}

// A header-less VBR MP3 whose frames could not be counted (the count passed
// its deadline, and ffprobe's estimate was stored) leaves the book's length
// revision unstamped, so the next scan reads it again, its files unchanged;
// once that read counts the frames the revision is stamped and the scan after
// leaves the book be (SRV-16).
func TestScanRereadsAnUncountedLength(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	copyFixture(t, filepath.Join("..", "metadata", "testdata", "probe", "lame-vbr-noxing.mp3"), filepath.Join(root, "Author", "Book.mp3"))
	const secs, estimate = 5.328980, 5.0799
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	// The count out of time, as on a slow disk: ffprobe's estimate stands.
	s.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		md := extractFile(ctx, p, ffprobe, timeout)
		md.Duration = estimate
		return metadata.UncountedForTests(md)
	}
	book := func() (d float64, stale bool) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT duration FROM books`).Scan(&d); err != nil {
			t.Fatal(err)
		}
		sigs, err := cat.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d, sigs["Author"].DurationStale
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	if d, stale := book(); d != estimate || !stale {
		t.Fatalf("uncounted: duration %v stale %t, want the estimate, its revision unstamped", d, stale)
	}
	// Rescanned with the count out of time again: still the estimate, still stale.
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan, uncounted = %+v, %v; want the book read again", res, err)
	}
	if _, stale := book(); !stale {
		t.Fatal("uncounted twice: revision stamped")
	}

	s.readFile = nil
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan = %+v, %v; want the book read again", res, err)
	}
	if d, stale := book(); d < secs-0.001 || d > secs+0.001 || stale {
		t.Errorf("counted: duration %v stale %t, want %v, the revision stamped", d, stale, secs)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Errorf("fourth scan = %+v, %v; want the book left be", res, err)
	}
}
