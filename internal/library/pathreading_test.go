package library

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// A book one folder deep takes that folder for its author, not its series. One
// read before that (no scanned revision) is put right by the next scan without a
// re-index: from its snapshot alone where that tells (the folder as series and
// author; a tag's series with no author), else from one read of its tags, which
// keeps a series tag that names the folder. An edit stays on top.
func TestScanReadsLoneFolderAsAuthor(t *testing.T) {
	ctx := t.Context()
	// ffprobe reads the MP3's series tag (TXXX), which the tag library doesn't.
	ffprobe := lookFFprobe(t)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	silence(t, root, "Charles Dickens/Great Expectations/01.mp3", 1, "artist=Charles Dickens")
	silence(t, root, "Becky Chambers/A Closed and Common Orbit/01.mp3", 1, "series=Wayfarers")
	silence(t, root, "Discworld/Mort/01.mp3", 1, "series=Discworld", "artist=Terry Pratchett")
	silence(t, root, "Ann Leckie/Ancillary Justice/01.mp3", 1)
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if err := cat.EditBook(ctx, lib.ID, "Ann Leckie/Ancillary Justice",
		catalog.BookEdit{Set: map[string]string{catalog.FieldSeries: "Imperial Radch"}}); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"Charles Dickens/Great Expectations":       {"Charles Dickens", ""},
		"Becky Chambers/A Closed and Common Orbit": {"Becky Chambers", "Wayfarers"},
		"Discworld/Mort":                           {"Terry Pratchett", "Discworld"},
		"Ann Leckie/Ancillary Justice":             {"Ann Leckie", "Imperial Radch"},
	}
	check := func(when string) {
		t.Helper()
		page, err := cat.ListAdminBooks(ctx, catalog.AdminListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Books) != len(want) {
			t.Fatalf("%s: %d books, want %d", when, len(page.Books), len(want))
		}
		for _, b := range page.Books {
			if got := [2]string{b.Author, b.Series}; got != want[b.Path] {
				t.Errorf("%s: %s = %v, want %v", when, b.Path, got, want[b.Path])
			}
		}
	}
	check("first scan")

	// The snapshots an older server wrote: no revision, the folder as the series
	// unless a tag gave one, no author unless a tag gave one.
	for _, q := range []string{
		`UPDATE books SET scanned = json_remove(scanned, '$."@rev"')`,
		`UPDATE books SET scanned = json_set(scanned, '$.series', 'Charles Dickens'), series = 'Charles Dickens'
		  WHERE rel_path = 'Charles Dickens/Great Expectations'`,
		`UPDATE books SET scanned = json_remove(scanned, '$.author'), author = ''
		  WHERE rel_path = 'Becky Chambers/A Closed and Common Orbit'`,
		`UPDATE books SET scanned = json_set(json_remove(scanned, '$.author'), '$.series', 'Ann Leckie')
		  WHERE rel_path = 'Ann Leckie/Ancillary Justice'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	// The two the snapshot settles are never read: unreadable, they're put right
	// all the same.
	for _, rel := range []string{"Charles Dickens/Great Expectations/01.mp3", "Becky Chambers/A Closed and Common Orbit/01.mp3"} {
		f := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.Chmod(f, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(f, 0o644) })
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the readings backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	check("after the backfill")
	var stale int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM books WHERE json_extract(scanned, '$."@rev"') IS NULL`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("snapshots without a revision after the backfill = %d (err %v), want 0", stale, err)
	}
}
