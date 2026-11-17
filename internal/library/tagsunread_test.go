package library

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/metadata"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A re-index where any file's read is cut short (a share that stopped
// answering: metadata.Metadata.TagsUnread) or refused (too many reads stuck:
// ReadRefused) rewrites nothing of the stored book (names, length, codec,
// chapters, cover flag, date, genres) and records the file as unreadable, with
// ffprobe or without, saying which, so the next scan reads it again and puts
// it right. A new book read that way is written with the path's names, as
// before.
func TestReindexWithAReadCutShortKeepsTheStoredBook(t *testing.T) {
	t.Parallel()
	probes := map[string]string{"no ffprobe": ""}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		probes["ffprobe"] = p
	}
	for name, ffprobe := range probes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db := storetest.Open(t)
			cat := catalog.New(db, time.Now)
			root := t.TempDir()
			kept := "Some Folder/Odd Name"
			for _, part := range []string{"01.mp3", "02.mp3"} {
				silence(t, root, kept+"/"+part, 1, "album=Real Title", "artist=Real Author",
					"genre=Fantasy", "date=2020-05-01", "title=Part "+part)
			}
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, ffprobe, slog.Default())
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			type reading struct {
				title, author, genres, released, codec string
				duration                               float64
				chapters, hasCover                     int
				scanError, detail                      string
			}
			read := func(rel string) reading {
				t.Helper()
				var r reading
				if err := db.QueryRowContext(ctx, `SELECT title, author, genres, IFNULL(released, ''), codec, duration,
					(SELECT COUNT(*) FROM chapters c WHERE c.book_id = books.id), IFNULL(has_cover, -1),
					scan_error, scan_error_detail FROM books WHERE rel_path = ?`, rel).
					Scan(&r.title, &r.author, &r.genres, &r.released, &r.codec, &r.duration, &r.chapters, &r.hasCover,
						&r.scanError, &r.detail); err != nil {
					t.Fatalf("%s: %v", rel, err)
				}
				return r
			}
			first := read(kept)
			if first.title != "Real Title" || first.genres == "" || first.released == "" || first.duration <= 0 ||
				first.codec == "" || first.chapters != 2 {
				t.Fatalf("first scan = %+v, want the tags' reading and its length", first)
			}
			// As a read that found embedded art left it.
			if _, err := db.ExecContext(ctx, `UPDATE books SET has_cover = 1 WHERE rel_path = ?`, kept); err != nil {
				t.Fatal(err)
			}
			first.hasCover = 1
			stored := func(problem, detail string) reading {
				r := first
				r.scanError, r.detail = problem, detail
				return r
			}

			// A part changes (so the book is re-indexed) on a share that stops
			// answering: every read runs out of time. A new book appears beside it.
			part2 := filepath.Join(root, filepath.FromSlash(kept+"/02.mp3"))
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(part2, later, later); err != nil {
				t.Fatal(err)
			}
			fresh := "Some Folder/New Arrival"
			silence(t, root, fresh+"/01.mp3", 1, "album=Tagged Title", "artist=Real Author")
			s.probeTimeout = time.Nanosecond
			res, err := s.Scan(ctx, *lib)
			if err != nil {
				t.Fatal(err)
			}
			// Kept whole is not updated (server#360).
			if res.Updated != 0 || res.Added != 1 {
				t.Errorf("re-index with its reads cut short: %d updated, %d added; want 0 and the new book", res.Updated, res.Added)
			}
			if got, want := read(kept), stored(problemUnreadable, tagsUnreadDetail); got != want {
				t.Errorf("re-index with its reads cut short = %+v, want the stored book kept, %+v", got, want)
			}
			if got := read(fresh); got.title != "New Arrival" || got.scanError != problemUnreadable {
				t.Errorf("a new book with its read cut short = %+v, want the path's title and the problem", got)
			}

			// Reads answer again, but the second part's is refused (too many
			// stuck): the book is still kept whole, and Health says why.
			s.probeTimeout = 10 * time.Second
			s.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
				if strings.HasSuffix(filepath.ToSlash(p), kept+"/02.mp3") {
					return metadata.UnreadForTests(p, true)
				}
				return extractFile(ctx, p, ffprobe, timeout)
			}
			if res, err = s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			if res.Updated != 1 {
				t.Errorf("re-index with a part's read refused: %d updated, want only the new book read again", res.Updated)
			}
			if got, want := read(kept), stored(problemUnreadable, readRefusedDetail); got != want {
				t.Errorf("re-index with a part's read refused = %+v, want the stored book kept, %+v", got, want)
			}
			if got := read(fresh); got.title != "Tagged Title" || got.scanError != "" {
				t.Errorf("new book once reads answer = %+v, want its tags' title", got)
			}

			// Every read answers: the next scan reads the book again.
			s.readFile = nil
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			if got := read(kept); got.title != "Real Title" || got.duration != first.duration || got.chapters != 2 ||
				got.scanError != "" {
				t.Errorf("after the share came back = %+v, want it read, no problem", got)
			}
		})
	}
}

