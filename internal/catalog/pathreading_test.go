package catalog

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// TestPathCheckOlderSnapshots: the snapshots an older server left are classified
// for the path backfill however they were written, and putting them right keeps
// what they hold.
func TestPathCheckOlderSnapshots(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	exec := func(q string, args ...any) int64 {
		t.Helper()
		res, err := c.db.ExecContext(ctx, q, args...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	pathCheck := func(p string) PathCheck {
		t.Helper()
		sigs, err := c.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sigs[p].PathCheck
	}

	// Migration 0016 wrote blank fields as "": a tag's series beside a blank
	// author is settled by the snapshot all the same.
	const orbit = "Becky Chambers/A Closed and Common Orbit"
	b := &Book{LibraryID: lib.ID, RelPath: orbit, IsFolder: true, Title: "A Closed and Common Orbit",
		Series: "Wayfarers", AddedAt: "2020-01-01"}
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE books SET scanned = json_object(
		'title', title, 'author', author, 'narrator', narrator, 'series', series,
		'series_index', CAST(series_index AS TEXT), '@indexed_at', indexed_at) WHERE id = ?`, b.ID)
	if got := pathCheck(orbit); got != PathFromSnapshot {
		t.Fatalf("a 0016 snapshot with a blank author: %d, want PathFromSnapshot", got)
	}
	if err := c.SetPathReading(ctx, lib.ID, map[string]*metadata.Metadata{orbit: nil}); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, orbit); got.Author != "Becky Chambers" || got.Series != "Wayfarers" {
		t.Fatalf("after the backfill: author %q series %q", got.Author, got.Series)
	}

	// A book an older server re-indexed after this one had: the stale snapshot's
	// revision says nothing about the row's reading, so the tags are read.
	id, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/Book"))
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE books SET series = 'A', indexed_at = '2027-01-01T00:00:00Z' WHERE id = ?`, id)
	if got := pathCheck("A/Book"); got != PathRead {
		t.Fatalf("a stale snapshot naming a revision: %d, want PathRead", got)
	}

	// A book an older server indexed: no snapshot, and chapters with no
	// scanned_title, whose titles the backfill keeps.
	oid := exec(`INSERT INTO books(library_id, rel_path, is_folder, title, author, series, indexed_at)
		VALUES(?, 'Old/Book', 1, 'Real Title', '', 'Old', '2026-01-01T00:00:00Z')`, lib.ID)
	exec(`INSERT INTO chapters(book_id, idx, title) VALUES(?, 0, 'Chapter One')`, oid)
	if got := pathCheck("Old/Book"); got != PathRead {
		t.Fatalf("an older server's row: %d, want PathRead", got)
	}
	if err := c.SetPathReading(ctx, lib.ID, map[string]*metadata.Metadata{
		"Old/Book": {Title: "Real Title", Author: "Old"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, "Old/Book"); got.Author != "Old" || got.Series != "" ||
		got.Chapters[0].Title != "Chapter One" {
		t.Fatalf("after the backfill: author %q series %q chapter %q", got.Author, got.Series, got.Chapters[0].Title)
	}

	// A snapshot already on today's baseline (re-indexed since the scan read its
	// signature) is left alone: its series naming the folder is a tag's.
	const mort = "Discworld/Mort"
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: mort, IsFolder: true, Title: "Mort",
		Series: "Discworld", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPathReading(ctx, lib.ID, map[string]*metadata.Metadata{mort: nil}); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, mort); got.Series != "Discworld" {
		t.Fatalf("a current snapshot lost its series: %q", got.Series)
	}
}

// TestPreferPathOlderSnapshot: until the scan puts it right, a snapshot read
// when a lone folder was the series resolves as it did then: in a library that
// prefers its folders the folder is no series, and its leaf's number goes with it.
func TestPreferPathOlderSnapshot(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l", MetadataSource: MetadataFromPath})
	const p = "Frank Herbert/01 - Dune"
	b := &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Dune", Series: "Frank Herbert",
		SeriesIndex: 1, AddedAt: "2020-01-01"}
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = json_remove(scanned, '$."@rev"') WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), map[string][2]string{
		FieldTitle: {"Dune", SourcePath}, FieldAuthor: {"Frank Herbert", SourcePath},
		FieldSeries: {"", ""}, FieldSeriesIndex: {"", ""},
	})
}

