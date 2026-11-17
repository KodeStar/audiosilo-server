package library

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A file beside a book's audio whose read hangs (server#360, SRV-15b: on a
// frozen share the metadata.json read, the ebook check, the attachments' reads
// and the cover's look each held the scan for good, cancel included) no longer
// holds the scan: each is given up on at the read's time, the book written with
// what there is and that file's problem recorded, so Health lists it and the
// next scan looks at it again. Once the file answers, that scan reads it. Not
// parallel: metadata.HoldOpensForTests swaps the mount lookup.
func TestHeldSideReadsLetTheScanFinish(t *testing.T) {
	const book = "Author/Book"
	for _, c := range []struct {
		name, file string
		data       func(t *testing.T) []byte
		column     string // the books column the file's reading fills, '' while given up on
	}{
		{"metadata.json", book + "/metadata.json", func(*testing.T) []byte { return []byte(`{"title":"ABS Title"}`) }, "abs_sig"},
		{"attachment", book + "/.audiosilo/characters.json", func(*testing.T) []byte { return []byte(`{}`) }, "attachments_sig"},
		{"ebook", book + "/Book.epub", func(t *testing.T) []byte { return epubBytes(t, minimalEpub...) }, "ebook_path"},
		{"cover", book + "/cover.jpg", func(*testing.T) []byte { return []byte("img") }, "cover_path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root := t.TempDir()
			writeFile(t, root, book+"/01.mp3", make([]byte, 1<<10))
			writeFile(t, root, c.file, c.data(t))
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, "", slog.Default())
			s.probeTimeout = 200 * time.Millisecond
			read := func() (problem, file, detail, column string) {
				t.Helper()
				if err := db.QueryRowContext(ctx, `SELECT scan_error, scan_error_file, scan_error_detail, `+c.column+`
					FROM books WHERE library_id = ? AND rel_path = ?`, lib.ID, book).Scan(&problem, &file, &detail, &column); err != nil {
					t.Fatal(err)
				}
				return problem, file, detail, column
			}

			release := metadata.HoldOpensForTests(filepath.Join(root, filepath.FromSlash(c.file)))
			t.Cleanup(release)
			if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 1 {
				t.Fatalf("added %d, want the book written with what there is", res.Added)
			}
			if problem, file, detail, column := read(); problem != problemUnreadable || file != c.file || detail != tagsUnreadDetail || column != "" {
				t.Errorf("held %s: problem %q on %q (%q), %s %q; want %q on the file, nothing read from it",
					c.name, problem, file, detail, c.column, column, problemUnreadable)
			}

			release()
			scanWithin(t, s, *lib, 20*time.Second)
			if problem, file, _, column := read(); problem != "" || column == "" {
				t.Errorf("after %s answers: problem %q on %q, %s %q; want it read, no problem", c.name, problem, file, c.column, column)
			}
		})
	}
}

// A book found unchanged whose metadata.json changed reads it again in the
// scan's backfill: held there, it is given up on and the scan finishes,
// keeping what is stored for the next scan (nothing about the book is
// rewritten, so no problem either). Not parallel: metadata.HoldOpensForTests.
func TestHeldBackfillReadLetsTheScanFinish(t *testing.T) {
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Author/Book"
	writeFile(t, root, book+"/01.mp3", make([]byte, 1<<10))
	writeFile(t, root, book+"/metadata.json", []byte(`{"title":"ABS Title"}`))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	s.probeTimeout = 200 * time.Millisecond
	scanWithin(t, s, *lib, 20*time.Second)
	sig := func() string {
		t.Helper()
		var sig string
		if err := db.QueryRowContext(ctx, `SELECT abs_sig FROM books WHERE rel_path = ?`, book).Scan(&sig); err != nil {
			t.Fatal(err)
		}
		return sig
	}
	was := sig()
	if was == "" {
		t.Fatal("no metadata.json read at the first scan")
	}
	writeFile(t, root, book+"/metadata.json", []byte(`{"title":"A Longer ABS Title"}`))
	release := metadata.HoldOpensForTests(filepath.Join(root, book, "metadata.json"))
	t.Cleanup(release)
	scanWithin(t, s, *lib, 20*time.Second)
	if got := sig(); got != was {
		t.Errorf("abs_sig = %q after a held read, want the stored %q kept", got, was)
	}
}

// A scan stopped while a book's read is held in a call that never returns
// (a share that stopped answering, in a call nothing bounds) ends all the
// same, shortly after the stop: the read is left to end on its own and
// counted, and another library's scan runs. Once the read returns, its book's
// flight ends, so the book can be read again.
func TestStopDuringAStuckRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	held, healthy := t.TempDir(), t.TempDir()
	writeFile(t, held, "Author/Book/01.mp3", make([]byte, 1<<10))
	writeFile(t, healthy, "Author/Book/01.mp3", make([]byte, 1<<10))
	libA, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Held", Root: held})
	libB, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "Healthy", Root: healthy})
	s := NewScanner(cat, "", slog.Default())
	started, unblock := make(chan struct{}), make(chan struct{})
	s.readFile = func(ctx context.Context, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata {
		if within(held, absPath) {
			close(started)
			<-unblock // ignores ctx, as a call stuck in the kernel does
		}
		return extractFile(ctx, absPath, ffprobePath, timeout)
	}

	scanCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.Scan(scanCtx, *libA)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("stopped scan = %v, want context.Canceled", err)
		}
	case <-time.After(stopGrace + 10*time.Second):
		close(unblock)
		t.Fatal("the stopped scan waited on its stuck read")
	}
	if n := s.readsLeft.Load(); n != 1 {
		t.Errorf("reads left = %d, want the stuck one counted", n)
	}
	if res := scanWithin(t, s, *libB, 20*time.Second); res.Added != 1 {
		t.Errorf("the healthy library's scan added %d, want its book", res.Added)
	}

	close(unblock)
	deadline := time.Now().Add(10 * time.Second)
	for s.readsLeft.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the left read never ended its flight")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := s.IndexPath(ctx, *libA, "Author/Book"); err != nil {
		t.Errorf("index the book once its read returned: %v", err)
	}
}

// within reports whether p is root or below it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && (len(rel) < 3 || rel[:3] != "../")
}
