package catalog

import (
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// TestABSFileLayer: an Audiobookshelf metadata.json goes over the tags (every
// field it names, normalized as an edit would be, a value its field refuses left
// out), reads as source abs, and an edit still goes over it; reverting the edit
// lands on the file's value.
func TestABSFileLayer(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Shelf/Book"
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: p, IsFolder: true,
		Title: "Tag Title", Author: "Tag Author", Narrator: "Tag Narrator", Series: "Cradle", SeriesIndex: 1,
		ABS: ABSReading{Sig: "10:1", File: &metadata.ABSFile{
			Title:       "ABS Title",
			Author:      "Andrea Davis Pinkney",
			Series:      []metadata.ABSSeries{{Name: "Cradle"}, {Name: "Other", Position: "2"}},
			Published:   "2011",
			Description: "A description.",
			ASIN:        "b005lxu2ta",
			ISBN:        "978-0-545-43705-9",
			Narrator:    "\x00bad", // refused by the field's rules: the tag's stays
		}},
	}); err != nil {
		t.Fatal(err)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]FieldValue{
		FieldTitle:       {Value: "ABS Title", Source: SourceABS},
		FieldAuthor:      {Value: "Andrea Davis Pinkney", Source: SourceABS},
		FieldNarrator:    {Value: "Tag Narrator", Source: SourceTag},
		FieldSeries:      {Value: "Cradle", Source: SourceABS},
		FieldSeriesIndex: {Value: "1", Source: SourceTag}, // no position in the file: the tag's, same series
		FieldMoreSeries:  {Value: `[{"name":"Other","position":2}]`, Source: SourceABS},
		FieldPublished:   {Value: "2011", Source: SourceABS},
		FieldDescription: {Value: "A description.", Source: SourceABS},
		FieldASIN:        {Value: "B005LXU2TA", Source: SourceABS},
		FieldISBN:        {Value: "9780545437059", Source: SourceABS},
	} {
		if got := d.Fields[field]; got.Value != want.Value || got.Source != want.Source {
			t.Errorf("%s = %q (%s), want %q (%s)", field, got.Value, got.Source, want.Value, want.Source)
		}
	}
	if b, _ := c.GetBookByPath(ctx, lib.ID, p); b.ASIN != "B005LXU2TA" || b.Title != "ABS Title" {
		t.Errorf("effective row = %q / %q", b.Title, b.ASIN)
	}

	// An edit goes over the file; its revert goes back to the file's value.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldTitle: "Edited"}}); err != nil {
		t.Fatal(err)
	}
	d, _ = c.AdminBookDetail(ctx, lib.ID, p)
	if got := d.Fields[FieldTitle]; got.Value != "Edited" || got.Source != SourceEdited || got.Scanned != "ABS Title" {
		t.Errorf("edited title = %+v, want the edit over the file's value", got)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldTitle}}); err != nil {
		t.Fatal(err)
	}
	d, _ = c.AdminBookDetail(ctx, lib.ID, p)
	if got := d.Fields[FieldTitle]; got.Value != "ABS Title" || got.Source != SourceABS {
		t.Errorf("reverted title = %+v, want the file's", got)
	}
}

// TestABSFileSeriesPosition: a series the file names without a position keeps
// the tag's only while it is the same series (any spelling).
func TestABSFileSeriesPosition(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, tc := range []struct {
		path, absSeries, absPos, wantIndex string
	}{
		{"A/Same", "the cradle", "", "4"},
		{"A/Other", "Other Series", "", ""},
		{"A/Own", "Other Series", "7", "7"},
		{"A/Range", "Omnibus", "1-3", "1-3"},
		{"A/Bad", "Bad Position", "Book One", ""},
	} {
		if _, err := c.UpsertBook(ctx, &Book{
			LibraryID: lib.ID, RelPath: tc.path, IsFolder: true, Title: "T", Series: "The Cradle", SeriesIndex: 4,
			ABS: ABSReading{Sig: "1:1", File: &metadata.ABSFile{Series: []metadata.ABSSeries{{Name: tc.absSeries, Position: tc.absPos}}}},
		}); err != nil {
			t.Fatal(err)
		}
		d, _ := c.AdminBookDetail(ctx, lib.ID, tc.path)
		if got := d.Fields[FieldSeriesIndex].Value; got != tc.wantIndex {
			t.Errorf("%s: series_index = %q, want %q", tc.path, got, tc.wantIndex)
		}
		if got := d.Fields[FieldSeries].Value; got != tc.absSeries {
			t.Errorf("%s: series = %q, want the file's %q", tc.path, got, tc.absSeries)
		}
	}
}

