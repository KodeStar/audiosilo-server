package catalog

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

func TestFormatSeriesPosition(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want string
	}{
		{0, ""},
		{2, "2"},
		{2.0, "2"},
		{2.5, "2.5"},
		{10, "10"},
		{1.25, "1.25"},
	}
	for _, tc := range cases {
		if got := FormatSeriesPosition(tc.in); got != tc.want {
			t.Errorf("FormatSeriesPosition(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRuntimeMinutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want int
	}{
		{0, 0},
		{-5, 0},
		{29, 0},      // rounds down to 0 -> omitted
		{31, 1},      // rounds up
		{36720, 612}, // 10h12m
	}
	for _, tc := range cases {
		if got := runtimeMinutes(tc.in); got != tc.want {
			t.Errorf("runtimeMinutes(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSlugifyAndFilename(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Main", "main"},
		{"My Audiobooks", "my-audiobooks"},
		{"Sci-Fi / Fantasy", "sci-fi-fantasy"},
		{"  ", ""},
	}
	for _, tc := range cases {
		if got := slugify(tc.in); got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	exp := &LibraryExport{ExportedAt: "2026-09-21T10:00:00Z"}
	exp.Library.Name = "My Audiobooks"
	if got := exp.Filename(); got != "audiosilo-my-audiobooks-2026-09-21.json" {
		t.Errorf("Filename() = %q", got)
	}
	// An unnameable library still yields a usable filename.
	exp.Library.Name = "???"
	if got := exp.Filename(); got != "audiosilo-library-2026-09-21.json" {
		t.Errorf("Filename() fallback = %q", got)
	}
	// An export with paths or tags is named apart from the one to share.
	exp.private = true
	if got := exp.Filename(); got != "audiosilo-library-2026-09-21-private.json" {
		t.Errorf("Filename() private = %q", got)
	}
}

// TestExportLibraryBooks covers the composed envelope end to end: the header
// fields, the per-book projection, chapter counts, multi-file books counting
// once, and de-duplication of a second copy of the same book.
func TestExportLibraryBooks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/main"})

	// A folder book with several files and embedded chapters.
	folder := &Book{
		LibraryID: lib.ID, RelPath: "Lee Child/Die Trying", IsFolder: true,
		Title: "Die Trying", Author: "Lee Child", Narrator: "Dick Hill",
		Series: "Jack Reacher", SeriesIndex: 2, Duration: 36720,
		ASIN: "B0TESTASIN", Format: "mp3", Size: 100,
		Files: []BookFile{
			{RelPath: "Lee Child/Die Trying/01.mp3", Seq: 0, Duration: 18360, Format: "mp3", Size: 50},
			{RelPath: "Lee Child/Die Trying/02.mp3", Seq: 1, Duration: 18360, Format: "mp3", Size: 50},
		},
		Chapters: []metadata.Chapter{
			{Index: 0, Title: "One", FilePath: "Lee Child/Die Trying/01.mp3", End: 18360},
			{Index: 1, Title: "Two", FilePath: "Lee Child/Die Trying/02.mp3", End: 18360, BookOffset: 18360},
		},
	}
	if _, err := c.UpsertBook(ctx, folder); err != nil {
		t.Fatal(err)
	}
	// A single-file book with two authors and no series position.
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Good Omens.m4b",
		Title: "Good Omens", Author: "Terry Pratchett & Neil Gaiman",
		Narrator: "Martin Jarvis", ISBN: "9780060853983", Duration: 90,
		Format: "m4b", Size: 10,
	}); err != nil {
		t.Fatal(err)
	}
	// A second copy of Die Trying in the same library: same ASIN, so it collapses.
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Duplicates/Die Trying.m4b",
		Title: "Die Trying", Author: "Lee Child", ASIN: "B0TESTASIN",
		Duration: 36720, Format: "m4b", Size: 200,
	}); err != nil {
		t.Fatal(err)
	}

	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "1.2.3", ExportOptions{})
	if err != nil {
		t.Fatalf("ExportLibraryBooks: %v", err)
	}
	if exp.Format != "audiosilo-books" || exp.Version != 1 || exp.Source != "audiosilo-server" {
		t.Fatalf("envelope header = %+v", exp)
	}
	if exp.ServerVersion != "1.2.3" {
		t.Errorf("server_version = %q, want 1.2.3", exp.ServerVersion)
	}
	if exp.Library.ID != lib.ID || exp.Library.Name != "Main" {
		t.Errorf("library = %+v", exp.Library)
	}
	if exp.ExportedAt == "" || strings.Contains(exp.ExportedAt, ".") {
		t.Errorf("exported_at = %q, want whole-second RFC3339", exp.ExportedAt)
	}
	// Three rows, two distinct books: the duplicate copy collapsed.
	if len(exp.Books) != 2 {
		t.Fatalf("books = %d, want 2: %+v", len(exp.Books), exp.Books)
	}

	byTitle := map[string]ExportBook{}
	for _, b := range exp.Books {
		byTitle[b.Title] = b
	}
	die, ok := byTitle["Die Trying"]
	if !ok {
		t.Fatalf("Die Trying missing: %+v", exp.Books)
	}
	want := ExportBook{
		Title: "Die Trying", Authors: []string{"Lee Child"}, Narrators: []string{"Dick Hill"},
		Series: "Jack Reacher", SeriesPosition: "2", ASIN: "B0TESTASIN",
		RuntimeMin: 612, Chapters: 2,
	}
	if !reflect.DeepEqual(die, want) {
		t.Errorf("Die Trying = %#v, want %#v", die, want)
	}
	omens := byTitle["Good Omens"]
	if !reflect.DeepEqual(omens.Authors, []string{"Terry Pratchett", "Neil Gaiman"}) {
		t.Errorf("Good Omens authors = %#v", omens.Authors)
	}
	if omens.SeriesPosition != "" || omens.Series != "" || omens.Chapters != 0 {
		t.Errorf("Good Omens should omit series/chapters: %#v", omens)
	}
	if omens.ISBN != "9780060853983" || omens.RuntimeMin != 2 {
		t.Errorf("Good Omens isbn/runtime = %q/%d", omens.ISBN, omens.RuntimeMin)
	}
}

