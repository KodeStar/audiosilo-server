package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// scannedBook is what a scan of "Author/Series/01 - Title" would upsert: tags
// supplied the title and author (they differ from the path's), the path the series
// and position.
func scannedBook(libID int64, path string) *Book {
	return &Book{
		LibraryID: libID, RelPath: path, IsFolder: true,
		Title: "Original Name", Author: "Tag Author", Series: "Series", SeriesIndex: 1, Narrator: "Reader",
		AddedAt: "2020-01-01T00:00:00Z", Duration: 3600,
		Chapters: []metadata.Chapter{{Index: 0, Title: "Opening", FilePath: path + "/a.m4b", End: 1800},
			{Index: 1, Title: "Track 2", FilePath: path + "/a.m4b", Start: 1800, End: 3600, BookOffset: 1800}},
	}
}

func mustBook(t *testing.T, c *Catalog, ctx context.Context, libID int64, path string) *Book {
	t.Helper()
	b, err := c.GetBookByPath(ctx, libID, path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	return b
}

func searchTitles(t *testing.T, c *Catalog, ctx context.Context, libID int64, q string) []string {
	t.Helper()
	books, err := c.Search(ctx, q, []Scope{{LibraryID: libID, AllowAll: true}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range books {
		out = append(out, b.Title)
	}
	return out
}

// TestOverrideSurvivesRescanAndRebuild is the lock: an edit is the effective value
// for every reader, a rescan that rewrites the scanned values keeps it, and so does
// a full index rebuild (the books row deleted and re-created).
func TestOverrideSurvivesRescanAndRebuild(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	const p = "Author/Series/01 - Title"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Set:        map[string]string{FieldTitle: "Edited Title", FieldSeriesIndex: "2.5", FieldPublished: "2011"},
		ChapterSet: map[int]string{1: "The Storm"},
		UserID:     uid,
	}); err != nil {
		t.Fatal(err)
	}

	check := func(stage string) {
		t.Helper()
		b := mustBook(t, c, ctx, lib.ID, p)
		if b.Title != "Edited Title" || b.SeriesIndex != 2.5 || b.Published != "2011" || b.Author != "Tag Author" {
			t.Fatalf("%s: effective values wrong: %+v", stage, b)
		}
		if b.Chapters[1].Title != "The Storm" || b.Chapters[0].Title != "Opening" {
			t.Fatalf("%s: chapter titles wrong: %+v", stage, b.Chapters)
		}
		if got := searchTitles(t, c, ctx, lib.ID, "edited"); len(got) != 1 {
			t.Fatalf("%s: search for the edited title found %v", stage, got)
		}
		if got := searchTitles(t, c, ctx, lib.ID, "original"); len(got) != 0 {
			t.Fatalf("%s: search still finds the scanned title: %v", stage, got)
		}
	}
	check("after edit")

	// A rescan writes the scanned values again (the file changed); the edit holds.
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	check("after rescan")

	// A rebuild: the index row is pruned and indexed afresh.
	if _, err := c.DeleteBooksNotIn(ctx, lib.ID, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	check("after rebuild")
}

func TestRevertRestoresScannedValues(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Series/01 - Title"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Set:        map[string]string{FieldTitle: "Edited", FieldAuthor: "Edited Author"},
		ChapterSet: map[int]string{0: "Prologue"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldTitle}, ChapterRevert: []int{0}}); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, p)
	if b.Title != "Original Name" || b.Author != "Edited Author" || b.Chapters[0].Title != "Opening" {
		t.Fatalf("revert restored the wrong values: title=%q author=%q chapter=%q", b.Title, b.Author, b.Chapters[0].Title)
	}
	if got := searchTitles(t, c, ctx, lib.ID, "original"); len(got) != 1 {
		t.Fatalf("search should find the reverted title again: %v", got)
	}
	// Reverting something never edited is a no-op, not an error.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldNarrator}}); err != nil {
		t.Fatalf("revert of an unedited field: %v", err)
	}
}