// TestABSFileOnlyForFolderBooks: ABS keeps metadata.json in a book's own folder,
// so a file book records none even when handed one.
func TestABSFileOnlyForFolderBooks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Loose.m4b", Title: "Tag",
		ABS: ABSReading{Sig: "1:1", File: &metadata.ABSFile{Title: "ABS", ASIN: "B000000001"}},
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := c.GetBookByPath(ctx, lib.ID, "Loose.m4b")
	if b.Title != "Tag" || b.ASIN != "" {
		t.Errorf("file book = %q / %q, want the tags alone", b.Title, b.ASIN)
	}
	sigs, _ := c.Signatures(ctx, lib.ID)
	if s := sigs["Loose.m4b"]; s.ABSUnchecked || s.ABSSig != "" {
		t.Errorf("file book signature = %+v, want nothing to read", s)
	}
}

// TestSetABSFiles: the scanner's re-read of an unchanged book records the file's
// new reading and re-resolves the book; a gone file takes its values away; a
// book never read (NULL, as migration 0046 leaves it) reads as unchecked.
func TestSetABSFiles(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Book"
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Tag Title"}); err != nil {
		t.Fatal(err)
	}
	sigs, _ := c.Signatures(ctx, lib.ID)
	if s := sigs[p]; s.ABSUnchecked || s.ABSSig != "" {
		t.Fatalf("indexed with no file: %+v, want checked, no signature", s)
	}

	if err := c.SetABSFiles(ctx, lib.ID, map[string]ABSReading{
		p:         {Sig: "5:5", File: &metadata.ABSFile{Title: "ABS Title", ASIN: "B000000002"}},
		"Gone/At": {Sig: "1:1"}, // no book there: skipped
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib.ID, p); b.Title != "ABS Title" || b.ASIN != "B000000002" {
		t.Errorf("after the read: %q / %q", b.Title, b.ASIN)
	}
	sigs, _ = c.Signatures(ctx, lib.ID)
	if s := sigs[p]; s.ABSUnchecked || s.ABSSig != "5:5" {
		t.Errorf("after the read: signature %+v", s)
	}

	if err := c.SetABSFiles(ctx, lib.ID, map[string]ABSReading{p: {}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib.ID, p); b.Title != "Tag Title" || b.ASIN != "" {
		t.Errorf("after the file went: %q / %q, want the tags back", b.Title, b.ASIN)
	}

	if _, err := c.db.ExecContext(ctx, `UPDATE books SET abs_indexed_at = NULL`); err != nil {
		t.Fatal(err)
	}
	sigs, _ = c.Signatures(ctx, lib.ID)
	if !sigs[p].ABSUnchecked {
		t.Error("a folder book never read should be unchecked")
	}
	// Re-indexed by an older server: indexed_at moved and its resolve left the
	// file out of the row, while the abs_* columns stayed. The book reads as
	// unchecked, and the same reading recorded again re-resolves it.
	reading := ABSReading{Sig: "6:6", File: &metadata.ABSFile{ASIN: "B000000003"}}
	if err := c.SetABSFiles(ctx, lib.ID, map[string]ABSReading{p: reading}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET indexed_at = 'later', asin = ''`); err != nil {
		t.Fatal(err)
	}
	sigs, _ = c.Signatures(ctx, lib.ID)
	if !sigs[p].ABSUnchecked {
		t.Error("a reading recorded against another index should be unchecked")
	}
	if err := c.SetABSFiles(ctx, lib.ID, map[string]ABSReading{p: reading}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib.ID, p); b.ASIN != "B000000003" {
		t.Errorf("after the older server's re-index: asin %q, want the file's back", b.ASIN)
	}
	if sigs, _ = c.Signatures(ctx, lib.ID); sigs[p].ABSUnchecked {
		t.Error("recorded for this index: should be checked")
	}
}