// TestExportLibraryBooksMergesDuplicateCopies pins what the collapsed entry
// carries. Copies are indexed independently, so the copy that sorts first can be
// the sparsely-tagged rip; the kept entry must still carry every fact the library
// holds for that book, not just the ones the first-sorting row happened to have.
func TestExportLibraryBooksMergesDuplicateCopies(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/main"})

	// Indexed first (so it sorts first on the (title, id) keyset): an old rip with
	// nothing but the title, author and asin.
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Rips/Die Trying.mp3",
		Title: "Die Trying", Author: "Lee Child", ASIN: "B0TESTASIN",
		Format: "mp3", Size: 10,
	}); err != nil {
		t.Fatal(err)
	}
	// Indexed second: the fully-tagged copy of the same book (same asin).
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Lee Child/Die Trying", IsFolder: true,
		Title: "Die Trying", Author: "Lee Child", Narrator: "Dick Hill",
		Series: "Jack Reacher", SeriesIndex: 2, Duration: 36720,
		ASIN: "B0TESTASIN", ISBN: "9780515123333", Format: "m4b", Size: 100,
		Chapters: []metadata.Chapter{
			{Index: 0, Title: "One", End: 18360},
			{Index: 1, Title: "Two", End: 36720, BookOffset: 18360},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Only the later copy knows its publication date, other series and blurb.
	if err := c.EditBook(ctx, lib.ID, "Lee Child/Die Trying", BookEdit{Set: map[string]string{
		FieldPublished: "1998-07-01", FieldDescription: "Reacher is kidnapped.",
		FieldMoreSeries: `[{"name":"Reacher Omnibus","position":1,"position_end":3}]`,
	}}); err != nil {
		t.Fatal(err)
	}

	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev", ExportOptions{IncludeDescription: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Books) != 1 {
		t.Fatalf("books = %d, want 1 (the copies collapse): %+v", len(exp.Books), exp.Books)
	}
	want := ExportBook{
		Title: "Die Trying", Authors: []string{"Lee Child"}, Narrators: []string{"Dick Hill"},
		Series: "Jack Reacher", SeriesPosition: "2", ASIN: "B0TESTASIN",
		SeriesList: []ExportSeries{{Name: "Jack Reacher", Position: "2"}, {Name: "Reacher Omnibus", Position: "1-3"}},
		Published:  "1998-07-01", Description: "Reacher is kidnapped.",
		ISBN: "9780515123333", RuntimeMin: 612, Chapters: 2,
	}
	if !reflect.DeepEqual(exp.Books[0], want) {
		t.Errorf("collapsed entry = %#v, want %#v", exp.Books[0], want)
	}
}