// With no ffprobe, a part whose length's in-process read runs out of time even
// alone (ProbeTimedOut, its tags read) keeps the stored book as a read cut short
// does, and records why (server#360): never the book kept with no problem, which
// would leave Health silent and clear one recorded before.
func TestReindexWithALengthTimeOutRecordsItsProblem(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Author/Book"
	for _, part := range []string{"01.mp3", "02.mp3"} {
		silence(t, root, book+"/"+part, 1, "album=Real Title", "artist=Real Author")
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	read := func() (title string, duration float64, problem, file, detail string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT title, duration, scan_error, scan_error_file, scan_error_detail
			FROM books WHERE rel_path = ?`, book).Scan(&title, &duration, &problem, &file, &detail); err != nil {
			t.Fatal(err)
		}
		return
	}
	title, duration, _, _, _ := read()
	if title != "Real Title" || duration <= 0 {
		t.Fatalf("first scan = %q, %v; want the tags' title and a length", title, duration)
	}

	part2 := filepath.Join(root, filepath.FromSlash(book+"/02.mp3"))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(part2, later, later); err != nil {
		t.Fatal(err)
	}
	s.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		if strings.HasSuffix(filepath.ToSlash(p), book+"/02.mp3") {
			return metadata.TimedOutForTests(p)
		}
		return extractFile(ctx, p, ffprobe, timeout)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	gotTitle, gotDuration, problem, file, detail := read()
	if gotTitle != title || gotDuration != duration || res.Updated != 0 {
		t.Errorf("re-index with a part's length out of time = %q, %v, %d updated; want the stored book kept", gotTitle, gotDuration, res.Updated)
	}
	if problem != problemUnreadable || file != book+"/02.mp3" || detail != tagsUnreadDetail {
		t.Errorf("recorded problem = %q on %q (%q), want %q on the part (%q)", problem, file, detail, problemUnreadable, tagsUnreadDetail)
	}
}

// problemCleared asks the recorded file again and counts a read whose length
// is still out of time as not cleared (readCutShort), so an unchanged book
// whose problem remains is not read whole on every scan only to be kept.
func TestProblemClearedNotWhileTheLengthTimesOut(t *testing.T) {
	t.Parallel()
	s := NewScanner(nil, "", slog.Default())
	lib := catalog.Library{Root: t.TempDir()}
	sig := catalog.Signature{ScanError: problemUnreadable, ScanErrorFile: "Book/02.mp3"}
	slots := newReadSlots(1, time.Second)
	slots.readFile = func(_ context.Context, p, _ string, _ time.Duration) *metadata.Metadata {
		return metadata.TimedOutForTests(p)
	}
	if s.problemCleared(t.Context(), lib, sig, slots) {
		t.Error("a length still out of time cleared the problem")
	}
	slots.readFile = func(_ context.Context, p, _ string, _ time.Duration) *metadata.Metadata {
		return &metadata.Metadata{Format: "mp3", Duration: 1}
	}
	if !s.problemCleared(t.Context(), lib, sig, slots) {
		t.Error("a read that answers in full did not clear the problem")
	}
}

// A kept book records the part whose read was cut short, over an earlier
// part's lasting problem (a file ffprobe can't read): that record is what the
// next scan's problemCleared looks at again, so an unchanged book is read
// whole once the part answers, rather than kept as stored for good behind a
// problem that never clears.
func TestKeptBookRecordsThePartCutShortOverAnEarlierProblem(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Author/Book"
	for _, part := range []string{"01.mp3", "02.mp3", "03.mp3"} {
		silence(t, root, book+"/"+part, 1, "album=Real Title", "artist=Real Author")
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, filepath.FromSlash(book+"/03.mp3")), later, later); err != nil {
		t.Fatal(err)
	}
	s.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		md := extractFile(ctx, p, ffprobe, timeout)
		switch {
		case strings.HasSuffix(filepath.ToSlash(p), book+"/01.mp3"):
			md.ProbeErr = errors.New("Invalid data found when processing input")
		case strings.HasSuffix(filepath.ToSlash(p), book+"/03.mp3"):
			return metadata.TimedOutForTests(p)
		}
		return md
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	var problem, file string
	if err := db.QueryRowContext(ctx, `SELECT scan_error, scan_error_file FROM books WHERE rel_path = ?`, book).
		Scan(&problem, &file); err != nil {
		t.Fatal(err)
	}
	if problem != problemUnreadable || file != book+"/03.mp3" || res.Updated != 0 {
		t.Errorf("recorded %q on %q, %d updated; want the kept book's problem on the part cut short", problem, file, res.Updated)
	}
}

// A length that runs out of time can do so on every scan (a damaged part), so
// it keeps the stored book only while the book's files are the stored ones:
// the part itself may have changed (its new length isn't known), but with a
// part added, or another part changed, the book is written whole with the
// part's problem, rather than held at a file list that is no longer on disk. A
// read cut short before its tags (a share that stopped answering, which comes
// back) keeps the stored book whatever its files.
func TestLengthTimeOutKeepsTheBookOnlyWhileItsFilesAreTheStoredOnes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Author/Book"
	part := func(name string, secs float64) {
		t.Helper()
		silence(t, root, book+"/"+name, secs, "album=Real Title", "artist=Real Author")
	}
	for _, name := range []string{"01.mp3", "02.mp3", "03.mp3"} {
		part(name, 1)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, "", slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	type stored struct {
		files, chapters int
		size02          int64
		problem, file   string
	}
	read := func() (got stored) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM book_files f WHERE f.book_id = books.id),
			(SELECT COUNT(*) FROM chapters c WHERE c.book_id = books.id),
			(SELECT f.size FROM book_files f WHERE f.book_id = books.id AND f.rel_path = ?),
			scan_error, scan_error_file FROM books WHERE rel_path = ?`, book+"/02.mp3", book).
			Scan(&got.files, &got.chapters, &got.size02, &got.problem, &got.file); err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := read()
	// What each scan below reads of the parts: 03's length out of time, or 02's
	// read cut short.
	timedOut, cutShort := book+"/03.mp3", ""
	s.readFile = func(ctx context.Context, p, ffprobe string, timeout time.Duration) *metadata.Metadata {
		switch p := filepath.ToSlash(p); {
		case cutShort != "" && strings.HasSuffix(p, cutShort):
			return metadata.UnreadForTests(p, false)
		case timedOut != "" && strings.HasSuffix(p, timedOut):
			return metadata.TimedOutForTests(p)
		}
		return extractFile(ctx, p, ffprobe, timeout)
	}
	scan := func() *ScanResult {
		t.Helper()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// The part that times out is the one that changed: the stored book is kept.
	part("03.mp3", 2)
	want := first
	want.problem, want.file = problemUnreadable, timedOut
	if res, got := scan(), read(); got != want || res.Updated != 0 {
		t.Errorf("the timed-out part changed = %+v (%d updated), want the stored book kept, %+v", got, res.Updated, want)
	}

	// A part is added: written whole, the timed-out part's problem with it.
	part("04.mp3", 1)
	want.files, want.chapters = 4, 4
	if res, got := scan(), read(); got != want || res.Updated != 1 {
		t.Errorf("a part added = %+v (%d updated), want the book written whole, %+v", got, res.Updated, want)
	}

	// Another part changes: written whole again.
	part("02.mp3", 2)
	res, got := scan(), read()
	if got.size02 == first.size02 || res.Updated != 1 {
		t.Errorf("another part changed = %+v (%d updated), want its new size stored", got, res.Updated)
	}
	want.size02 = got.size02
	if got != want {
		t.Errorf("another part changed = %+v, want %+v", got, want)
	}

	// A part goes while another's read is cut short: kept, whatever its files.
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(book+"/04.mp3"))); err != nil {
		t.Fatal(err)
	}
	timedOut, cutShort = "", book+"/02.mp3"
	want.file = cutShort
	if res, got := scan(), read(); got != want || res.Updated != 0 {
		t.Errorf("a part gone, a read cut short = %+v (%d updated), want the stored book kept, %+v", got, res.Updated, want)
	}

	// Every read answers: the book is read as it is on disk.
	s.readFile = nil
	scan()
	if got := read(); got.files != 3 || got.chapters != 3 || got.problem != "" {
		t.Errorf("once every read answers = %+v, want the three parts, no problem", got)
	}
}

