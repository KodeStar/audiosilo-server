//go:build unix

package library

import (
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A book of an older length revision kept as stored for a part that reads the
// same every time (a FIFO, server#432) records today's revision when it is
// kept: a read for the revision would only keep it again, so the next scan
// leaves it be rather than reading it on every run (server#452).
func TestStaleBookKeptForAFIFORecordsTheRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	copyFixture(t, filepath.Join("..", "metadata", "testdata", "probe", "lame-vbr-noxing.mp3"), filepath.Join(root, "Author", "Book", "01.mp3"))
	fifo := filepath.Join(root, "Author", "Book", "02.mp3")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	writer, err := os.OpenFile(fifo, os.O_RDWR, 0) // a writer that never writes
	if err != nil {
		t.Skipf("can't hold the FIFO open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, "", slog.Default())
	c := &readCounter{reads: map[string]int{}, cut: map[string]bool{}}
	s.readFile = c.read
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
		t.Fatalf("added %d, want the book", res.Added)
	}
	// As an older server left it: no revision, a stand-in length.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration_rev = 0, duration = 999`); err != nil {
		t.Fatal(err)
	}
	scanWithin(t, s, *lib, 20*time.Second)
	var d float64
	if err := db.QueryRowContext(ctx, `SELECT duration FROM books`).Scan(&d); err != nil || d != 999 {
		t.Fatalf("duration %v, %v; want the stored book kept for its FIFO", d, err)
	}
	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil || sigs["Author/Book"].DurationStale {
		t.Fatalf("kept for its FIFO: stale %t, %v; want the revision recorded", sigs["Author/Book"].DurationStale, err)
	}
	reads := c.count("01.mp3")
	scanWithin(t, s, *lib, 20*time.Second)
	if n := c.count("01.mp3") - reads; n != 0 {
		t.Errorf("the next scan read the book's other part %d times, want it left be", n)
	}
}