// TestExportLibraryBooksLeaksNoFilesystem is the guard that matters most: the
// file leaves the server, so no path, size, codec or format may appear in it.
func TestExportLibraryBooksLeaksNoFilesystem(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/srv/secret-root"})
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Author/Secret Folder/book.m4b", Title: "Book",
		Author: "Someone", Format: "m4b", Codec: "aac", Size: 12345,
		CoverPath: "/srv/secret-root/cover.jpg", ContentHash: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}
	// A tag is the owner's own note: not for a file that leaves the server either.
	if err := c.EditBook(ctx, lib.ID, "Author/Secret Folder/book.m4b", BookEdit{Set: map[string]string{FieldTags: `["Private Shelf"]`}}); err != nil {
		t.Fatal(err)
	}
	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev", ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	// Values that exist on the indexed row but must never reach the file.
	for _, leak := range []string{"secret-root", "Secret Folder", "book.m4b", "rel_path",
		"aac", "12345", "deadbeef", "cover", "content_hash", "Private Shelf"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("export leaks %q: %s", leak, raw)
		}
	}

	// The teeth: assert on the MARSHALLED key sets, not on the Go structs, and as
	// an allowlist rather than a denylist - so a field added to ExportBook (or to
	// the envelope) fails here even when it has a name the substring scan above
	// cannot see, such as "format" (the envelope's own marker) or "files".
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	assertKeys := func(what string, obj map[string]json.RawMessage, allowed ...string) {
		t.Helper()
		ok := map[string]bool{}
		for _, k := range allowed {
			ok[k] = true
		}
		for k := range obj {
			if !ok[k] {
				t.Errorf("export %s carries unexpected field %q: %s", what, k, raw)
			}
		}
	}
	assertKeys("envelope", doc,
		"format", "version", "source", "server_version", "library", "exported_at", "books")

	var libObj map[string]json.RawMessage
	if err := json.Unmarshal(doc["library"], &libObj); err != nil {
		t.Fatal(err)
	}
	assertKeys("library", libObj, "id", "name") // never "root"

	var books []map[string]json.RawMessage
	if err := json.Unmarshal(doc["books"], &books); err != nil {
		t.Fatal(err)
	}
	if len(books) != 1 {
		t.Fatalf("books = %d, want 1: %s", len(books), raw)
	}
	assertKeys("book", books[0], "title", "subtitle", "authors", "narrators",
		"series", "series_position", "series_list", "published", "publisher",
		"language", "abridged", "genres", "asin", "isbn", "runtime_min", "chapters")
}

