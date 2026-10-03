package catalog

import (
	"context"
	"errors"
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
	if err := c.SetCover(ctx, lib.ID, "old/Book", pngBytes, 0); err != nil {
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
		'series_index', CAST(series_index AS TEXT)) WHERE id = ?`, b.ID); err != nil {
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
	if len(d.Listeners) != 1 || d.Listeners[0].Username != "u" || d.Listeners[0].Position != 60 {
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
	if err := c.SetCover(ctx, lib.ID, "A/B", pngBytes, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, lib.ID, "A/./B", jpegBytes, 0); err != nil {
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
		if err := c.SetCover(ctx, lib.ID, tc.path, tc.data, 0); !errors.Is(err, tc.want) {
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
