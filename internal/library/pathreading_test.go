package library

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	t.Parallel()
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

// An album tag naming only the book's series gives way to the book's own title
// (metadata.TitleNotSeries), and a book read before that (scanned revision 1,
// its title the series) is put right by the next scan from one read of its
// tags, with no re-index. A book whose album is its title, or whose title isn't
// its series, keeps it; an edit stays on top.
func TestScanTitleNotSeries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ffprobe := lookFFprobe(t)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	tufo := "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes"
	vall := "Eric Vall/Without Law/Book 10"
	first := "Mark Tufo/Mark Tufo; Indian Hill; Book 01"
	carl := "Matt Dinniman/Dungeon Crawler Carl/Book 8 - A Parade of Horribles"
	edited := "John O'Brien/John O'Brien; Lifting the Veil; Book 03; Emergence"
	silence(t, root, tufo+"/"+filepath.Base(tufo)+".mp3", 1, "album=Indian Hill", "title=From The Ashes", "artist=Mark Tufo")
	silence(t, root, vall+"/Book 10 - Without Law.mp3", 1, "album=Without Law", "title=Without Law 10", "artist=Eric Vall")
	silence(t, root, first+"/"+filepath.Base(first)+".mp3", 1, "album=Indian Hill", "title=Indian Hill", "artist=Mark Tufo")
	silence(t, root, carl+"/01.mp3", 1, "album=A Parade of Horribles", "title=Chapter 1", "artist=Matt Dinniman")
	silence(t, root, edited+"/"+filepath.Base(edited)+".mp3", 1, "album=Lifting the Veil", "title=Emergence", "artist=John O'Brien")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		tufo: "From The Ashes", vall: "Without Law 10", first: "Indian Hill", carl: "A Parade of Horribles",
		edited: "Emergence",
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
			if b.Title != want[b.Path] {
				t.Errorf("%s: %s title = %q, want %q", when, b.Path, b.Title, want[b.Path])
			}
		}
	}
	check("first scan")

	// The snapshots a server before revision 2 wrote: the album as the title.
	for rel, title := range map[string]string{tufo: "Indian Hill", vall: "Without Law", edited: "Lifting the Veil"} {
		if _, err := db.ExecContext(ctx,
			`UPDATE books SET scanned = json_set(scanned, '$.title', ?, '$."@rev"', '1'), title = ? WHERE rel_path = ?`,
			title, title, rel); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '1')
		WHERE rel_path IN (?, ?)`, first, carl); err != nil {
		t.Fatal(err)
	}
	if err := cat.EditBook(ctx, lib.ID, edited, catalog.BookEdit{Set: map[string]string{catalog.FieldTitle: "Emergence (edited)"}}); err != nil {
		t.Fatal(err)
	}
	want[edited] = "Emergence (edited)"
	// A book whose stored title isn't its series is never read: unreadable, it
	// keeps its reading.
	f := filepath.Join(root, filepath.FromSlash(carl+"/01.mp3"))
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f, 0o644) })
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the titles backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	check("after the backfill")
	var revs []string
	rows, err := db.QueryContext(ctx, `SELECT rel_path || '=' || json_extract(scanned, '$."@rev"') FROM books ORDER BY rel_path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		revs = append(revs, r)
	}
	// Every book is at revision 2: the three read again and the book named for its
	// series (read, unchanged) by their readings, and the one whose title isn't a
	// series, never read, stamped (StampScannedRev), so no later scan tests it
	// again.
	wantRevs := []string{edited + "=2", vall + "=2", first + "=2", tufo + "=2", carl + "=2"}
	slices.Sort(wantRevs)
	if !slices.Equal(revs, wantRevs) {
		t.Errorf("revisions = %v, want %v", revs, wantRevs)
	}
	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rel, sig := range sigs {
		if sig.TitleCheck || sig.RevStamp {
			t.Errorf("%s after the backfill: TitleCheck %v, RevStamp %v, want neither", rel, sig.TitleCheck, sig.RevStamp)
		}
	}
	// Stamping writes the revision alone: the snapshot's stamp still matches the
	// row, and the title isn't touched.
	var stale int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books
		WHERE json_extract(scanned, '$."@indexed_at"') IS NOT indexed_at`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("snapshots no longer the row's own = %d (err %v), want 0", stale, err)
	}
	// The edit reverted, the backfilled title is what shows.
	if err := cat.EditBook(ctx, lib.ID, edited, catalog.BookEdit{Revert: []string{catalog.FieldTitle}}); err != nil {
		t.Fatal(err)
	}
	want[edited] = "Emergence"
	check("after clearing the edit")
}

// A book one folder deep whose snapshot alone would settle its path reading
// (catalog.PathFromSnapshot) but whose title may be the album naming its series
// (catalog.Signature.TitleCheck) waits for a read of its tags: unreadable, it
// keeps its revision for the next scan rather than recording today's with the
// title unread, and the next readable scan puts both right.
func TestScanTitleCheckWaitsForRead(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	ctx := t.Context()
	ffprobe := lookFFprobe(t)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	tufo := "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes"
	silence(t, root, tufo+"/01.mp3", 1, "album=Indian Hill", "title=From The Ashes", "artist=Mark Tufo")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	// An older server's snapshot: no revision, the folder as the series (and the
	// author), the album as the title.
	if _, err := db.ExecContext(ctx, `UPDATE books
		SET scanned = json_set(json_remove(scanned, '$."@rev"'), '$.title', 'Indian Hill', '$.series', 'Mark Tufo'),
		    title = 'Indian Hill', series = 'Mark Tufo'`); err != nil {
		t.Fatal(err)
	}
	reading := func(when, wantTitle string, wantRev any) {
		t.Helper()
		var title string
		var rev any
		if err := db.QueryRowContext(ctx, `SELECT title, json_extract(scanned, '$."@rev"') FROM books`).Scan(&title, &rev); err != nil {
			t.Fatal(err)
		}
		if title != wantTitle || rev != wantRev {
			t.Errorf("%s: title %q, revision %v; want %q, %v", when, title, rev, wantTitle, wantRev)
		}
	}
	f := filepath.Join(root, filepath.FromSlash(tufo+"/01.mp3"))
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f, 0o644) })
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	reading("unreadable", "Indian Hill", nil)
	if err := os.Chmod(f, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	reading("readable again", "From The Ashes", "2")
}