// TestExportLibraryBooksIncludes: asked for paths, each row names the path of
// every copy collapsed into it; asked for tags, each row carries its tags; each
// only with its own option, and the leak guard's allowlist holds otherwise
// (without either, neither key appears: above).
func TestExportLibraryBooksIncludes(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/srv/secret-root"})
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "Rips/Die Trying.mp3", Title: "Die Trying", Author: "Lee Child", ASIN: "B0TESTASIN", Size: 10},
		{LibraryID: lib.ID, RelPath: "Lee Child/Die Trying", IsFolder: true, Title: "Die Trying", Author: "Lee Child",
			ASIN: "B0TESTASIN", Genres: []string{"Thriller"}, Size: 100},
		{LibraryID: lib.ID, RelPath: "Other/Book.m4b", Title: "Other", Author: "Someone", Size: 5},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// The tag is on the copy that sorts second: the collapsed row takes it.
	if err := c.EditBook(ctx, lib.ID, "Lee Child/Die Trying", BookEdit{Set: map[string]string{FieldTags: `["Owned"]`}}); err != nil {
		t.Fatal(err)
	}
	export := func(opt ExportOptions) ([]ExportBook, []map[string]json.RawMessage) {
		t.Helper()
		exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev", opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(exp.Books) != 2 {
			t.Fatalf("books = %d, want 2: %+v", len(exp.Books), exp.Books)
		}
		raw, _ := json.Marshal(exp)
		var doc struct {
			Library map[string]json.RawMessage   `json:"library"`
			Books   []map[string]json.RawMessage `json:"books"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.Library["root"]; ok || strings.Contains(string(raw), "secret-root") {
			t.Errorf("the library root leaked: %s", raw)
		}
		return exp.Books, doc.Books
	}
	keys := func(rows []map[string]json.RawMessage, allowed ...string) {
		t.Helper()
		for _, row := range rows {
			for k := range row {
				if !slices.Contains(allowed, k) {
					t.Errorf("row carries unexpected field %q: %v", k, row)
				}
			}
		}
	}

	books, rows := export(ExportOptions{IncludePaths: true})
	die, other := books[0], books[1]
	if !slices.Equal(die.Paths, []string{"Rips/Die Trying.mp3", "Lee Child/Die Trying"}) ||
		!slices.Equal(die.Genres, []string{"Thriller"}) || die.Tags != nil {
		t.Errorf("collapsed row = %+v, want both copies' paths and the genre, no tags", die)
	}
	if !slices.Equal(other.Paths, []string{"Other/Book.m4b"}) {
		t.Errorf("single row = %+v", other)
	}
	keys(rows, "title", "authors", "asin", "genres", "paths")

	books, rows = export(ExportOptions{IncludeTags: true})
	if die := books[0]; !slices.Equal(die.Tags, []string{"Owned"}) || die.Paths != nil {
		t.Errorf("collapsed row = %+v, want the tag, no paths", die)
	}
	keys(rows, "title", "authors", "asin", "genres", "tags")

	books, rows = export(ExportOptions{IncludePaths: true, IncludeTags: true})
	if die := books[0]; !slices.Equal(die.Tags, []string{"Owned"}) || len(die.Paths) != 2 {
		t.Errorf("collapsed row = %+v, want the tag and both paths", die)
	}
	keys(rows, "title", "authors", "asin", "genres", "tags", "paths")

	// Both copies tagged: the row holds every copy's tags, each once.
	if err := c.EditBook(ctx, lib.ID, "Rips/Die Trying.mp3", BookEdit{Set: map[string]string{FieldTags: `["Rip","owned"]`}}); err != nil {
		t.Fatal(err)
	}
	if books, _ = export(ExportOptions{IncludeTags: true}); !slices.Equal(books[0].Tags, []string{"Rip", "owned"}) {
		t.Errorf("collapsed row tags = %q, want both copies' tags, each once", books[0].Tags)
	}
}

// TestExportLibraryBooksPublishedSeriesDescription: every row carries its
// publication date and, for a book in more than its main series, every series
// it is in; the description only when asked for, and asking for it alone keeps
// the file the shareable one.
func TestExportLibraryBooksPublishedSeriesDescription(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/main"})
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "Pratchett/Colour", IsFolder: true, Title: "The Colour of Magic",
			Author: "Terry Pratchett", Series: "Discworld", SeriesIndex: 1},
		{LibraryID: lib.ID, RelPath: "Pratchett/Mort", IsFolder: true, Title: "Mort",
			Author: "Terry Pratchett", Series: "Discworld", SeriesIndex: 4},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "Pratchett/Colour", BookEdit{Set: map[string]string{
		FieldPublished: "1983-11-24", FieldDescription: "The first Discworld novel.",
		FieldMoreSeries: `[{"name":"Rincewind","position":1}]`,
	}}); err != nil {
		t.Fatal(err)
	}
	export := func(opt ExportOptions) (*LibraryExport, map[string]ExportBook, string) {
		t.Helper()
		exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev", opt)
		if err != nil {
			t.Fatal(err)
		}
		byTitle := map[string]ExportBook{}
		for _, b := range exp.Books {
			byTitle[b.Title] = b
		}
		raw, _ := json.Marshal(exp)
		return exp, byTitle, string(raw)
	}

	exp, books, raw := export(ExportOptions{})
	colour, mort := books["The Colour of Magic"], books["Mort"]
	if colour.Published != "1983-11-24" {
		t.Errorf("published = %q, want 1983-11-24", colour.Published)
	}
	if want := []ExportSeries{{Name: "Discworld", Position: "1"}, {Name: "Rincewind", Position: "1"}}; !reflect.DeepEqual(colour.SeriesList, want) {
		t.Errorf("series_list = %#v, want %#v", colour.SeriesList, want)
	}
	if colour.Series != "Discworld" || colour.SeriesPosition != "1" {
		t.Errorf("main series = %q %q, want it unchanged", colour.Series, colour.SeriesPosition)
	}
	// Only its main series: no list, as the player's Book.SeriesList.
	if mort.SeriesList != nil || mort.Published != "" {
		t.Errorf("Mort = %#v, want no series_list or published", mort)
	}
	if strings.Contains(raw, `"description"`) || strings.Contains(raw, "first Discworld novel") {
		t.Errorf("the default export carries a description: %s", raw)
	}
	if !strings.Contains(raw, `"series_list":[{"name":"Discworld","position":"1"},{"name":"Rincewind","position":"1"}]`) ||
		!strings.Contains(raw, `"published":"1983-11-24"`) {
		t.Errorf("wire shape: %s", raw)
	}
	if strings.HasSuffix(exp.Filename(), "-private.json") {
		t.Errorf("Filename() = %q, want the shareable one", exp.Filename())
	}

	exp, books, _ = export(ExportOptions{IncludeDescription: true})
	if got := books["The Colour of Magic"].Description; got != "The first Discworld novel." {
		t.Errorf("description = %q", got)
	}
	if got := books["Mort"].Description; got != "" {
		t.Errorf("Mort description = %q, want none", got)
	}
	if strings.HasSuffix(exp.Filename(), "-private.json") {
		t.Errorf("Filename() with descriptions = %q, want the shareable one", exp.Filename())
	}
}

// TestMergeExportSeries: merged copies keep the main series and its position
// as one fact, and series_list lists every series either copy lists with the
// row's main one first, never one copy's list without the row's main series.
func TestMergeExportSeries(t *testing.T) {
	t.Parallel()
	type ser = ExportSeries
	for _, tc := range []struct {
		name     string
		dst, src ExportBook
		want     ExportBook
	}{{
		name: "no series takes the copy's whole",
		dst:  ExportBook{},
		src:  ExportBook{Series: "Jack Reacher", SeriesPosition: "2", SeriesList: []ser{{"Jack Reacher", "2"}, {"Reacher Omnibus", "1-3"}}},
		want: ExportBook{Series: "Jack Reacher", SeriesPosition: "2", SeriesList: []ser{{"Jack Reacher", "2"}, {"Reacher Omnibus", "1-3"}}},
	}, {
		name: "the same series fills its position and list",
		dst:  ExportBook{Series: "Jack Reacher"},
		src:  ExportBook{Series: "jack reacher", SeriesPosition: "2", SeriesList: []ser{{"jack reacher", "2"}, {"Reacher Omnibus", "1-3"}}},
		want: ExportBook{Series: "Jack Reacher", SeriesPosition: "2", SeriesList: []ser{{"Jack Reacher", "2"}, {"Reacher Omnibus", "1-3"}}},
	}, {
		name: "another series never lends its position",
		dst:  ExportBook{Series: "Discworld"},
		src:  ExportBook{Series: "City Watch", SeriesPosition: "1"},
		want: ExportBook{Series: "Discworld"},
	}, {
		name: "a list from a copy with no main series keeps the row's main first",
		dst:  ExportBook{Series: "Jack Reacher", SeriesPosition: "2"},
		src:  ExportBook{SeriesList: []ser{{"Reacher Omnibus", "1-3"}}},
		want: ExportBook{Series: "Jack Reacher", SeriesPosition: "2", SeriesList: []ser{{"Jack Reacher", "2"}, {"Reacher Omnibus", "1-3"}}},
	}, {
		name: "a row with only a list gains the copy's main series first",
		dst:  ExportBook{SeriesList: []ser{{"Reacher Omnibus", "1-3"}}},
		src:  ExportBook{Series: "Jack Reacher", SeriesPosition: "2", SeriesList: []ser{{"Jack Reacher", "2"}, {"Collected", "4"}}},
		want: ExportBook{Series: "Jack Reacher", SeriesPosition: "2", SeriesList: []ser{{"Jack Reacher", "2"}, {"Reacher Omnibus", "1-3"}, {"Collected", "4"}}},
	}, {
		name: "a list naming only the main series is no list",
		dst:  ExportBook{Series: "Jack Reacher", SeriesPosition: "2"},
		src:  ExportBook{SeriesList: []ser{{"Jack Reacher", "2"}}},
		want: ExportBook{Series: "Jack Reacher", SeriesPosition: "2"},
	}} {
		dst := tc.dst
		mergeExportSeries(&dst, tc.src)
		if !reflect.DeepEqual(dst, tc.want) {
			t.Errorf("%s: got %#v, want %#v", tc.name, dst, tc.want)
		}
	}
}

// TestExportLibraryBooksSeparateCopies: with SeparateCopies, every indexed copy
// is its own row with its own path and tags; without it (the default) copies of
// the same book still merge.
func TestExportLibraryBooksSeparateCopies(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/main"})
	for _, b := range []*Book{
		{LibraryID: lib.ID, RelPath: "Rips/Die Trying.mp3", Title: "Die Trying", Author: "Lee Child", ASIN: "B0TESTASIN"},
		{LibraryID: lib.ID, RelPath: "Lee Child/Die Trying", IsFolder: true, Title: "Die Trying", Author: "Lee Child",
			Narrator: "Dick Hill", ASIN: "B0TESTASIN"},
	} {
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for path, tags := range map[string]string{"Rips/Die Trying.mp3": `["Rip"]`, "Lee Child/Die Trying": `["Owned"]`} {
		if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldTags: tags}}); err != nil {
			t.Fatal(err)
		}
	}

	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev", ExportOptions{IncludePaths: true, IncludeTags: true, SeparateCopies: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Books) != 2 {
		t.Fatalf("books = %d, want 2 (one per copy): %+v", len(exp.Books), exp.Books)
	}
	byPath := map[string]ExportBook{}
	for _, b := range exp.Books {
		if len(b.Paths) != 1 {
			t.Fatalf("row paths = %q, want its own path alone", b.Paths)
		}
		byPath[b.Paths[0]] = b
	}
	if rip := byPath["Rips/Die Trying.mp3"]; !slices.Equal(rip.Tags, []string{"Rip"}) || rip.Narrators != nil {
		t.Errorf("rip row = %#v, want its own tag and no narrator taken from the other copy", rip)
	}
	if full := byPath["Lee Child/Die Trying"]; !slices.Equal(full.Tags, []string{"Owned"}) ||
		!slices.Equal(full.Narrators, []string{"Dick Hill"}) {
		t.Errorf("tagged row = %#v, want its own tag and narrator", full)
	}

	// The default still merges the copies into one row.
	exp, err = c.ExportLibraryBooks(ctx, lib.ID, "dev", ExportOptions{IncludePaths: true, IncludeTags: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Books) != 1 || len(exp.Books[0].Paths) != 2 || !slices.Equal(exp.Books[0].Tags, []string{"Rip", "Owned"}) {
		t.Errorf("merged = %+v, want one row with both paths and both tags", exp.Books)
	}
}

// TestExportLibraryBooksPagesEveryBook checks the keyset paging loop drains a
// library larger than one page (exportPageSize) without repeating or dropping.
func TestExportLibraryBooksPagesEveryBook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Big", Root: "/tmp/big"})
	const n = exportPageSize + 25
	for i := range n {
		title := "Book " + string(rune('A'+i%26)) + "-" + strconv.Itoa(i)
		if _, err := c.UpsertBook(ctx, &Book{
			LibraryID: lib.ID, RelPath: title + ".m4b", Title: title, Author: "Author " + strconv.Itoa(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev", ExportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(exp.Books) != n {
		t.Fatalf("books = %d, want %d", len(exp.Books), n)
	}
	seen := map[string]bool{}
	for _, b := range exp.Books {
		if seen[b.Title] {
			t.Fatalf("duplicate book in export: %q", b.Title)
		}
		seen[b.Title] = true
	}
}

func TestExportLibraryBooksUnknownLibrary(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	if _, err := c.ExportLibraryBooks(ctx, 999, "dev", ExportOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
