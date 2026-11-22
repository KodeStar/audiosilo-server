package library

import (
	"fmt"
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Every book is at today's revision: the three read again and the book named
	// for its series (read, unchanged) by their readings, and the one whose title
	// isn't a series, never read, stamped (StampScannedRev), so no later scan
	// tests it again.
	wantRevs := []string{edited + "=6", vall + "=6", first + "=6", tufo + "=6", carl + "=6"}
	slices.Sort(wantRevs)
	if !slices.Equal(revs, wantRevs) {
		t.Errorf("revisions = %v, want %v", revs, wantRevs)
	}
	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rel, sig := range sigs {
		if sig.NamesCheck || sig.RevStamp {
			t.Errorf("%s after the backfill: NamesCheck %v, RevStamp %v, want neither", rel, sig.NamesCheck, sig.RevStamp)
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

// A book of several files takes its album over its first file's title tag
// that extends it (metadata.PartsTitle, server#272): each file's tag names that
// file ("Frankenstein; ... - Letters 1-4"). A book of one file keeps the title
// tag ("Portal to Nova Roma: Paris"), and so does one whose tag names its
// folder ("The Expanse Box Set: Leviathan Wakes" in "Leviathan Wakes") or is on
// its last file too (the series as the album, the book's title on every file,
// in a folder naming neither). Where the files' tags differ, the name they
// share past the album is the book's ("The Great Hunt" of "The Wheel of Time:
// The Great Hunt - The Wheel"), and only tags that share none give the album.
// One read before revision 5 is put right by the next scan from one read of
// its first file's tags, and of its last's where the title turns on them, with
// no re-index; a book of one file needs no read and is stamped.
func TestScanPartsTitle(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root reads unreadable files")
	}
	ctx := t.Context()
	ffprobe := lookFFprobe(t)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const (
		parts  = "Mary Shelley/Frankenstein"
		single = "Dennis E. Taylor/Paris"
		album  = "Frankenstein; or, The Modern Prometheus"
	)
	silence(t, root, parts+"/01.mp3", 1, "album="+album, "title="+album+" - Letters 1-4", "artist=Mary Shelley")
	silence(t, root, parts+"/02.mp3", 1, "album="+album, "title="+album+" - Chapters 1-4", "artist=Mary Shelley")
	silence(t, root, single+"/Paris.mp3", 1, "album=Portal to Nova Roma", "title=Portal to Nova Roma: Paris", "artist=Dennis E. Taylor")
	// A box set's album, each book's files titled with the book: the tag names
	// the book's folder, so it stays.
	const boxed, boxTitle = "James S. A. Corey/Leviathan Wakes", "The Expanse Box Set: Leviathan Wakes"
	silence(t, root, boxed+"/01.mp3", 1, "album=The Expanse Box Set", "title="+boxTitle, "artist=James S. A. Corey")
	silence(t, root, boxed+"/02.mp3", 1, "album=The Expanse Box Set", "title="+boxTitle, "artist=James S. A. Corey")
	// The series as the album and the book's title on every file, in a folder
	// that names neither: the last file's tag says the first's is the book's.
	const shared, sharedTitle = "J.K. Rowling/HP1", "Harry Potter and the Philosopher's Stone"
	for _, f := range []string{"/01.mp3", "/02.mp3", "/03.mp3"} {
		silence(t, root, shared+f, 1, "album=Harry Potter", "title="+sharedTitle, "artist=J.K. Rowling")
	}
	// The series as the album and each file titled "<album>: <book> <part or
	// chapter>" in a folder that names neither: the name the files share.
	const hunt, nova, athens = "Robert Jordan/Book 2", "Pat Writer/Book 2", "Pat Writer/Nova Roma 4"
	const wot, huntTitle = "The Wheel of Time", "The Wheel of Time: The Great Hunt - "
	for i, ch := range []string{"The Wheel", "The Flame", "The Dragon"} {
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", hunt, i+1), 1, "album="+wot, "title="+huntTitle+ch, "artist=Robert Jordan")
	}
	for i, part := range []string{"1", "2"} {
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", nova, i+1), 1, "album=Portal to Nova Roma", "title=Portal to Nova Roma: Paris (Part "+part+")", "artist=Pat Writer")
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", athens, i+1), 1, "album=Portal to Nova Roma", "title=Portal to Nova Roma: Athens - Part "+part, "artist=Pat Writer")
	}
	// Different chapter titles, with and without the series as a prefix or tag.
	const dragon, essays = "Robert Jordan/The Dragon Reborn", "Francis Bacon/Essays"
	for i, ch := range []string{"A Beginning", "An Ending"} {
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", dragon, i+1), 1, "album="+wot, "title="+ch)
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", essays, i+1), 1, "title="+ch)
	}
	const eye, eyeSeries = "Robert Jordan/The Eye of the World", "Robert Jordan 2/The Eye of the World"
	for i, ch := range []string{"Ravens", "An Empty Road", "The Peddler"} {
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", eye, i+1), 1, "album="+wot, "title="+wot+" - "+ch, "artist=Robert Jordan")
		silence(t, root, fmt.Sprintf("%s/%02d.mp3", eyeSeries, i+1), 1, "album="+wot, "title="+wot+" - "+ch, "artist=Robert Jordan", "series="+wot)
	}
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		parts: album, single: "Portal to Nova Roma: Paris", boxed: boxTitle, shared: sharedTitle,
		hunt: "The Great Hunt", nova: "Paris", athens: "Athens", eye: "The Eye of the World", eyeSeries: "The Eye of the World", dragon: "The Dragon Reborn", essays: "Essays",
	}
	check := func(when string) {
		t.Helper()
		for rel, title := range want {
			b, err := cat.GetBookByPath(ctx, lib.ID, rel)
			if err != nil || b.Title != title {
				t.Errorf("%s: %s = %+v (err %v), want title %q", when, rel, b, err, title)
			}
		}
	}
	check("first scan")

	// The snapshots of revision 4: the first file's title tag as the title.
	for rel, title := range map[string]string{parts: album + " - Letters 1-4", hunt: huntTitle + "The Wheel"} {
		if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$.title', ?, '$."@rev"', '4'), title = ?
			WHERE rel_path = ?`, title, title, rel); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '4') WHERE rel_path IN (?, ?)`, single, shared); err != nil {
		t.Fatal(err)
	}
	// Revision 5 kept the series album or the first file's tag. A revision 5
	// book already titled exactly like its folder can just take the stamp.
	for rel, old := range map[string]string{eye: wot, dragon: wot, essays: "A Beginning", eyeSeries: "The Eye of the World"} {
		if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$.title', ?, '$."@rev"', '5'), title = ? WHERE rel_path = ?`, old, old, rel); err != nil {
			t.Fatal(err)
		}
	}

	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sigs[eye].NamesCheck || !sigs[dragon].NamesCheck || !sigs[essays].NamesCheck || !sigs[eyeSeries].RevStamp {
		t.Fatalf("revision 5 backfill: wanted three title reads and a stamp for the matching folder")
	}

	if !sigs[parts].NamesCheck || !sigs[shared].NamesCheck || sigs[single].NamesCheck || !sigs[single].RevStamp {
		t.Fatalf("signatures: several files %+v and %+v, one file %+v; want the first two read, the other stamped",
			sigs[parts], sigs[shared], sigs[single])
	}
	// The book of one file is never read: unreadable, it keeps its reading.
	f := filepath.Join(root, filepath.FromSlash(single+"/Paris.mp3"))
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f, 0o644) })
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the title backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	check("after the backfill")
	var stale int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books
		WHERE json_extract(scanned, '$."@rev"') IS NOT '6'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("snapshots before today's revision after the backfill = %d (err %v), want 0", stale, err)
	}

	// A title that turns on the last file's tags waits for them: with that file
	// unreadable the snapshot keeps its revision and title, never the album
	// recorded on a guess, and later scans don't read it again until the book is
	// indexed again.
	rev := func() (r string) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT json_extract(scanned, '$."@rev"') FROM books WHERE rel_path = ?`, shared).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '4') WHERE rel_path = ?`, shared); err != nil {
		t.Fatal(err)
	}
	last := filepath.Join(root, filepath.FromSlash(shared+"/03.mp3"))
	if err := os.Chmod(last, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(last, 0o644) })
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	check("last file unreadable")
	if got := rev(); got != "4" {
		t.Fatalf("revision with the last file unreadable = %q, want 4 (left as it is)", got)
	}
	sigs, err = cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sigs[shared].TagReadFailed {
		t.Fatalf("an unopenable last file = %+v, want the read remembered as failed (not made on every scan)", sigs[shared])
	}
	if err := os.Chmod(last, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	if got := rev(); got != "4" {
		t.Fatalf("revision on the next scan = %q, want 4 (not read again before the book is indexed)", got)
	}
	// Its files changed (as a Re-read would), the book is indexed again.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(last, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	check("last file readable again")
	if got := rev(); got != "6" {
		t.Fatalf("revision once the last file is read = %q, want 6", got)
	}
}

