package library

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// scanWithin is s.Scan of lib, failing the test when it hasn't finished
// within limit (left running: the scan this guards against never ends).
func scanWithin(t *testing.T, s *Scanner, lib catalog.Library, limit time.Duration) *ScanResult {
	t.Helper()
	type scanned struct {
		res *ScanResult
		err error
	}
	done := make(chan scanned, 1)
	go func() {
		res, err := s.Scan(context.WithoutCancel(t.Context()), lib)
		done <- scanned{res, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.res
	case <-time.After(limit):
		t.Fatalf("the scan of %s has not finished after %v", lib.Name, limit)
		return nil
	}
}

// One share whose opens don't answer (server#360: a frozen NFS mount blocks
// in openat before any read) no longer holds its scan for good: each open is
// given up on at the read's deadline and counted on the share's mount, then,
// with the mount's count full, the rest are refused at once. The scan finishes
// with every book recorded as unreadable, so the next scan (another library's,
// queued behind it) runs, and reads as ever. Not parallel:
// metadata.HoldOpensForTests swaps the mount lookup.
func TestHeldOpensLetTheScanFinish(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	held, healthy := t.TempDir(), t.TempDir()
	// Many more than the mount's count of hung reads holds, read four at a
	// time: past the count, at most three more are under way to be given up
	// on, and the rest are refused.
	const books = 40
	for i := range books {
		writeFile(t, held, fmt.Sprintf("Author/Book %02d/01.mp3", i), make([]byte, 1<<10))
	}
	silence(t, healthy, "Author/Book/01.mp3", 1, "album=Healthy Title", "artist=Real Author")
	libA, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Held", Root: held})
	libB, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Healthy", Root: healthy})
	s := NewScanner(cat, "", slog.Default())
	s.probeTimeout = 200 * time.Millisecond
	s.concurrency = 4

	release := metadata.HoldOpensForTests(held)
	t.Cleanup(release)
	if res := scanWithin(t, s, *libA, 20*time.Second); res.Added != books {
		t.Errorf("held share's scan added %d, want every book written with what there is", res.Added)
	}
	var unreadable, refused int
	if err := db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(scan_error = ?), 0), COALESCE(SUM(scan_error_detail = ?), 0)
		FROM books WHERE library_id = ?`, problemUnreadable, readRefusedDetail, libA.ID).Scan(&unreadable, &refused); err != nil {
		t.Fatal(err)
	}
	if unreadable != books || refused == 0 {
		t.Errorf("held share: %d unreadable, %d refused; want all %d unreadable, those past the mount's count refused", unreadable, refused, books)
	}

	scanWithin(t, s, *libB, 20*time.Second)
	var title, problem string
	if err := db.QueryRowContext(ctx, `SELECT title, scan_error FROM books WHERE library_id = ?`, libB.ID).
		Scan(&title, &problem); err != nil {
		t.Fatal(err)
	}
	if title != "Healthy Title" || problem != "" {
		t.Errorf("the healthy library beside the held share = %q, %q; want it read", title, problem)
	}
}
