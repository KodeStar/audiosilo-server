package library

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// SafeJoin's Lstats of the names below the root on a share that stopped
// answering (SRV-15c: one stream sat 71 s in them before its open's bound
// began) are given up on at the bound, as a look given up on
// (metadata.GaveUp), and answer as SafeJoin's once the share does. A path out
// of the root is refused without a look, and so is a hidden one by
// ContentJoinBounded. Not parallel: metadata.HoldOpensForTests swaps the mount
// lookup.
func TestSafeJoinBoundedGivesUpOnAHeldShare(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "Book/01.mp3", []byte("audio"))
	release := metadata.HoldOpensForTests(filepath.Join(root, "Book"))
	t.Cleanup(release)

	start := time.Now()
	if _, err := SafeJoinBounded(t.Context(), root, "Book/01.mp3", 100*time.Millisecond); !metadata.GaveUp(err) {
		t.Errorf("SafeJoinBounded on a held share = %v, want a look given up on", err)
	}
	if _, err := ContentJoinBounded(t.Context(), root, "Book/01.mp3", 100*time.Millisecond); !metadata.GaveUp(err) {
		t.Errorf("ContentJoinBounded on a held share = %v, want a look given up on", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the joins took %v, want each given up on at its bound", took)
	}
	if _, err := SafeJoinBounded(t.Context(), root, "../outside", time.Second); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("a path out of the root = %v, want ErrOutsideRoot", err)
	}
	if _, err := ContentJoinBounded(t.Context(), root, "Book/.audiosilo/x.mp3", time.Second); !errors.Is(err, ErrHidden) {
		t.Errorf("a hidden path = %v, want ErrHidden", err)
	}

	release()
	got, err := SafeJoinBounded(t.Context(), root, "Book/01.mp3", time.Second)
	if want := filepath.Join(root, "Book", "01.mp3"); err != nil || got != want {
		t.Errorf("SafeJoinBounded once the share answers = %q, %v; want %q", got, err, want)
	}
}

// An admin's Re-read of a book on a share that stopped answering (SRV-15c:
// it answered the request timeout's generic 503, the side reads' bound being
// the probe's time, the request's own, and resolveBook's lstats unbounded)
// gives up at the on-demand look's time (WithLookTimeout), well inside the
// request's: a look at the book's folder or its metadata.json while resolving
// it, or a file beside the audio while reading it (the book then kept as
// stored, that file's problem recorded), is a look given up on
// (metadata.GaveUp). Once the share answers, the Re-read reads the book. Not
// parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestReindexOnAHeldShareGivesUp(t *testing.T) {
	const book = "Author/Book"
	for _, c := range []struct{ name, held string }{
		{"the folder", book},
		{"metadata.json", book + "/metadata.json"},
		{"cover", book + "/cover.jpg"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, root, book+"/01.mp3", make([]byte, 1<<10))
			writeFile(t, root, book+"/metadata.json", []byte(`{"title":"ABS Title"}`))
			writeFile(t, root, book+"/cover.jpg", []byte("img"))
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			s.probeTimeout = time.Minute // the look's time, not the probe's, ends it
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
				t.Fatalf("added %d, want the book", res.Added)
			}

			release := metadata.HoldOpensForTests(filepath.Join(root, filepath.FromSlash(c.held)))
			t.Cleanup(release)
			start := time.Now()
			_, err = s.ReindexPath(WithLookTimeout(ctx, 100*time.Millisecond), *lib, book)
			if !metadata.GaveUp(err) {
				t.Errorf("Re-read with %s held = %v, want a look given up on", c.held, err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("Re-read took %v, want it given up on at the look's time", took)
			}
			if c.name == "cover" {
				var problem, file string
				if err := db.QueryRowContext(ctx, `SELECT scan_error, scan_error_file FROM books WHERE library_id = ? AND rel_path = ?`,
					lib.ID, book).Scan(&problem, &file); err != nil || problem != problemUnreadable || file != c.held {
					t.Errorf("the kept book's problem = %q on %q (%v), want %q on %q", problem, file, err, problemUnreadable, c.held)
				}
			}

			release()
			if b, err := s.ReindexPath(WithLookTimeout(ctx, 10*time.Second), *lib, book); err != nil || b.RelPath != book {
				t.Errorf("Re-read once the share answers = %v, want the book", err)
			}
		})
	}
}
