package launcher

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A scan that indexes a new book moves the rows a client stored under its
// track before it was indexed (server#210): the scanner's OnRunFinished
// re-keys the library, then hands the run on (the notifications).
func TestScanEndRekeysANewBook(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO users(username, password_hash, role, created_at, updated_at)
		VALUES('ann', 'x', 'admin', 't', 't')`)
	if err != nil {
		t.Fatal(err)
	}
	ann, _ := res.LastInsertId()
	// The book's track, saved before any scan saw the book.
	const book, track = "Author/Book", "Author/Book/01 - Unsouled.m4b"
	if _, err := db.ExecContext(ctx, `INSERT INTO progress(user_id, library_id, rel_path, position, duration, updated_at)
		VALUES(?, ?, ?, 42, 0, '2026-01-01T00:00:00Z')`, ann, lib.ID, track); err != nil {
		t.Fatal(err)
	}
	audio, err := os.ReadFile(filepath.Join("..", "..", "testdata", "library", "Will Wight", "Cradle", "01 - Unsouled.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(track)), audio, 0o644); err != nil {
		t.Fatal(err)
	}

	scanner := library.NewScanner(cat, "", discardLog())
	heard := make(chan library.RunReport, 1)
	scanner.OnRunFinished = rekeyAfterScan(ctx, cat, discardLog(), func(_ context.Context, r library.RunReport) { heard <- r })
	scanner.Start(ctx)
	scanner.Enqueue(*lib, library.TriggerManual, nil)
	select {
	case r := <-heard:
		if r.Status != catalog.RunOK {
			t.Fatalf("scan ended %s", r.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scan never finished")
	}
	p, err := cat.GetProgress(ctx, ann, catalog.Ref{LibraryID: lib.ID, Path: book})
	if err != nil || p == nil || p.Position != 42 {
		t.Fatalf("progress on the new book = %+v (%v), want the track's 42", p, err)
	}
	if left, _ := cat.GetProgress(ctx, ann, catalog.Ref{LibraryID: lib.ID, Path: track}); left != nil {
		t.Errorf("progress left on the track: %+v", left)
	}
	var logged int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM path_rekeys WHERE from_path = ?`, track).Scan(&logged); err != nil || logged != 1 {
		t.Errorf("logged %d rows (%v), want the moved one", logged, err)
	}
}

// A start-up job's progress is logged while it runs, at most once an interval,
// and never for its last count (the job's own line says that).
func TestStartProgress(t *testing.T) {
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	each := startProgress(log, "working", 0)
	each(1, 3)
	each(2, 3)
	each(3, 3)
	if got := strings.Count(out.String(), "msg=working"); got != 2 || !strings.Contains(out.String(), "done=2 of=3") {
		t.Errorf("logged %d lines, want one a count but the last:\n%s", got, out.String())
	}
	out.Reset()
	rare := startProgress(log, "working", time.Hour)
	rare(1, 3)
	rare(2, 3)
	if out.Len() != 0 {
		t.Errorf("logged within the interval:\n%s", out.String())
	}
}

// The start's re-key names a path whose move failed, and at the next start
// names it as the one it tries again (server#210): it looks at no other path
// then.
func TestRekeyAtStartNamesFailedPaths(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	const book, track = "Author/Book", "Author/Book/01.mp3"
	if _, err := cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: book, IsFolder: true, Title: "Book",
		Files: []catalog.BookFile{{RelPath: track}}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(7, 'ann', 'x', 'admin', 't', 't')`,
		`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(7, 1, 'Author/Book/01.mp3', 1, 'mark', 't')`,
		`CREATE TRIGGER refuse_carry BEFORE UPDATE OF rel_path ON bookmarks BEGIN SELECT RAISE(ABORT, 'refused'); END`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	rekeyAtStart(ctx, cat, nil, log)
	if got := out.String(); !strings.Contains(got, `msg="path_rekey failed"`) || !strings.Contains(got, "path=Author/Book/01.mp3") ||
		strings.Contains(got, "path_rekey retried") {
		t.Fatalf("the first start logged:\n%s", got)
	}
	out.Reset()
	if _, err := db.ExecContext(ctx, `DROP TRIGGER refuse_carry`); err != nil {
		t.Fatal(err)
	}
	rekeyAtStart(ctx, cat, nil, log)
	got := out.String()
	if !strings.Contains(got, "path_rekey retried") || !strings.Contains(got, "path=Author/Book/01.mp3") ||
		!strings.Contains(got, "paths_file=1") || strings.Contains(got, "path_rekey failed") {
		t.Fatalf("the next start logged:\n%s", got)
	}
	out.Reset()
	rekeyAtStart(ctx, cat, nil, log)
	if out.Len() != 0 {
		t.Errorf("a start with nothing to re-key logged:\n%s", out.String())
	}
}

// The start's re-key leaves a library whose folder does not answer whole
// (server#210): a disk it can't look at can't tell a sibling folder of another
// spelling apart from the book, so the index alone moves nothing. It names the
// library, and the next start, with the folder back, runs over it.
func TestRekeyAtStartSkipsUnansweringRoot(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := filepath.Join(t.TempDir(), "share") // not there yet: a stat fails
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	const book, track = "Author/Book", "Author/Book/01.mp3"
	if _, err := cat.UpsertBook(ctx, &catalog.Book{LibraryID: lib.ID, RelPath: book, IsFolder: true, Title: "Book",
		Files: []catalog.BookFile{{RelPath: track}}}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(7, 'ann', 'x', 'admin', 't', 't')`,
		`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES(7, 1, 'Author/Book/01.mp3', 1, 'mark', 't')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	marks := func(p string) int {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bookmarks WHERE rel_path = ?`, p).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var out bytes.Buffer
	log := slog.New(slog.NewTextHandler(&out, nil))
	rekeyAtStart(ctx, cat, rootAnswers(ctx, cat, library.NewScanner(cat, "", discardLog()), log), log)
	if got := out.String(); !strings.Contains(got, "path_rekey skipped a library") || marks(track) != 1 {
		t.Fatalf("with the folder not answering, %d bookmarks left on the track; logged:\n%s", marks(track), got)
	}
	out.Reset()
	if err := os.MkdirAll(filepath.Join(root, "Author", "Book"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A new start, a new scanner (its root checks are cached a while).
	rekeyAtStart(ctx, cat, rootAnswers(ctx, cat, library.NewScanner(cat, "", discardLog()), log), log)
	if got := out.String(); !strings.Contains(got, "path_rekey retried a library") || marks(book) != 1 || marks(track) != 0 {
		t.Fatalf("with the folder back, %d bookmarks on the book; logged:\n%s", marks(book), got)
	}
}