// A book one folder deep whose snapshot alone would settle its path reading
// (catalog.PathFromSnapshot) but whose title may be the album naming its series
// (catalog.Signature.NamesCheck) waits for a read of its tags: unreadable, it
// keeps its revision for the next scan rather than recording today's with the
// title unread, and the next readable scan puts both right.
func TestScanNamesCheckWaitsForRead(t *testing.T) {
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
	reading("readable again", "From The Ashes", "6")
}

// A leaf named "Author; Series; Book NN[; Title]" states the book's series and
// position (metadata.DeriveFromPath; a range for "Book 01-03", none for "Book
// XX" or "Book 00") where the tags state no series, and a series tag still
// wins. A book read before that (scanned revision 2, no series) is put right by
// the next scan from one read of its tags, with no re-index; a book whose leaf
// isn't so named is only stamped, and an edit stays on top.
func TestScanLeafStatesSeries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// ffprobe reads the MP3's series tags (TXXX), which the tag library doesn't.
	ffprobe := lookFFprobe(t)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	tufo := "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes"
	rules := "Aaron Oster/Aaron Oster; The Rules; Book 01-03"
	apoc := "Tao Wong/Tao Wong; System Apocalypse; Book 10-12; Omnibus Volume 4"
	army := "B.V. Larson/B.V. Larson; Star Force; Book XX; Army of One"
	tokyo := "Larry Correia/Larry Correia; Grimnoir Chronicles; Book 00; Tokyo Raider"
	tagged := "A.G. Henley/A.G. Henley; Viridian Gate Online; Book 02"
	edited := "A.R. Wise/A.R. Wise; Deadlocked; Book 01"
	carl := "Matt Dinniman/Dungeon Crawler Carl/Book 8 - A Parade of Horribles"
	silence(t, root, tufo+"/"+filepath.Base(tufo)+".mp3", 1, "album=Indian Hill", "title=From The Ashes", "artist=Mark Tufo")
	silence(t, root, rules+"/"+filepath.Base(rules)+".mp3", 1, "album=The Rules: The Complete Series", "artist=Aaron Oster")
	silence(t, root, apoc+"/01.mp3", 1, "album=The System Apocalypse, Books 10-12", "artist=Tao Wong")
	silence(t, root, army+"/01.mp3", 1, "album=Army of One", "artist=B.V. Larson")
	silence(t, root, tokyo+"/01.mp3", 1, "album=Tokyo Raider", "artist=Larry Correia")
	silence(t, root, tagged+"/01.mp3", 1, "album=Cost of Power", "series=The Viridian Gate Archives", "series-part=2")
	silence(t, root, edited+"/01.mp3", 1, "album=Deadlocked 1", "artist=A.R. Wise")
	silence(t, root, carl+"/01.mp3", 1, "album=A Parade of Horribles", "artist=Matt Dinniman")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	type reading struct {
		series     string
		start, end float64
	}
	want := map[string]reading{
		tufo: {"Indian Hill", 4, 0}, rules: {"The Rules", 1, 3}, apoc: {"System Apocalypse", 10, 12},
		army: {"Star Force", 0, 0}, tokyo: {"Grimnoir Chronicles", 0, 0}, tagged: {"The Viridian Gate Archives", 2, 0},
		edited: {"Deadlocked", 1, 0}, carl: {"Dungeon Crawler Carl", 8, 0},
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
			if got := (reading{b.Series, b.SeriesIndex, b.SeriesIndexEnd}); got != want[b.Path] {
				t.Errorf("%s: %s = %+v, want %+v", when, b.Path, got, want[b.Path])
			}
		}
	}
	check("first scan")

	// The snapshots a server before revision 3 wrote: no series or position from
	// the leaf (a series tag's stays), every one at revision 2.
	if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '2')`); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{tufo, rules, apoc, army, tokyo, edited} {
		if _, err := db.ExecContext(ctx, `UPDATE books
			SET scanned = json_remove(scanned, '$.series', '$.series_index'),
			    series = '', series_key = '', series_index = 0, series_index_end = 0
			WHERE rel_path = ?`, rel); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.EditBook(ctx, lib.ID, edited, catalog.BookEdit{Set: map[string]string{catalog.FieldSeries: "Deadlocked Saga"}}); err != nil {
		t.Fatal(err)
	}
	want[edited] = reading{"Deadlocked Saga", 1, 0}
	// A book whose leaf states no series is never read: unreadable, it keeps its
	// reading.
	f := filepath.Join(root, filepath.FromSlash(carl+"/01.mp3"))
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f, 0o644) })
	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the series backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	check("after the backfill")
	var stale int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books
		WHERE json_extract(scanned, '$."@rev"') IS NOT '6'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("snapshots before today's revision after the backfill = %d (err %v), want 0", stale, err)
	}
	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rel, sig := range sigs {
		if sig.NamesCheck || sig.RevStamp {
			t.Errorf("%s after the backfill: NamesCheck %v, RevStamp %v, want neither", rel, sig.NamesCheck, sig.RevStamp)
		}
	}
	// The edit reverted, the backfilled series is what shows.
	if err := cat.EditBook(ctx, lib.ID, edited, catalog.BookEdit{Revert: []string{catalog.FieldSeries}}); err != nil {
		t.Fatal(err)
	}
	want[edited] = reading{"Deadlocked", 1, 0}
	check("after clearing the edit")
}

// Revision 4 (server#175): a snapshot an older server wrote, before the tag
// reading of 2.4.0 and 2.5.0, is read again once where that reading may differ
// (an MP4's \xa9nrt narrator over its composer) and stamped where it can't (an
// MP3 with an ASCII title and no narrator), all with no re-index.
func TestScanTagReadingRevision(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	ffprobe := lookFFprobe(t)
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	m4b := "Some Author/Narrated"
	src, err := os.ReadFile(filepath.Join("..", "metadata", "testdata", "nrt-composer.m4b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(m4b)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(m4b), "Narrated.m4b"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	plain := "Charles Dickens/Great Expectations"
	silence(t, root, plain+"/01.mp3", 1, "artist=Charles Dickens", "album=Great Expectations")
	lib, _ := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	s := NewScanner(cat, ffprobe, slog.Default())
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	narrator := func() string {
		t.Helper()
		var n string
		if err := db.QueryRowContext(ctx, `SELECT narrator FROM books WHERE rel_path = ?`, m4b).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := narrator(); n != "The Narrator" {
		t.Fatalf("first scan: narrator = %q, want the \\xa9nrt item's", n)
	}

	// What 2.4.0 wrote: the composer as narrator, at revision 3; the MP3 at 3 too.
	if _, err := db.ExecContext(ctx, `UPDATE books SET narrator = 'A Composer',
		scanned = json_set(scanned, '$.narrator', 'A Composer') WHERE rel_path = ?`, m4b); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '3')`); err != nil {
		t.Fatal(err)
	}
	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := [2]bool{sigs[m4b].NamesCheck, sigs[m4b].RevStamp}; got != [2]bool{true, false} {
		t.Errorf("m4b: NamesCheck, RevStamp = %v, want a read", got)
	}
	if got := [2]bool{sigs[plain].NamesCheck, sigs[plain].RevStamp}; got != [2]bool{false, true} {
		t.Errorf("mp3: NamesCheck, RevStamp = %v, want a stamp", got)
	}

	res, err := s.Scan(ctx, *lib)
	if err != nil || res.Updated != 0 {
		t.Fatalf("rescan = %+v (err %v), want the narrator backfilled, nothing re-indexed", res.ScanCounts, err)
	}
	if n := narrator(); n != "The Narrator" {
		t.Errorf("after the backfill: narrator = %q, want the \\xa9nrt item's", n)
	}
	var stale int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM books
		WHERE json_extract(scanned, '$."@rev"') IS NOT '6'`).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("snapshots before today's revision after the backfill = %d (err %v), want 0", stale, err)
	}
	if sigs, err = cat.Signatures(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	for rel, sig := range sigs {
		if sig.NamesCheck || sig.RevStamp {
			t.Errorf("%s after the backfill: NamesCheck, RevStamp = %v, %v, want nothing to do", rel, sig.NamesCheck, sig.RevStamp)
		}
	}
}

func TestScanLeafNamesRevisionSix(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	cat := catalog.New(db, time.Now)
	root := t.TempDir()
	const (
		leaf  = "Bea Writer; The Crossing; Books 01-03; Homecoming"
		same  = "Shelf/Bea Writer; The Crossing; Book 01-03; The Ferryman"
		other = "Shelf/Bea Writer; The Crossing; Book 01-03; Far Bank"
		// The tag spells the leaf's series without its article: the same series.
		spelled = "Shelf/Bea Writer; The Crossing; Book 05; Second Light"
		disc    = "Bea Writer/Bea Writer; The Crossing; Book 04; Low Water/CD1"
		plain   = "Plain Name.mp3"
	)
	silence(t, root, leaf+"/01.mp3", 1)
	silence(t, root, same+"/01.mp3", 1, "series=THE CROSSING")
	silence(t, root, other+"/01.mp3", 1, "series=River Stories")
	silence(t, root, spelled+"/01.mp3", 1, "series=Crossing")
	silence(t, root, disc+"/01.mp3", 1)
	silence(t, root, plain, 1)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "L", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s := NewScanner(cat, lookFFprobe(t), slog.Default())
	type names struct {
		title, author, series string
		start, end            float64
	}
	want := map[string]names{
		leaf:    {"Homecoming", "Bea Writer", "The Crossing", 1, 3},
		same:    {"The Ferryman", "Shelf", "THE CROSSING", 1, 3},
		other:   {"Far Bank", "Shelf", "River Stories", 0, 0},
		spelled: {"Second Light", "Shelf", "Crossing", 5, 0},
		disc:    {"Low Water", "Bea Writer", "The Crossing", 4, 0},
		plain:   {"Plain Name", "", "", 0, 0},
	}
	scan := func() {
		t.Helper()
		if _, err := s.Scan(ctx, *lib); err != nil {
			t.Fatal(err)
		}
		for path, w := range want {
			b, err := cat.GetBookByPath(ctx, lib.ID, path)
			if err != nil {
				t.Fatal(err)
			}
			if got := (names{b.Title, b.Author, b.Series, b.SeriesIndex, b.SeriesIndexEnd}); got != w {
				t.Errorf("%s: got %+v, want %+v", path, got, w)
			}
		}
	}
	scan()
	// Seed the revision 5 readings. The disc had just one file, so only
	// recognizing its parent as a leaf can schedule its reread.
	for path, old := range map[string]names{
		leaf:  {leaf, "", "", 0, 0},
		same:  {filepath.Base(same), "Shelf", "THE CROSSING", 1, 3},
		other: {filepath.Base(other), "Shelf", "River Stories", 1, 3},
		disc:  {"CD1", "Bea Writer", "Bea Writer; The Crossing; Book 04; Low Water", 0, 0},
	} {
		if _, err := db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '5',
			'$.title', ?, '$.series', ?, '$.series_index', ?, '$.author', ?),
			title = ?, series = ?, series_index = ?, series_index_end = ?, author = ? WHERE rel_path = ?`,
			old.title, old.series, catalog.FormatSeriesRange(old.start, old.end), old.author,
			old.title, old.series, old.start, old.end, old.author, path); err != nil {
			t.Fatal(err)
		}
	}
	scan()
	sigs, err := cat.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	for path, sig := range sigs {
		if sig.NamesCheck || sig.RevStamp {
			t.Errorf("%s still needs a backfill", path)
		}
	}
}