// A problem with a file beside the audio clears only once the kind of call
// that gave up answers: a read of a metadata.json, an attachment or an ebook,
// a listing of a folder, a stat of a cover image. A file whose stat answers
// while it still can't be read (on NFS or FUSE a stat can answer from a cache
// while reads hang) keeps its problem, so the scan does not read the whole
// book again only to wait out the same read. Gone counts as cleared.
func TestProblemClearedAsksTheKindOfCallThatGaveUp(t *testing.T) {
	t.Parallel()
	s := NewScanner(nil, "", slog.Default())
	s.probeTimeout = 5 * time.Second
	root := t.TempDir()
	lib := catalog.Library{Root: root}
	writeFile(t, root, "Book/metadata.json", []byte(`{"title":"T"}`))
	writeFile(t, root, "Book/cover.jpg", []byte("img"))
	writeFile(t, root, "Folder/01.mp3", []byte("x"))
	cleared := func(rel string) bool {
		t.Helper()
		sig := catalog.Signature{ScanError: problemUnreadable, ScanErrorFile: rel}
		return s.problemCleared(t.Context(), lib, sig, newReadSlots(1, time.Second))
	}
	for _, rel := range []string{"Book/metadata.json", "Book/cover.jpg", "Folder", "Book/gone.json"} {
		if !cleared(rel) {
			t.Errorf("%s answers its call: problem not cleared", rel)
		}
	}

	chmod := func(rel string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		was, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, was.Mode().Perm()) })
	}
	chmod("Book/metadata.json", 0)
	chmod("Book/cover.jpg", 0)
	chmod("Folder", 0o300)
	if _, err := os.ReadFile(filepath.Join(root, "Book", "metadata.json")); err == nil {
		t.Skip("file permissions don't stop this user reading (root?)")
	}
	if cleared("Book/metadata.json") {
		t.Error("a metadata.json whose stat answers but read doesn't: problem cleared, want it kept")
	}
	if cleared("Folder") {
		t.Error("a folder whose stat answers but listing doesn't: problem cleared, want it kept")
	}
	if !cleared("Book/cover.jpg") {
		t.Error("a cover image whose stat answers: problem kept, want it cleared (a stat is all its look makes)")
	}
}
