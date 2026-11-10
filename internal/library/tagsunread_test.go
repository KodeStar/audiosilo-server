package library

import (
	"database/sql"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A re-index whose tag read is cut short (a share that stopped answering:
// metadata.Metadata.TagsUnread) keeps the stored reading (names, embedded art
// flag, release date, genres) instead of writing the path's names over it, and
// records the file as unreadable, with ffprobe or without, so the next scan
// looks again and puts the reading right. A new book read that way takes the
// path's names, as before.
func TestReindexWithTagsUnreadKeepsTheStoredReading(t *testing.T) {
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
			silence(t, root, kept+"/01.mp3", 1, "album=Real Title", "artist=Real Author",
				"genre=Fantasy", "date=2020-05-01")
			lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
			s := NewScanner(cat, ffprobe, slog.Default())
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			type reading struct {
				title, author, genres, released string
				hasCover                        sql.NullBool
				scanError, detail               string
			}
			read := func(rel string) reading {
				t.Helper()
				var r reading
				if err := db.QueryRowContext(ctx, `SELECT title, author, genres, IFNULL(released, ''), has_cover,
					scan_error, scan_error_detail FROM books WHERE rel_path = ?`, rel).
					Scan(&r.title, &r.author, &r.genres, &r.released, &r.hasCover, &r.scanError, &r.detail); err != nil {
					t.Fatalf("%s: %v", rel, err)
				}
				return r
			}
			first := read(kept)
			if first.title != "Real Title" || first.author != "Real Author" || first.genres == "" || first.released == "" {
				t.Fatalf("first scan = %+v, want the tags' reading", first)
			}
			// As a read that found embedded art left it.
			if _, err := db.ExecContext(ctx, `UPDATE books SET has_cover = 1 WHERE rel_path = ?`, kept); err != nil {
				t.Fatal(err)
			}

			// The file changes (so the book is re-indexed) on a share that stops
			// answering: every read runs out of time. A new book appears beside it.
			f := filepath.Join(root, filepath.FromSlash(kept+"/01.mp3"))
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(f, later, later); err != nil {
				t.Fatal(err)
			}
			fresh := "Some Folder/New Arrival"
			silence(t, root, fresh+"/01.mp3", 1, "album=Tagged Title", "artist=Real Author")
			s.probeTimeout = time.Nanosecond
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			got := read(kept)
			want := reading{title: "Real Title", author: "Real Author", genres: first.genres, released: first.released,
				hasCover: sql.NullBool{Bool: true, Valid: true}, scanError: problemUnreadable, detail: tagsUnreadDetail}
			if got != want {
				t.Errorf("re-index with its tags unread = %+v, want the stored reading kept, %+v", got, want)
			}
			if got := read(fresh); got.title != "New Arrival" || got.scanError != problemUnreadable {
				t.Errorf("a new book with its tags unread = %+v, want the path's title and the problem", got)
			}

			// The share answers again: the next scan reads both again.
			s.probeTimeout = 10 * time.Second
			if _, err := s.Scan(ctx, *lib); err != nil {
				t.Fatal(err)
			}
			if got := read(kept); got.title != "Real Title" || got.scanError != "" {
				t.Errorf("after the share came back = %+v, want it read, no problem", got)
			}
			if got := read(fresh); got.title != "Tagged Title" || got.scanError != "" {
				t.Errorf("new book after the share came back = %+v, want its tags' title", got)
			}
		})
	}
}
