package library

import (
	"context"
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
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
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
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
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