// TestMoveReplacesStaleEnrichment: a leftover enrichment row at the destination
// (an earlier book there) must not abort the move; the moved book's wins.
func TestMoveReplacesStaleEnrichment(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	if err := c.SetEnrichment(ctx, lib.ID, "new/Book", "B0STALE000", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, lib.ID, "old/Book", "B0MOVED000", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveProgress(ctx, uid, Progress{Ref: Ref{LibraryID: lib.ID, Path: "old/Book"}, Position: 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.MoveDurableState(ctx, lib.ID, "old/Book", "new/Book"); err != nil {
		t.Fatalf("move with a stale enrichment row at the destination: %v", err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "new/Book")); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "new/Book"); b.ASIN != "B0MOVED000" {
		t.Fatalf("asin = %q, want the moved book's", b.ASIN)
	}
	if p, _ := c.GetProgress(ctx, uid, Ref{LibraryID: lib.ID, Path: "new/Book"}); p == nil || p.Position != 7 {
		t.Fatalf("progress did not move: %+v", p)
	}
}

func TestMoveCarriesOverridesAndCover(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "old/Book")); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "old/Book", BookEdit{
		Set: map[string]string{FieldTitle: "Kept"}, ChapterSet: map[int]string{1: "Kept chapter"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, lib.ID, "old/Book", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	// A stale override left at the destination by an earlier book there, on a field
	// the moved book never edited: it must not merge into the moved book's edits.
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO book_overrides(library_id, path, field, value, source, updated_at)
		 VALUES(?, 'new/Book', 'author', 'Stale Author', 'edited', '2020-01-01T00:00:00Z')`, lib.ID); err != nil {
		t.Fatal(err)
	}
	// The scanner moves durable state first, then indexes the new path.
	if err := c.MoveDurableState(ctx, lib.ID, "old/Book", "new/Book"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "new/Book")); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "new/Book")
	if b.Title != "Kept" || b.Chapters[1].Title != "Kept chapter" {
		t.Fatalf("overrides did not follow the move: %q %q", b.Title, b.Chapters[1].Title)
	}
	if b.Author != "Tag Author" {
		t.Fatalf("a stale destination override merged into the moved book: author %q", b.Author)
	}
	if _, err := c.Cover(ctx, lib.ID, "new/Book"); err != nil {
		t.Fatalf("custom cover did not follow the move: %v", err)
	}
	if _, err := c.Cover(ctx, lib.ID, "old/Book"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cover still at the old path: %v", err)
	}
}

// TestEditBooksIsAtomic: a bulk edit naming a path that isn't indexed writes
// nothing at all, and a good one edits every book.
func TestEditBooksIsAtomic(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"A/One", "A/Two"} {
		if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
			t.Fatal(err)
		}
	}
	edit := BookEdit{Set: map[string]string{FieldAuthor: "Brandon Sanderson"}}
	err := c.EditBooks(ctx, []Ref{{lib.ID, "A/One"}, {lib.ID, "A/Missing"}}, edit)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("bulk edit with a missing book = %v, want ErrNotFound", err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "A/One"); b.Author != "Tag Author" {
		t.Fatalf("a failed bulk edit still changed A/One: %q", b.Author)
	}
	if err := c.EditBooks(ctx, []Ref{{lib.ID, "A/One"}, {lib.ID, "A/Two"}}, edit); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"A/One", "A/Two"} {
		if b := mustBook(t, c, ctx, lib.ID, p); b.Author != "Brandon Sanderson" {
			t.Fatalf("%s author = %q", p, b.Author)
		}
	}
	// Chapter indexes are per book, so a bulk chapter edit is refused.
	err = c.EditBooks(ctx, []Ref{{lib.ID, "A/One"}, {lib.ID, "A/Two"}}, BookEdit{ChapterSet: map[int]string{0: "x"}})
	if !errors.Is(err, ErrInvalidOverride) {
		t.Fatalf("bulk chapter edit = %v, want ErrInvalidOverride", err)
	}
}

func TestEditValidation(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]BookEdit{
		"unknown field":      {Set: map[string]string{"cover_path": "/etc/passwd"}},
		"unknown revert":     {Revert: []string{"rel_path"}},
		"empty title":        {Set: map[string]string{FieldTitle: "  "}},
		"bad index":          {Set: map[string]string{FieldSeriesIndex: "two"}},
		"negative index":     {Set: map[string]string{FieldSeriesIndex: "-1"}},
		"bad date":           {Set: map[string]string{FieldPublished: "2021-02-30"}},
		"timestamp date":     {Set: map[string]string{FieldPublished: "2021-02-03T10:00:00Z"}},
		"bad asin":           {Set: map[string]string{FieldASIN: "short"}},
		"bad isbn":           {Set: map[string]string{FieldISBN: "12345"}},
		"control chars":      {Set: map[string]string{FieldAuthor: "a\x00b"}},
		"set and revert":     {Set: map[string]string{FieldAuthor: "x"}, Revert: []string{FieldAuthor}},
		"bad source":         {Set: map[string]string{FieldAuthor: "x"}, Source: "tag"},
		"missing chapter":    {ChapterSet: map[int]string{9: "Nope"}},
		"empty chapter":      {ChapterSet: map[int]string{0: " "}},
		"chapter set+revert": {ChapterSet: map[int]string{0: "x"}, ChapterRevert: []int{0}},
	} {
		if err := c.EditBook(ctx, lib.ID, p, edit); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s: err = %v, want ErrInvalidOverride", name, err)
		}
	}
	// Nothing above was stored.
	if b := mustBook(t, c, ctx, lib.ID, p); b.Title != "Original Name" || b.Author != "Tag Author" {
		t.Fatalf("a refused edit changed the book: %+v", b)
	}
	if err := c.EditBook(ctx, lib.ID, "A/Nope", BookEdit{Set: map[string]string{FieldAuthor: "x"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edit of an unindexed path = %v, want ErrNotFound", err)
	}
}

func TestNormalizeOverride(t *testing.T) {
	for _, tc := range []struct{ field, in, want string }{
		{FieldTitle, "  Dune  ", "Dune"},
		{FieldAuthor, "", ""},
		{FieldSeriesIndex, "2.50", "2.5"},
		{FieldSeriesIndex, "0", ""},
		{FieldSeriesIndex, "", ""},
		{FieldPublished, "1965", "1965"},
		{FieldPublished, "1965-08", "1965-08"},
		{FieldPublished, "1965-08-01", "1965-08-01"},
		{FieldASIN, " b00abc1234 ", "B00ABC1234"},
		{FieldISBN, "978-0-441-17271-9", "9780441172719"},
		{FieldISBN, "0-441-17271-x", "044117271X"},
		{FieldDescription, "Line one\r\nLine two", "Line one\nLine two"},
	} {
		got, err := normalizeOverride(tc.field, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeOverride(%s, %q) = %q, %v; want %q", tc.field, tc.in, got, err, tc.want)
		}
	}
}

// TestOverrideBeatsEnrichment: an admin's ASIN edit wins over one the manager
// attached, before and after the enrichment is (re)set, and survives a rescan.
func TestOverrideBeatsEnrichment(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, lib.ID, p, "B000000001", "9780441172719"); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldASIN: "B000000002"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, lib.ID, p, "B000000003", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, p)
	if b.ASIN != "B000000002" || b.ISBN != "9780441172719" {
		t.Fatalf("asin=%q isbn=%q; want the override and the enriched isbn", b.ASIN, b.ISBN)
	}
	// Reverting the override falls back to the enrichment, not to blank.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldASIN}}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); b.ASIN != "B000000003" {
		t.Fatalf("after revert asin = %q, want the enrichment", b.ASIN)
	}
}

func TestBookDetailProvenance(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	const p = "Author/Series/01 - Title"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, lib.ID, p, "B000000001", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldTitle: "Edited"}, UserID: uid}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{
		Set: map[string]string{FieldDescription: "From the community."}, Source: SourceCommunity,
	}); err != nil {
		t.Fatal(err)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]FieldValue{
		FieldTitle:       {Value: "Edited", Source: SourceEdited, Scanned: "Original Name", Locked: true, EditedBy: "u"},
		FieldAuthor:      {Value: "Tag Author", Source: SourceTag, Scanned: "Tag Author"},
		FieldSeries:      {Value: "Series", Source: SourcePath, Scanned: "Series"},
		FieldSeriesIndex: {Value: "1", Source: SourcePath, Scanned: "1"},
		FieldDescription: {Value: "From the community.", Source: SourceCommunity, Locked: true},
		FieldASIN:        {Value: "B000000001", Source: SourceCommunity},
		FieldISBN:        {},
	}
	for field, w := range want {
		got := d.Fields[field]
		got.EditedAt = ""
		if got != w {
			t.Errorf("%s = %+v, want %+v", field, got, w)
		}
	}
	if d.Description != "From the community." || !d.Book.Edited || d.Book.Title != "Edited" {
		t.Fatalf("detail book/description wrong: %+v / %q", d.Book, d.Description)
	}

	// The editor's account going away keeps the edit (updated_by SET NULL).
	if _, err := c.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, uid); err != nil {
		t.Fatal(err)
	}
	d, _ = c.AdminBookDetail(ctx, lib.ID, p)
	if f := d.Fields[FieldTitle]; f.Value != "Edited" || f.EditedBy != "" {
		t.Fatalf("edit after its author was deleted: %+v", f)
	}
}

// TestLegacyRowProvenance: a row backfilled by migration 0016 reads the same way
// as a fresh scan: a value the path yields is path, anything else tag.
func TestLegacyRowProvenance(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Will Wight/Cradle/01 - Unsouled"
	b := &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Unsouled", Author: "Will Wight",
		Series: "Cradle", SeriesIndex: 1, Narrator: "Travis Baldree", AddedAt: "2020-01-01"}
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	// What migration 0016 writes for a pre-existing row.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET scanned = json_object(
		'title', title, 'author', author, 'narrator', narrator, 'series', series,
		'series_index', CAST(series_index AS TEXT), '@indexed_at', indexed_at) WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	for field, src := range map[string]string{
		FieldTitle: SourcePath, FieldAuthor: SourcePath, FieldSeries: SourcePath,
		FieldSeriesIndex: SourcePath, FieldNarrator: SourceTag, FieldPublished: "",
	} {
		if got := d.Fields[field].Source; got != src {
			t.Errorf("%s source = %q, want %q", field, got, src)
		}
	}
	// The SQL-cast "1.0" reads back as the canonical "1", and an edit-then-revert
	// restores the same value a scan would have written.
	if got := d.Fields[FieldSeriesIndex].Scanned; got != "1" {
		t.Fatalf("scanned series_index = %q, want 1", got)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldSeriesIndex: "3"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldSeriesIndex}}); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, p).SeriesIndex; got != 1 {
		t.Fatalf("reverted series_index = %v, want 1", got)
	}
}

func TestBookDetailRelations(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	const p = "Author/Book"
	book := scannedBook(lib.ID, p)
	book.Codec = "aac"
	book.Files = []BookFile{{RelPath: p + "/a.m4b", Seq: 0, Duration: 1800, Format: "m4b", Codec: "aac", Size: 14_400_000},
		{RelPath: p + "/b.m4b", Seq: 1, Duration: 1800, Format: "m4b", Size: 14_400_000}}
	if _, err := c.UpsertBook(ctx, book); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SaveProgress(ctx, uid, Progress{Ref: Ref{LibraryID: lib.ID, Path: p}, Position: 60, Duration: 3600}); err != nil {
		t.Fatal(err)
	}
	inShare, _ := c.CreateShare(ctx, Share{Name: "Author"})
	_ = c.AddSharePath(ctx, inShare.ID, PathRule{LibraryID: lib.ID, Path: "Author"})
	outShare, _ := c.CreateShare(ctx, Share{Name: "Other"})
	_ = c.AddSharePath(ctx, outShare.ID, PathRule{LibraryID: lib.ID, Path: "Author/Book2"})
	if err := c.SetFolderOverride(ctx, lib.ID, p, OverrideBook); err != nil {
		t.Fatal(err)
	}

	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	// The listener carries their start date (SaveProgress stamps it), unfinished.
	if len(d.Listeners) != 1 || d.Listeners[0].Username != "u" || d.Listeners[0].Position != 60 ||
		d.Listeners[0].StartedAt == nil || d.Listeners[0].FinishedAt != nil {
		t.Fatalf("listeners = %+v", d.Listeners)
	}
	if len(d.Shares) != 1 || d.Shares[0].Name != "Author" || d.Shares[0].Path != "Author" {
		t.Fatalf("shares = %+v (a rule on a sibling must not count)", d.Shares)
	}
	if len(d.Files) != 2 || d.Files[0].Bitrate != 64000 || d.Files[1].Codec != "aac" {
		t.Fatalf("files = %+v (second part should fall back to the book codec)", d.Files)
	}
	if d.Folder != (BookFolder{Path: p, Override: OverrideBook}) {
		t.Fatalf("folder = %+v", d.Folder)
	}
	if len(d.Chapters) != 2 || d.Chapters[1].ScannedTitle != "Track 2" || d.Chapters[1].Edited {
		t.Fatalf("chapters = %+v", d.Chapters)
	}

	if _, err := c.AdminBookDetail(ctx, lib.ID, "Author/Nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detail of an unindexed path = %v, want ErrNotFound", err)
	}
}

// pngBytes and jpegBytes are just enough of each format for content sniffing.
var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
)

func TestCoverStore(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/B")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cover(ctx, lib.ID, "A/B"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no cover yet = %v", err)
	}
	if err := c.SetCover(ctx, lib.ID, "A/B", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, lib.ID, "A/./B", jpegBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	cv, err := c.Cover(ctx, lib.ID, "A/B")
	if err != nil || string(cv.Data) != string(jpegBytes) || cv.MIME != "image/jpeg" || cv.UpdatedAt == "" {
		t.Fatalf("cover = %+v %v (the second upload should replace the first)", cv, err)
	}
	if info, err := c.CoverInfo(ctx, lib.ID, "A/B"); err != nil || info.Data != nil || info.UpdatedAt != cv.UpdatedAt {
		t.Fatalf("cover info = %+v %v (no image bytes, same timestamp)", info, err)
	}
	for name, tc := range map[string]struct {
		path string
		data []byte
		want error
	}{
		"svg":       {"A/B", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), ErrUnsupportedImage},
		"html":      {"A/B", []byte("<!doctype html><script>alert(1)</script>"), ErrUnsupportedImage},
		"too large": {"A/B", append(pngBytes, make([]byte, MaxCoverBytes)...), ErrCoverTooLarge},
		"no book":   {"A/Nope", pngBytes, ErrNotFound},
	} {
		if err := c.SetCover(ctx, lib.ID, tc.path, tc.data, 0, SourceEdited); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if err := c.DeleteCover(ctx, lib.ID, "A/B"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cover(ctx, lib.ID, "A/B"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

// TestCustomCoverNeedsAnIndexedBook: a custom cover is durable state, so it outlives
// its book being pruned and returns with it, but it is served only while a book is
// indexed at the path.
func TestCustomCoverNeedsAnIndexedBook(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/B")); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, lib.ID, "A/B", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeleteBooksNotIn(ctx, lib.ID, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	for name, get := range map[string]func(context.Context, int64, string) (*CustomCover, error){
		"CoverInfo": c.CoverInfo, "Cover": c.Cover,
	} {
		if _, err := get(ctx, lib.ID, "A/B"); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s of a pruned book = %v, want ErrNotFound", name, err)
		}
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "A/B")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Cover(ctx, lib.ID, "A/B"); err != nil {
		t.Fatalf("the cover should come back with its book: %v", err)
	}
}

// stageStale leaves at path what an earlier, since-pruned book there would have:
// a field override, a chapter title and a custom cover.
func stageStale(t *testing.T, c *Catalog, ctx context.Context, libID int64, path string) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO book_overrides(library_id, path, field, value, source, updated_at) VALUES(?, ?, 'author', 'Stale Author', 'edited', 't')`,
		`INSERT INTO chapter_overrides(library_id, path, file, start_ms, title, updated_at) VALUES(?, ?, 'a.m4b', 0, 'Stale Chapter', 't')`,
		`INSERT INTO book_covers(library_id, path, mime, data, updated_at) VALUES(?, ?, 'image/png', x'00', 't')`,
	} {
		if _, err := c.db.ExecContext(ctx, q, libID, path); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMoveDropsStaleStateForAnEditedBook: when the moved book has any admin state
// (here only a chapter rename), whatever an earlier book left at the new path is
// dropped in every table - a field override, a chapter title, a custom cover - so
// none of it merges into the moved book. A moved book with no state keeps the
// path's own rows, as any book appearing there would; a self-move is a no-op.
func TestMoveDropsStaleStateForAnEditedBook(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "old/Book")); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "old/Book", BookEdit{ChapterSet: map[int]string{1: "Moved chapter"}}); err != nil {
		t.Fatal(err)
	}
	stageStale(t, c, ctx, lib.ID, "new/Book")
	if err := c.MoveDurableState(ctx, lib.ID, "old/Book", "new/Book"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "new/Book")); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "new/Book")
	if b.Author != "Tag Author" || b.Chapters[0].Title != "Opening" || b.Chapters[1].Title != "Moved chapter" {
		t.Fatalf("stale state merged into the moved book: author %q, chapters %q / %q",
			b.Author, b.Chapters[0].Title, b.Chapters[1].Title)
	}
	if _, err := c.Cover(ctx, lib.ID, "new/Book"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a stale custom cover survived the move: %v", err)
	}

	// Moving a path onto itself keeps its edits.
	if err := c.MoveDurableState(ctx, lib.ID, "new/Book", "new/Book"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "new/Book")); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "new/Book"); b.Chapters[1].Title != "Moved chapter" {
		t.Fatalf("a self-move dropped the book's edits: %q", b.Chapters[1].Title)
	}

	// A book with no state of its own takes the path's (path is the identity).
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "plain/Book")); err != nil {
		t.Fatal(err)
	}
	stageStale(t, c, ctx, lib.ID, "other/Book")
	if err := c.MoveDurableState(ctx, lib.ID, "plain/Book", "other/Book"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "other/Book")); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "other/Book"); b.Author != "Stale Author" {
		t.Fatalf("a book with no edits should keep the path's own state: author %q", b.Author)
	}
}

