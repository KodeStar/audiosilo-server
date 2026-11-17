package library

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
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
