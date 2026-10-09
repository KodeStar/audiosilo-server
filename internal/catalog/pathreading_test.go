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
