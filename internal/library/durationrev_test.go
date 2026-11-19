package library

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// headerlessSecs is the length ffmpeg decodes lame-vbr-noxing.mp3 to (metadata's
// countedFixtures).
const headerlessSecs = 5.328980

// headerlessBook is a library holding one book, a header-less VBR MP3 (lame
// -t), and a scanner with no ffprobe, not yet scanned.
func headerlessBook(t *testing.T) (*store.DB, *catalog.Catalog, *catalog.Library, *Scanner) {
	t.Helper()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	copyFixture(t, filepath.Join("..", "metadata", "testdata", "probe", "lame-vbr-noxing.mp3"), filepath.Join(root, "Author", "Book.mp3"))
	lib, err := cat.CreateLibrary(t.Context(), catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return db, cat, lib, NewScanner(cat, "", slog.Default())
}

// bookDuration is the one book's stored duration.
func bookDuration(t *testing.T, db *store.DB) float64 {
	t.Helper()
	var d float64
	if err := db.QueryRowContext(t.Context(), `SELECT duration FROM books`).Scan(&d); err != nil {
		t.Fatal(err)
	}
	return d
}

// A book holding an MP3 that an older server indexed (duration_rev 0, its VBR
// file's length a bitrate's estimate) is indexed once more by the next scan,
// its files unchanged, so the frames are counted (SRV-16); the scan after
// leaves it be.
func TestScanRereadsLengthsOfAnOlderRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, _, lib, s := headerlessBook(t)
	if res, err := s.Scan(ctx, *lib); err != nil || res.Added != 1 {
		t.Fatalf("first scan = %+v, %v", res, err)
	}
	if d := bookDuration(t, db); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 {
		t.Fatalf("duration %v, want the frames counted, %v", d, headerlessSecs)
	}

	// As an older server left it: the estimate, and no revision.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration = 5.0799, duration_rev = 0`); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 1 {
		t.Fatalf("rescan = %+v, %v; want the book read again", res, err)
	}
	if d := bookDuration(t, db); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 {
		t.Errorf("after the rescan: duration %v, want %v", d, headerlessSecs)
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
	db, cat, lib, s := headerlessBook(t)
	const estimate = 5.0799
	// The count out of time, as on a slow disk: ffprobe's estimate stands.
	s.readFile = func(ctx context.Context, root, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		md := extractFile(ctx, root, p, ffprobe, timeout)
		md.Duration = estimate
		return metadata.UncountedForTests(md)
	}
	book := func() (d float64, stale bool) {
		t.Helper()
		d = bookDuration(t, db)
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
	if d, stale := book(); d < headerlessSecs-0.001 || d > headerlessSecs+0.001 || stale {
		t.Errorf("counted: duration %v stale %t, want %v, the revision stamped", d, stale, headerlessSecs)
	}
	if res, err := s.Scan(ctx, *lib); err != nil || res.Updated != 0 {
		t.Errorf("fourth scan = %+v, %v; want the book left be", res, err)
	}
}
