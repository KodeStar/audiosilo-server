package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/mounts"
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
			// The root the scan walks (macOS's temp folder is under a link,
			// /var to /private/var): held there, the walk's own looks are too.
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
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

			hold := metadata.HoldOpensForTests
			if c.name == "metadata.json" || c.name == "attachment" {
				// Its lstat answers (the walk's, from a share's cached
				// attributes) while its read hangs. (A .audiosilo folder
				// whose listing hangs is TestAScanSkipsAFolderThatDoesNotAnswer's.)
				hold = metadata.HoldReadsForTests
			}
			release := hold(filepath.Join(root, filepath.FromSlash(c.file)))
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
	root, err := filepath.EvalSymlinks(t.TempDir()) // the root the scan walks
	if err != nil {
		t.Fatal(err)
	}
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
	started, release := holdRead(t, s, func(absPath string) bool { return mounts.Within(absPath, held) })

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
		t.Fatal("the stopped scan waited on its stuck read")
	}
	if n := s.readsLeft.Load(); n != 1 {
		t.Errorf("reads left = %d, want the stuck one counted", n)
	}
	if res := scanWithin(t, s, *libB, 20*time.Second); res.Added != 1 {
		t.Errorf("the healthy library's scan added %d, want its book", res.Added)
	}

	release()
	waitNoReadsLeft(t, s)
	if _, err := s.IndexPath(ctx, *libA, "Author/Book"); err != nil {
		t.Errorf("index the book once its read returned: %v", err)
	}
}

