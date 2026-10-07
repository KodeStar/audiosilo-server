package catalog

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// setMetadataSource switches a library's metadata source, failing the test on an
// error.
func setMetadataSource(t *testing.T, c *Catalog, ctx context.Context, libID int64, source string) {
	t.Helper()
	if _, stale, err := c.UpdateLibrary(ctx, libID, LibraryPatch{MetadataSource: &source}); err != nil {
		t.Fatal(err)
	} else if stale {
		t.Fatal("a new metadata source asked for a rescan")
	}
}

// fieldValues is a book's resolved values and sources, for comparing.
func fieldValues(t *testing.T, c *Catalog, ctx context.Context, libID int64, path string) map[string][2]string {
	t.Helper()
	d, err := c.AdminBookDetail(ctx, libID, path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]string{}
	for _, f := range []string{FieldTitle, FieldAuthor, FieldNarrator, FieldSeries, FieldSeriesIndex} {
		out[f] = [2]string{d.Fields[f].Value, d.Fields[f].Source}
	}
	return out
}

func assertFields(t *testing.T, got, want map[string][2]string) {
	t.Helper()
	for f, w := range want {
		if got[f] != w {
			t.Errorf("%s = %q, want %q", f, got[f], w)
		}
	}
}

// TestPreferPathResolvesFromTheLayout: a library that prefers its folders takes the
// title, author, series and position from the layout over junk tags, on the row
// every reader sees (search included) and as path provenance; the tags still fill
// what the path doesn't say (the narrator), an edit stays the lock, and switching
// back restores the tags - all without a rescan.
func TestPreferPathResolvesFromTheLayout(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetLibrary(ctx, lib.ID); got.MetadataSource != MetadataFromTags {
		t.Fatalf("a new library's metadata source = %q, want tags", got.MetadataSource)
	}
	if _, err := c.CreateLibrary(ctx, Library{Name: "Bad", Root: "/tmp/b", MetadataSource: "folders"}); !errors.Is(err, ErrInvalidMetadataSource) {
		t.Fatalf("create with a bogus source = %v, want ErrInvalidMetadataSource", err)
	}
	bogus := "folders"
	if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{MetadataSource: &bogus}); !errors.Is(err, ErrInvalidMetadataSource) {
		t.Fatalf("update with a bogus source = %v, want ErrInvalidMetadataSource", err)
	}
	uid := seedUser(t, c, ctx)
	const p = "James S. A. Corey/The Expanse/03 - Abaddon's Gate"
	b := scannedBook(lib.ID, p)
	b.Title, b.Author, b.Narrator = "Abaddon's Gate: Expanse, Book 3 (Unabridged)", "James S. A. Corey", "Jefferson Mays"
	b.Series, b.SeriesIndex = "The Expanse, book #3", 0
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	tags := map[string][2]string{
		FieldTitle: {"Abaddon's Gate: Expanse, Book 3 (Unabridged)", SourceTag},
		// The tag agrees with the author folder, so it reads as the path's.
		FieldAuthor:      {"James S. A. Corey", SourcePath},
		FieldNarrator:    {"Jefferson Mays", SourceTag},
		FieldSeries:      {"The Expanse, book #3", SourceTag},
		FieldSeriesIndex: {"", ""},
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), tags)

	setMetadataSource(t, c, ctx, lib.ID, MetadataFromPath)
	path := map[string][2]string{
		FieldTitle:       {"Abaddon's Gate", SourcePath},
		FieldAuthor:      {"James S. A. Corey", SourcePath},
		FieldNarrator:    {"Jefferson Mays", SourceTag},
		FieldSeries:      {"The Expanse", SourcePath},
		FieldSeriesIndex: {"3", SourcePath},
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), path)
	if row := mustBook(t, c, ctx, lib.ID, p); row.Title != "Abaddon's Gate" || row.Series != "The Expanse" || row.SeriesIndex != 3 {
		t.Fatalf("row = %q / %q #%v, want the path's values", row.Title, row.Series, row.SeriesIndex)
	}
	if got := searchTitles(t, c, ctx, lib.ID, "Unabridged"); len(got) != 0 {
		t.Fatalf("search still finds the tag title: %v", got)
	}
	if got := searchTitles(t, c, ctx, lib.ID, "Abaddon"); !slices.Equal(got, []string{"Abaddon's Gate"}) {
		t.Fatalf("search = %v, want the path title", got)
	}

	// A rescan resolves the same way.
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), path)

	// An edit beats the path, and survives the switch back.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldTitle: "Mine"}, UserID: uid}); err != nil {
		t.Fatal(err)
	}
	setMetadataSource(t, c, ctx, lib.ID, MetadataFromTags)
	tags[FieldTitle] = [2]string{"Mine", SourceEdited}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), tags)

	// Reverting it in a path library restores the path's value.
	setMetadataSource(t, c, ctx, lib.ID, MetadataFromPath)
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldTitle}, UserID: uid}); err != nil {
		t.Fatal(err)
	}
	assertFields(t, fieldValues(t, c, ctx, lib.ID, p), path)
	if d, _ := c.AdminBookDetail(ctx, lib.ID, p); d.Fields[FieldTitle].Scanned != "Abaddon's Gate" {
		t.Fatalf("scanned title = %q, want the path's", d.Fields[FieldTitle].Scanned)
	}
}