// TestStampScannedRev: a scan stamps today's revision on a current revision 1
// or 2 snapshot that needs no read (Signature.RevStamp), leaving its values;
// a snapshot that is no longer the row's own, or already re-read, is left as it
// is. A book is flagged for a read instead (Signature.NamesCheck) when its title
// may be its series (before revision 2: one of revision 2 had that read) or its
// leaf states a series (before revision 3).
func TestStampScannedRev(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const (
		plain   = "Matt Dinniman/Dungeon Crawler Carl/Book 8 - A Parade of Horribles"
		stale   = "Brandon Sanderson/Mistborn/01 - The Final Empire"
		series  = "Eric Vall/Without Law/Book 10"
		series2 = "Eric Vall/Without Law/Book 11"
		leaf    = "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes"
		plain2  = "Matt Dinniman/Dungeon Crawler Carl/Book 7 - The Butcher's Masquerade"
	)
	titles := map[string]string{plain: "A Parade of Horribles", stale: "The Final Empire", series: "Without Law",
		series2: "Without Law", leaf: "From The Ashes", plain2: "The Butcher's Masquerade"}
	for rel, title := range titles {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: rel, IsFolder: true, Title: title, AddedAt: "2020-01-01"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@rev"', '2') WHERE rel_path IN (?, ?, ?)`,
		series2, leaf, plain2); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = json_set(scanned, '$."@indexed_at"', 'older') WHERE rel_path = ?`, stale); err != nil {
		t.Fatal(err)
	}
	sigs, err := c.Signatures(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]bool{
		plain: {false, true}, stale: {false, false}, series: {true, false},
		series2: {false, true}, leaf: {true, false}, plain2: {false, true},
	}
	for rel, w := range want {
		if got := [2]bool{sigs[rel].NamesCheck, sigs[rel].RevStamp}; got != w {
			t.Errorf("%s: NamesCheck, RevStamp = %v, want %v", rel, got, w)
		}
	}
	// The series book re-indexed by an older server after the signature was read
	// (its indexed_at moved, the snapshot no longer its own): asked for anyway, the
	// compare-and-set leaves it. Asked for the stale one too (a row re-indexed by
	// an older server before the signature was read), it is left as it is.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET indexed_at = 'later' WHERE rel_path = ?`, series); err != nil {
		t.Fatal(err)
	}
	if err := c.StampScannedRev(ctx, lib.ID, []string{plain, stale, series, series2, plain2}); err != nil {
		t.Fatal(err)
	}
	wantRevs := map[string]string{
		plain: "4|A Parade of Horribles", stale: "1|The Final Empire", series: "1|Without Law",
		series2: "4|Without Law", leaf: "2|From The Ashes", plain2: "4|The Butcher's Masquerade",
	}
	for rel, w := range wantRevs {
		var got string
		if err := c.db.QueryRowContext(ctx, `SELECT IFNULL(json_extract(scanned, '$."@rev"'), '') || '|' || title
			FROM books WHERE rel_path = ?`, rel).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Errorf("%s = %q, want %q", rel, got, w)
		}
	}
	if sigs, err = c.Signatures(ctx, lib.ID); err != nil || sigs[plain].RevStamp || sigs[plain].NamesCheck {
		t.Errorf("stamped book's signature = %+v (err %v), want nothing to do", sigs[plain], err)
	}
}

// TestNamesCheckByRevision: a snapshot is read again only for the readings that
// came after its revision, and revision 4's tag reading only where it may read
// the book otherwise (tagReadingMayDiffer).
func TestNamesCheckByRevision(t *testing.T) {
	t.Parallel()
	const leaf = "Mark Tufo/Mark Tufo; Indian Hill; Book 04; From The Ashes"
	cases := []struct {
		name                            string
		rev                             int
		rel, format, title, series, nar string
		want                            bool
	}{
		{"plain mp3 at 3", 3, "Charles Dickens/Great Expectations", "mp3", "Great Expectations", "", "", false},
		{"plain mp3 at 4", 4, "Charles Dickens/Great Expectations", "mp3", "Great Expectations", "", "", false},
		{"m4b at 3", 3, "Some Author/Narrated.m4b", "m4b", "Narrated", "", "", true},
		{"M4A at 3", 3, "Some Author/Narrated", "M4A", "Narrated", "", "", true},
		{"m4b at 4", 4, "Some Author/Narrated.m4b", "m4b", "Narrated", "", "", false},
		{"narrator at 3", 3, "A/B", "mp3", "B", "", "Comp", true},
		{"series with its position at 3", 3, "A/B", "mp3", "B", "Discworld, book #40", "", true},
		{"plain series at 3", 3, "A/B", "mp3", "B", "Discworld", "", false},
		{"Han title at 3", 3, "Natsume/Neko", "mp3", "吾輩は猫である", "", "", true},
		{"Cyrillic path at 3", 3, "Толстой/Война и мир", "mp3", "Book", "", "", true},
		{"Latin accent at 3", 3, "A/Les Misérables", "mp3", "Les Misérables", "", "", true},
		{"punctuation only at 3", 3, "A/B – C", "mp3", "B – C", "", "", false},
		{"leaf series at 2", 2, leaf, "mp3", "From The Ashes", "", "", true},
		{"leaf series at 3", 3, leaf, "mp3", "From The Ashes", "", "", false},
		{"no revision", 0, "Charles Dickens/Great Expectations", "mp3", "Great Expectations", "", "", false},
	}
	for _, c := range cases {
		if got := namesCheck(c.rev, c.rel, true, c.format, c.title, c.series, c.nar); got != c.want {
			t.Errorf("%s: namesCheck = %v, want %v", c.name, got, c.want)
		}
	}
	if revNumber("3") != 3 || revNumber("") != 0 || revNumber("x") != 0 || revNumber("-1") != 0 {
		t.Error("revNumber misreads a revision")
	}
}