// A later scan does not wait on a book whose read a stopped scan left stuck
// (its flight held until that read returns, which may be never): it keeps the
// book as stored with an unreadable problem and scans the rest of the library.
// Once the read returns, the next scan reads the book as ever.
func TestALaterScanSkipsABookWhoseReadWasLeft(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	writeFile(t, root, "Author/Other/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if res := scanWithin(t, s, *lib, 20*time.Second); res.Added != 2 {
		t.Fatalf("first scan added %d, want both books", res.Added)
	}
	row := func(rel string) (size int64, problem, file string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT size, scan_error, scan_error_file FROM books WHERE library_id = ? AND rel_path = ?`,
			lib.ID, rel).Scan(&size, &problem, &file); err != nil {
			t.Fatal(err)
		}
		return size, problem, file
	}

	// The book changed on disk, and its read hangs: a scan stopped there
	// leaves the read under way.
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 2<<10))
	writeFile(t, root, "Author/New/01.mp3", make([]byte, 1<<10))
	started, release := holdRead(t, s, isFile(filepath.Join(root, "Author", "Book", "01.mp3")))
	scanCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := s.Scan(scanCtx, *lib)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped scan = %v, want context.Canceled", err)
	}
	if n := s.readsLeft.Load(); n != 1 {
		t.Fatalf("reads left = %d, want the stuck one", n)
	}

	scanWithin(t, s, *lib, 10*time.Second)
	if size, problem, file := row("Author/Book"); size != 1<<10 || problem != problemUnreadable || file != "Author/Book/01.mp3" {
		t.Errorf("the book left stuck: size %d, problem %q on %q; want the stored row kept (%d) with %q on its file",
			size, problem, file, 1<<10, problemUnreadable)
	}
	if size, problem, _ := row("Author/New"); size != 1<<10 || problem != "" {
		t.Errorf("the book after it: size %d, problem %q; want it read", size, problem)
	}

	release()
	waitNoReadsLeft(t, s)
	scanWithin(t, s, *lib, 20*time.Second)
	if size, problem, _ := row("Author/Book"); size != 2<<10 || problem != "" {
		t.Errorf("the book once its read returned: size %d, problem %q; want it read again", size, problem)
	}
}

// holdRead makes s's reads of the files held matches hang, ignoring their
// context as a call stuck in the kernel does, until release (at the test's end
// at the latest): started closes once one is held.
func holdRead(t *testing.T, s *Scanner, held func(absPath string) bool) (started chan struct{}, release func()) {
	started, unblock := make(chan struct{}), make(chan struct{})
	release = sync.OnceFunc(func() { close(unblock) })
	t.Cleanup(release)
	var once sync.Once
	s.readFile = func(ctx context.Context, absPath, ffprobePath string, timeout time.Duration) *metadata.Metadata {
		if held(absPath) {
			once.Do(func() { close(started) })
			<-unblock
		}
		return extractFile(ctx, absPath, ffprobePath, timeout)
	}
	return started, release
}

// isFile matches the one file at path.
func isFile(path string) func(string) bool {
	return func(absPath string) bool { return absPath == path }
}

// A scan does not wait for good on a book an on-demand read is stuck reading
// (its request gone, the read held in a call nothing bounds): after a look's
// time it keeps the book as stored with an unreadable problem and scans the
// rest of the library. The on-demand read stores the book once it returns.
func TestAScanSkipsABookAStuckOnDemandReadHolds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	writeFile(t, root, "Author/Other/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	s.probeTimeout = 200 * time.Millisecond
	started, release := holdRead(t, s, isFile(filepath.Join(root, "Author", "Book", "01.mp3")))

	read := make(chan error, 1)
	go func() {
		_, err := s.IndexPath(ctx, *lib, "Author/Book")
		read <- err
	}()
	<-started
	scanWithin(t, s, *lib, 10*time.Second)
	problem := func(rel string) string {
		t.Helper()
		var p string
		if err := db.QueryRowContext(ctx, `SELECT scan_error FROM books WHERE library_id = ? AND rel_path = ?`,
			lib.ID, rel).Scan(&p); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		return p
	}
	if p := problem("Author/Book"); p != problemUnreadable {
		t.Errorf("the book the stuck read holds: problem %q, want %q", p, problemUnreadable)
	}
	if p := problem("Author/Other"); p != "" {
		t.Errorf("the book after it: problem %q, want it read", p)
	}

	release()
	if err := <-read; err != nil {
		t.Errorf("the on-demand read once it returned: %v", err)
	}
	if p := problem("Author/Book"); p != "" {
		t.Errorf("the book once the on-demand read stored it: problem %q, want none", p)
	}
}

// An on-demand read of a book a scan's read is stuck on (held in a call
// nothing bounds) does not wait out its request: after a look's time it gives
// up, as a look at a share that stopped answering (metadata.GaveUp, which the
// API answers 503 share_not_answering), the shared read and a read of its own
// alike.
func TestAnOnDemandReadGivesUpOnAStuckScanRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	writeFile(t, root, "Author/Book/01.mp3", make([]byte, 1<<10))
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	s.probeTimeout = 200 * time.Millisecond
	started, release := holdRead(t, s, isFile(filepath.Join(root, "Author", "Book", "01.mp3")))

	scanned := make(chan error, 1)
	go func() {
		_, err := s.Scan(ctx, *lib)
		scanned <- err
	}()
	<-started
	for name, read := range map[string]func() (*catalog.Book, error){
		"IndexPath":   func() (*catalog.Book, error) { return s.IndexPath(ctx, *lib, "Author/Book") },
		"ReindexPath": func() (*catalog.Book, error) { return s.ReindexPath(ctx, *lib, "Author/Book") },
	} {
		began := time.Now()
		b, err := read()
		if took := time.Since(began); took > 5*time.Second {
			t.Errorf("%s waited %v on the stuck read", name, took)
		}
		if b != nil || !metadata.GaveUp(err) {
			t.Errorf("%s = %+v, %v; want given up (metadata.GaveUp)", name, b, err)
		}
	}

	release()
	select {
	case err := <-scanned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scan did not finish once its read returned")
	}
	if _, err := s.IndexPath(ctx, *lib, "Author/Book"); err != nil {
		t.Errorf("index the book once the scan's read returned: %v", err)
	}
}

// waitNoReadsLeft waits for the reads stopped scans left under way to end.
func waitNoReadsLeft(t *testing.T, s *Scanner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.readsLeft.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the left read never ended its flight")
		}
		time.Sleep(time.Millisecond)
	}
}

// stampAddedAt's look at each file (Linux's statx included, not only the
// lstat) is bounded: a book on a share that stopped answering is stamped
// after one look's time, not one per file (the rest are not looked at once one
// is given up on), with the walk's time of its files, the earliest, where the
// walk gave one: added_at is never written again, so now would put a book
// years old at the top of Recently added for good. A book built without the
// walk's times is stamped now, first seen. Not parallel:
// metadata.HoldOpensForTests.
func TestStampAddedAtOnAHeldShare(t *testing.T) {
	root := t.TempDir()
	lib := catalog.Library{Root: root}
	walked := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	b := catalog.Book{RelPath: "A/Book", IsFolder: true}
	for i := range 5 {
		rel := fmt.Sprintf("A/Book/%02d.mp3", i)
		writeFile(t, root, rel, []byte("x"))
		b.Files = append(b.Files, catalog.BookFile{RelPath: rel})
		// The earliest is not the first: the rest count though never looked at.
		b.FoundAdded = append(b.FoundAdded, walked.Add(time.Duration(4-i)*time.Hour).Unix())
	}
	release := metadata.HoldOpensForTests(filepath.Join(root, "A", "Book"))
	t.Cleanup(release)
	const look = 200 * time.Millisecond

	start := time.Now()
	stampAddedAt(t.Context(), lib, &b, look)
	if took := time.Since(start); took > 3*look {
		t.Errorf("stampAddedAt on a held share took %v, want one look's time (%v)", took, look)
	}
	if want := walked.Format(time.RFC3339); b.AddedAt != want {
		t.Errorf("added_at on a held share = %q, want the walk's earliest %q", b.AddedAt, want)
	}

	unwalked := catalog.Book{RelPath: b.RelPath, IsFolder: true, Files: b.Files}
	before := time.Now().UTC().Add(-time.Second)
	stampAddedAt(t.Context(), lib, &unwalked, look)
	if at, err := time.Parse(time.RFC3339, unwalked.AddedAt); err != nil || at.Before(before.Truncate(time.Second)) {
		t.Errorf("added_at on a held share with no walk's time = %q (%v), want now", unwalked.AddedAt, err)
	}
}
