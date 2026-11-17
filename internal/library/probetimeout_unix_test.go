//go:build unix

package library

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A part whose ffprobe run times out keeps the book's length (server#360):
// the live check's repro, a part swapped for a FIFO held open by a writer that
// never writes, so ffprobe's read waits until it is killed, alone too. The
// re-index keeps the stored book whole (length, chapters) and records the
// part's time-out, not counted as updated; once the part is back, the next
// scan reads the book again.
func TestPartWhoseProbeTimesOutKeepsTheBookLength(t *testing.T) {
	t.Parallel()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not available")
	}
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const book = "Author/Book"
	for _, part := range []string{"01.mp3", "02.mp3", "03.mp3"} {
		silence(t, root, book+"/"+part, 1, "album=Real Title", "artist=Real Author")
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	type reading struct {
		title     string
		duration  float64
		chapters  int
		scanError string
		file      string
	}
	read := func() reading {
		t.Helper()
		var r reading
		if err := db.QueryRowContext(ctx, `SELECT title, duration,
			(SELECT COUNT(*) FROM chapters c WHERE c.book_id = books.id), scan_error, scan_error_file
			FROM books WHERE rel_path = ?`, book).
			Scan(&r.title, &r.duration, &r.chapters, &r.scanError, &r.file); err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := read()
	if first.title != "Real Title" || first.duration < 2.5 || first.chapters != 3 || first.scanError != "" {
		t.Fatalf("first scan = %+v, want the three parts' length and chapters", first)
	}

	// Part 2 becomes a FIFO a writer holds open (`sleep 900 > 02.mp3`).
	part2 := filepath.Join(root, filepath.FromSlash(book+"/02.mp3"))
	if err := os.Remove(part2); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(part2, 0o644); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	writer, err := os.OpenFile(part2, os.O_RDWR, 0) // a writer, so a reader's open returns and its read waits
	if err != nil {
		t.Skipf("can't hold the FIFO open: %v", err)
	}
	defer writer.Close()
	s.probeTimeout = 500 * time.Millisecond
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	got := read()
	if got.duration != first.duration || got.chapters != first.chapters || got.title != first.title {
		t.Errorf("re-index with a part's ffprobe timed out = %+v, want the stored length and chapters, %+v", got, first)
	}
	// The FIFO's size is 0, so the part reads as empty_file (an empty part
	// comes first); a part on a hung share is probe_failed. Either way it is
	// the part's problem, recorded.
	if got.scanError == "" || got.file != book+"/02.mp3" {
		t.Errorf("recorded problem %q on %q, want one on the part", got.scanError, got.file)
	}
	if res.Updated != 0 {
		t.Errorf("%d updated, want the book kept whole, not counted", res.Updated)
	}

	// The part is back: the next scan reads the book again.
	_ = writer.Close()
	if err := os.Remove(part2); err != nil {
		t.Fatal(err)
	}
	silence(t, root, book+"/02.mp3", 1, "album=Real Title", "artist=Real Author")
	s.probeTimeout = 10 * time.Second
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.duration < 2.5 || got.chapters != 3 || got.scanError != "" {
		t.Errorf("after the part came back = %+v, want it read, no problem", got)
	}
}
