package library

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A re-index where only a part's in-process tag read was cut short but its
// length was read all the same (ffprobe answered once the share did:
// metadata.Metadata.LengthRead) keeps the stored book's names and records the
// read problem, as any read cut short does, but takes the lengths, codecs and
// chapters read now rather than throwing them away (server#360). The next
// scan, its tags answering, reads it whole.
func TestReindexWithOnlyTagsCutShortKeepsTheLengthsRead(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Some Folder/Odd Name"
	for _, part := range []string{"01.mp3", "02.mp3"} {
		silence(t, root, book+"/"+part, 1, "album=Real Title", "artist=Real Author", "title=Part "+part)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	type reading struct {
		title           string
		duration, part2 float64
		lastEnd         float64
		problem, detail string
		mtime           int64
	}
	read := func() reading {
		t.Helper()
		var r reading
		if err := db.QueryRowContext(ctx, `SELECT title, duration,
			(SELECT f.duration FROM book_files f WHERE f.book_id = books.id AND f.rel_path LIKE '%02.mp3'),
			(SELECT MAX(c.book_offset - c.start + c."end") FROM chapters c WHERE c.book_id = books.id),
			scan_error, scan_error_detail, mtime FROM books WHERE rel_path = ?`, book).
			Scan(&r.title, &r.duration, &r.part2, &r.lastEnd, &r.problem, &r.detail, &r.mtime); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := read()
	if first.title != "Real Title" || first.duration < 1.5 || first.duration > 2.5 {
		t.Fatalf("first scan = %+v, want the tags' title and about 2 s", first)
	}

	// The second part is replaced by a longer one, its tags read cut short.
	silence(t, root, book+"/02.mp3", 3, "album=Changed Title", "artist=Real Author")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(book+"/02.mp3")), later, later); err != nil {
		t.Fatal(err)
	}
	s.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		md := extractFile(ctx, p, ffprobe, timeout)
		if strings.HasSuffix(filepath.ToSlash(p), book+"/02.mp3") {
			return metadata.TagsUnreadForTests(md)
		}
		return md
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.title != first.title || got.mtime != first.mtime || got.problem != problemUnreadable || got.detail != tagsUnreadDetail {
		t.Errorf("re-index with a part's tags cut short = %+v, want the names and signature kept, the problem recorded", got)
	}
	if got.part2 < 2.5 || got.duration < 3.5 || got.lastEnd < 3.5 || res.Updated != 1 {
		t.Errorf("re-index with a part's tags cut short = %+v, %d updated; want the new part's length taken (about 4 s)", got, res.Updated)
	}

	// Its tags answer again: read whole, the problem gone.
	s.readFile = nil
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.problem != "" || got.mtime == first.mtime || got.duration < 3.5 {
		t.Errorf("once its tags answer = %+v, want it read whole, no problem", got)
	}
}

// A file whose read was cut short (a share that hung) and that answers with a
// lasting problem once the share is back (a permission) has that problem
// recorded rather than keeping "reading it timed out" for good, its book kept
// as stored (an open that fails as a share fails knows nothing of the file);
// the next scan leaves it until the file opens (server#360).
func TestAHungFileThatAnswersWithAPermissionRecordsIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Book.mp3"
	silence(t, root, book, 1, "album=Real Title", "artist=Real Author")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	// As a scan whose read of it was cut short left it.
	if _, err := db.ExecContext(ctx, `UPDATE books SET scan_error = ?, scan_error_file = ?, scan_error_detail = ? WHERE rel_path = ?`,
		problemUnreadable, book, tagsUnreadDetail, book); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(root, filepath.FromSlash(book))
	if err := os.Chmod(abs, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(abs, 0o644) })
	if f, err := os.Open(abs); err == nil {
		_ = f.Close()
		t.Skip("file permissions don't stop this user reading (root?)")
	}
	read := func() (title, problem, detail string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT title, scan_error, scan_error_detail FROM books WHERE rel_path = ?`, book).
			Scan(&title, &problem, &detail); err != nil {
			t.Fatal(err)
		}
		return
	}
	permission := syscall.EACCES.Error()
	for i := range 2 {
		if _, err := s.Scan(ctx, *lib); err != nil {
			t.Fatal(err)
		}
		if title, problem, detail := read(); title != "Real Title" || problem != problemUnreadable || detail != permission {
			t.Errorf("scan %d with the file refused = %q, %q (%q); want the book kept, the permission recorded", i+1, title, problem, detail)
		}
	}
	if err := os.Chmod(abs, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if title, problem, _ := read(); title != "Real Title" || problem != "" {
		t.Errorf("once it opens = %q, problem %q; want it read, no problem", title, problem)
	}
}