// TestMoveMergesAListenerCollision: a per-user row already at the new path (a
// stale progress row or favourite of the same user) no longer fails the move: the
// listener keeps the newer save whole (not the furthest: a removed book's stale
// finish there must not mark the moved book finished), under a version above both,
// and the favourite once; the book's own edits and cover move as ever.
func TestMoveMergesAListenerCollision(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	uid := seedUser(t, c, ctx)
	bob := seedNamedUser(t, c, "bob")
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "old/Book")); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "old/Book", BookEdit{Set: map[string]string{FieldTitle: "Kept"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, lib.ID, "old/Book", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	for _, s := range []struct {
		user     int64
		path     string
		pos      float64
		finished bool
		at       string
	}{
		// The user's stale row at the new path finished an earlier book there, before
		// the moving book's (newer) save.
		{uid, "old/Book", 3, false, "2026-01-02T10:00:00Z"},
		{uid, "new/Book", 7, true, "2026-01-01T10:00:00Z"},
		// bob's row at the new path is the newer save: it stays, though nearer the start.
		{bob, "old/Book", 9, false, "2026-01-01T10:00:00Z"},
		{bob, "new/Book", 4, false, "2026-01-03T10:00:00Z"},
	} {
		ref := Ref{LibraryID: lib.ID, Path: s.path}
		if _, err := c.SaveProgress(ctx, s.user, Progress{Ref: ref, Position: s.pos, Finished: s.finished,
			UpdatedAt: s.at, Version: 5}); err != nil {
			t.Fatal(err)
		}
		if s.user == uid {
			if err := c.AddFavourite(ctx, uid, ref); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.MoveDurableState(ctx, lib.ID, "old/Book", "new/Book"); err != nil {
		t.Fatalf("a listener collision failed the move: %v", err)
	}
	p, _ := c.GetProgress(ctx, uid, Ref{LibraryID: lib.ID, Path: "new/Book"})
	if p == nil || p.Position != 3 || p.Finished || p.UpdatedAt != "2026-01-02T10:00:00Z" || p.Version != 6 {
		t.Fatalf("merged progress = %+v, want the moving book's newer save (3, not finished), version 6", p)
	}
	var finishedAt *string
	if err := c.db.QueryRowContext(ctx, `SELECT finished_at FROM progress WHERE user_id = ? AND rel_path = 'new/Book'`,
		uid).Scan(&finishedAt); err != nil || finishedAt != nil {
		t.Fatalf("the stale finish date survived the move: %v (err %v)", finishedAt, err)
	}
	if p, _ := c.GetProgress(ctx, bob, Ref{LibraryID: lib.ID, Path: "new/Book"}); p == nil || p.Position != 4 ||
		p.UpdatedAt != "2026-01-03T10:00:00Z" || p.Version != 6 {
		t.Fatalf("bob's merged progress = %+v, want the newer save at the new path (4), version 6", p)
	}
	for _, u := range []int64{uid, bob} {
		if left, _ := c.GetProgress(ctx, u, Ref{LibraryID: lib.ID, Path: "old/Book"}); left != nil {
			t.Fatalf("progress left at the old path: %+v", left)
		}
	}
	favs, _ := c.ListAllFavourites(ctx, uid, []Scope{{LibraryID: lib.ID, AllowAll: true}})
	if len(favs) != 1 || favs[0].Path != "new/Book" {
		t.Fatalf("favourites = %+v, want new/Book once", favs)
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, "new/Book")); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "new/Book"); b.Title != "Kept" {
		t.Fatalf("the edit was stranded at the old path: title %q", b.Title)
	}
	if _, err := c.Cover(ctx, lib.ID, "new/Book"); err != nil {
		t.Fatalf("the custom cover was stranded at the old path: %v", err)
	}
}

// TestRowsFromAnOlderServerKeepTheirValues: a server that predates the scanned
// snapshot (an older release run against this database) writes what its scan found
// to the row itself and leaves `scanned` blank (a book it indexed) or stale (a book
// it re-indexed), and its chapters without scanned_title. Layering the durable
// tables back on must neither blank those values nor roll them back.
func TestRowsFromAnOlderServerKeepTheirValues(t *testing.T) {
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

	// A book the older server indexed: no snapshot at all.
	id := exec(`INSERT INTO books(library_id, rel_path, is_folder, title, author, series, indexed_at)
		VALUES(?, 'Old/Book', 1, 'Real Title', 'Real Author', 'Real Series', '2026-01-01T00:00:00Z')`, lib.ID)
	exec(`INSERT INTO chapters(book_id, idx, title) VALUES(?, 0, 'Chapter One')`, id)
	exec(`INSERT INTO books_fts(rowid, title, author, series, narrator) VALUES(?, 'Real Title', 'Real Author', 'Real Series', '')`, id)
	if err := c.SetEnrichment(ctx, lib.ID, "Old/Book", "B000000001", ""); err != nil {
		t.Fatal(err)
	}
	b := mustBook(t, c, ctx, lib.ID, "Old/Book")
	if b.Title != "Real Title" || b.Author != "Real Author" || b.Chapters[0].Title != "Chapter One" || b.ASIN != "B000000001" {
		t.Fatalf("an older server's row lost its values: %+v", b)
	}
	if got := searchTitles(t, c, ctx, lib.ID, "real"); len(got) != 1 {
		t.Fatalf("search lost the book: %v", got)
	}
	// What it recorded is a real snapshot: an edit reverts to it.
	if err := c.EditBook(ctx, lib.ID, "Old/Book", BookEdit{Set: map[string]string{FieldTitle: "Edited"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "Old/Book", BookEdit{Revert: []string{FieldTitle}}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, "Old/Book"); b.Title != "Real Title" {
		t.Fatalf("revert restored %q, want the older server's scanned title", b.Title)
	}

	// A book the older server re-indexed after this one had: the snapshot is from
	// the earlier scan.
	const p = "A/Book"
	id, err := c.UpsertBook(ctx, scannedBook(lib.ID, p))
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE books SET title = 'Retagged', indexed_at = '2027-01-01T00:00:00Z' WHERE id = ?`, id)
	exec(`DELETE FROM chapters WHERE book_id = ?`, id)
	exec(`INSERT INTO chapters(book_id, idx, title) VALUES(?, 0, 'Retagged chapter')`, id)
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{FieldDescription: "x"}}); err != nil {
		t.Fatal(err)
	}
	if b := mustBook(t, c, ctx, lib.ID, p); b.Title != "Retagged" || b.Chapters[0].Title != "Retagged chapter" {
		t.Fatalf("an unrelated edit rolled the re-indexed row back: title %q, chapter %q", b.Title, b.Chapters[0].Title)
	}
}

// TestNonFiniteSeriesIndexIsNoPosition: a series-part tag of "inf" parses as an
// infinite position, which no JSON reply can carry; the upsert records no position
// instead, so the admin list, book page and series aggregate still answer.
func TestNonFiniteSeriesIndexIsNoPosition(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	b := scannedBook(lib.ID, "A/Book")
	b.SeriesIndex = math.Inf(1)
	if _, err := c.UpsertBook(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got := mustBook(t, c, ctx, lib.ID, "A/Book").SeriesIndex; got != 0 {
		t.Fatalf("series_index = %v, want 0 (no position)", got)
	}
	d, err := c.AdminBookDetail(ctx, lib.ID, "A/Book")
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Sort: "series"})
	if err != nil {
		t.Fatal(err)
	}
	series, err := c.Series(ctx, 0, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"book page": d, "list": page, "series": series} {
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestDormantChapterOverrideIsNotAnEdit: an override on a chapter index the book no
// longer has (a rescan found fewer chapters) is kept, to reapply if the chapter
// returns, but it shows nowhere on the book page, so it must not mark the book
// edited; it counts again once the chapter is back.
func TestDormantChapterOverrideIsNotAnEdit(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "A/Book"
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{1: "Renamed"}}); err != nil {
		t.Fatal(err)
	}
	edited := func() bool {
		t.Helper()
		yes := true
		page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Edited: &yes}})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Books) == 1
	}
	if !edited() {
		t.Fatal("a chapter rename should mark the book edited")
	}
	one := scannedBook(lib.ID, p)
	one.Chapters = one.Chapters[:1]
	if _, err := c.UpsertBook(ctx, one); err != nil {
		t.Fatal(err)
	}
	if edited() {
		t.Fatal("an override on a chapter the book no longer has marked it edited")
	}
	if _, err := c.UpsertBook(ctx, scannedBook(lib.ID, p)); err != nil {
		t.Fatal(err)
	}
	if !edited() || mustBook(t, c, ctx, lib.ID, p).Chapters[1].Title != "Renamed" {
		t.Fatal("the override should reapply, and count, once the chapter is back")
	}
}

// TestChapterRenameFollowsTheChapter: a chapter rename is keyed on the chapter's
// file and start, so a rescan that inserts a chapter before it (a missing intro
// part turning up) keeps the rename on the same chapter, and a chapter that is
// gone (the file re-encoded with new marks) leaves the rename dormant rather than
// moving it onto whatever now sits at that position.
func TestChapterRenameFollowsTheChapter(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Book"
	upsert := func(chs ...metadata.Chapter) {
		t.Helper()
		b := scannedBook(lib.ID, p)
		for i := range chs {
			chs[i].Index = i
		}
		b.Chapters = chs
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	titles := func() []string {
		t.Helper()
		var out []string
		for _, ch := range mustBook(t, c, ctx, lib.ID, p).Chapters {
			out = append(out, ch.Title)
		}
		return out
	}
	one := metadata.Chapter{Title: "Opening", FilePath: p + "/a.m4b", End: 1800}
	two := metadata.Chapter{Title: "Track 2", FilePath: p + "/a.m4b", Start: 1800, End: 3600}
	upsert(one, two)
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{ChapterSet: map[int]string{1: "The Storm"}}); err != nil {
		t.Fatal(err)
	}

	intro := metadata.Chapter{Title: "Intro", FilePath: p + "/00 intro.m4b", End: 60}
	upsert(intro, one, two)
	if got := titles(); !reflect.DeepEqual(got, []string{"Intro", "Opening", "The Storm"}) {
		t.Fatalf("after a chapter was inserted before it: %v", got)
	}

	// Re-encoded: the renamed chapter's mark moved. Nothing is renamed, the book is
	// not counted as edited, and the rename comes back with the chapter.
	moved := two
	moved.Start = 1700
	upsert(intro, one, moved)
	if got := titles(); !reflect.DeepEqual(got, []string{"Intro", "Opening", "Track 2"}) {
		t.Fatalf("a vanished chapter's rename landed elsewhere: %v", got)
	}
	yes := true
	if page, _ := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Edited: &yes}}); len(page.Books) != 0 {
		t.Fatalf("a dormant chapter rename marked the book edited: %+v", page.Books)
	}
	upsert(intro, one, two)
	if got := titles(); got[2] != "The Storm" {
		t.Fatalf("the rename did not come back with its chapter: %v", got)
	}
}