// TestPreferPathKeepsWhatThePathDoesNotSay: where the layout names no author or
// series, the tags' stay; a tag's position stays only beside the same series.
func TestPreferPathKeepsWhatThePathDoesNotSay(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l", MetadataSource: MetadataFromPath})
	upsert := func(path string, isFolder bool, title, author, series string, idx float64) {
		t.Helper()
		b := scannedBook(lib.ID, path)
		b.IsFolder, b.Title, b.Author, b.Series, b.SeriesIndex = isFolder, title, author, series, idx
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, path    string
		isFolder      bool
		title, author string
		series        string
		idx           float64
		want          map[string][2]string
	}{
		{
			// A root file says only its title.
			name: "root file", path: "Dune.m4b", title: "Dune (Unabridged)", author: "Frank Herbert",
			series: "Dune Chronicles", idx: 1,
			want: map[string][2]string{
				FieldTitle: {"Dune", SourcePath}, FieldAuthor: {"Frank Herbert", SourceTag},
				FieldSeries: {"Dune Chronicles", SourceTag}, FieldSeriesIndex: {"1", SourceTag},
			},
		},
		{
			// One folder deep is the author, not a series: the tag's series stays.
			name: "author folder", path: "George Orwell/Animal Farm", isFolder: true, title: "Animal Farm: A Fairy Story",
			author: "Orwell, George", series: "Penguin Modern Classics", idx: 2,
			want: map[string][2]string{
				FieldTitle: {"Animal Farm", SourcePath}, FieldAuthor: {"George Orwell", SourcePath},
				FieldSeries: {"Penguin Modern Classics", SourceTag}, FieldSeriesIndex: {"2", SourceTag},
			},
		},
		{
			// Untagged, the scan's baseline took the author folder for a series;
			// the layout reads none, and that wins.
			name: "untagged author folder", path: "George Orwell/Nineteen Eighty-Four", isFolder: true,
			title: "Nineteen Eighty-Four", series: "George Orwell",
			want: map[string][2]string{
				FieldTitle: {"Nineteen Eighty-Four", SourcePath}, FieldAuthor: {"George Orwell", SourcePath},
				FieldSeries: {"", ""}, FieldSeriesIndex: {"", ""},
			},
		},
		{
			// ... and its leaf's number numbered that misread series: gone with it.
			name: "untagged numbered leaf", path: "Frank Herbert/01 - Dune", isFolder: true,
			title: "Dune", series: "Frank Herbert", idx: 1,
			want: map[string][2]string{
				FieldTitle: {"Dune", SourcePath}, FieldAuthor: {"Frank Herbert", SourcePath},
				FieldSeries: {"", ""}, FieldSeriesIndex: {"", ""},
			},
		},
		{
			// A tag's series the layout doesn't replace keeps the tag's position,
			// even one equal to the leaf's number.
			name: "tagged numbered leaf", path: "Brandon Sanderson/01 - The Way of Kings", isFolder: true,
			title: "The Way of Kings", author: "Brandon Sanderson", series: "The Stormlight Archive", idx: 1,
			want: map[string][2]string{
				FieldTitle: {"The Way of Kings", SourcePath}, FieldSeries: {"The Stormlight Archive", SourceTag},
				FieldSeriesIndex: {"1", SourcePath},
			},
		},
		{
			// A tag title that is the folder's name, number and all, says the
			// number is part of the title: it stays, and numbers no series.
			name: "number in the title", path: "Jay Asher/Teen Fiction/13 Reasons Why", isFolder: true,
			title: "13 Reasons Why", author: "Jay Asher", series: "Teen Fiction",
			want: map[string][2]string{
				FieldTitle: {"13 Reasons Why", SourcePath}, FieldSeries: {"Teen Fiction", SourcePath},
				FieldSeriesIndex: {"", ""},
			},
		},
		{
			// The same series unnumbered by the path keeps the tag's position.
			name: "same series", path: "James S. A. Corey/The Expanse/Abaddon's Gate", isFolder: true,
			title: "Abaddon's Gate", author: "James S. A. Corey", series: "the expanse", idx: 3,
			want: map[string][2]string{
				FieldSeries: {"The Expanse", SourcePath}, FieldSeriesIndex: {"3", SourceTag},
			},
		},
		{
			// Another series' position doesn't number the path's.
			name: "other series", path: "James S. A. Corey/The Expanse/Cibola Burn", isFolder: true,
			title: "Cibola Burn", author: "James S. A. Corey", series: "Space Opera Favourites", idx: 9,
			want: map[string][2]string{
				FieldSeries: {"The Expanse", SourcePath}, FieldSeriesIndex: {"", ""},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upsert(tc.path, tc.isFolder, tc.title, tc.author, tc.series, tc.idx)
			assertFields(t, fieldValues(t, c, ctx, lib.ID, tc.path), tc.want)
		})
	}
}
