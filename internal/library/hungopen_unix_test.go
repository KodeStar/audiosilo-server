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

// A single-file book that is a FIFO holds no scan (server#360's repro): with
// no writer its open would wait for one, with a writer that never writes its
// reads (the tag read's, the fingerprint's) would. It is no regular file,
// turned away at once, and its problem recorded.
func TestFIFOBookDoesNotHoldTheScan(t *testing.T) {
	t.Parallel()
	for _, writer := range []bool{false, true} {
		t.Run(map[bool]string{false: "no writer", true: "writer held"}[writer], func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root := t.TempDir()
			const book = "Lone Author/Lone Book.mp3"
			fifo := filepath.Join(root, filepath.FromSlash(book))
			if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(fifo, 0o644); err != nil {
				t.Skipf("no FIFO here: %v", err)
			}
			if writer {
				w, err := os.OpenFile(fifo, os.O_RDWR, 0)
				if err != nil {
					t.Skipf("can't hold the FIFO open: %v", err)
				}
				t.Cleanup(func() { _ = w.Close() })
			}
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			s.probeTimeout = time.Minute // the bound is not what ends it
			if res := scanWithin(t, s, *lib, 10*time.Second); res.Added != 1 {
				t.Errorf("added %d, want the book written with what there is", res.Added)
			}
			var problem, file string
			if err := db.QueryRowContext(ctx, `SELECT scan_error, scan_error_file FROM books WHERE library_id = ?`, lib.ID).
				Scan(&problem, &file); err != nil {
				t.Fatal(err)
			}
			// A FIFO's size is 0: it reads as an empty file.
			if problem != problemEmptyFile || file != book {
				t.Errorf("recorded problem %q on %q, want %q on the FIFO", problem, file, problemEmptyFile)
			}
		})
	}
}
