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

// A FIFO among a book's parts is a known bad file, not a read cut short
// (SRV-15b; #432 held the stored book for as long as the FIFO sat there, so a
// real change to another part was never taken): a re-read with every other
// part unchanged keeps the stored book, the FIFO's problem recorded, and a
// change to another part rewrites the book with it.
func TestFIFOPartLetsOtherChangesThrough(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Author/Book"
	silence(t, root, book+"/01.mp3", 1, "album=Real Title", "artist=Real Author")
	silence(t, root, book+"/03.mp3", 1, "album=Real Title", "artist=Real Author")
	fifo := filepath.Join(root, filepath.FromSlash(book+"/02.mp3"))
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	writer, err := os.OpenFile(fifo, os.O_RDWR, 0) // a writer that never writes
	if err != nil {
		t.Skipf("can't hold the FIFO open: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	read := func() (duration float64, problem, file string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT duration, scan_error, scan_error_file FROM books WHERE rel_path = ?`, book).
			Scan(&duration, &problem, &file); err != nil {
			t.Fatal(err)
		}
		return duration, problem, file
	}
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
		t.Fatalf("added %d, want the book", res.Added)
	}
	first, problem, file := read()
	if first < 1.5 || problem != problemEmptyFile || file != book+"/02.mp3" {
		t.Fatalf("first scan: %v s, %q on %q; want the two parts' length, the FIFO's problem", first, problem, file)
	}

	// Kept while the other parts are as stored: a re-read leaves the row (a
	// stand-in length marks it) and records the FIFO's problem again.
	if _, err := db.ExecContext(ctx, `UPDATE books SET duration = 999 WHERE rel_path = ?`, book); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReindexPath(ctx, *lib, book); err != nil {
		t.Fatal(err)
	}
	if d, problem, _ := read(); d != 999 || problem != problemEmptyFile {
		t.Errorf("re-read with the parts unchanged: %v s, %q; want the stored book kept, the problem recorded", d, problem)
	}

	// Another part changes: the book is rewritten with it.
	silence(t, root, book+"/03.mp3", 2, "album=Real Title", "artist=Real Author")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(book+"/03.mp3")), later, later); err != nil {
		t.Fatal(err)
	}
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Updated != 1 {
		t.Errorf("updated %d, want the book rewritten for its changed part", res.Updated)
	}
	if d, problem, file := read(); d < first+0.5 || d > 100 || problem != problemEmptyFile || file != book+"/02.mp3" {
		t.Errorf("after a part changed: %v s, %q on %q; want the new length, the FIFO's problem", d, problem, file)
	}
}
