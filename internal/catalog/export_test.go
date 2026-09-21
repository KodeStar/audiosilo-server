package catalog

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// TestSplitNames pins the contributor-splitting rule: unambiguous joiners always
// split, a comma splits only when every part still looks like a full name, and a
// single name is never broken up.
func TestSplitNames(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"Brandon Sanderson", []string{"Brandon Sanderson"}},
		{"  Brandon Sanderson  ", []string{"Brandon Sanderson"}},
		// The comma here is part of ONE name: "pere" is a single word.
		{"Alexandre Dumas, pere", []string{"Alexandre Dumas, pere"}},
		{"Dumas, Alexandre", []string{"Dumas, Alexandre"}},
		{"Martin Luther King, Jr.", []string{"Martin Luther King, Jr."}},
		// Unambiguous joiners.
		{"Terry Pratchett & Neil Gaiman", []string{"Terry Pratchett", "Neil Gaiman"}},
		{"Terry Pratchett and Neil Gaiman", []string{"Terry Pratchett", "Neil Gaiman"}},
		{"A; B", []string{"A", "B"}},
		{"A;B;C", []string{"A", "B", "C"}},
		// Every comma part has two words, so the comma splits.
		{"Terry Pratchett, Neil Gaiman", []string{"Terry Pratchett", "Neil Gaiman"}},
		{"Terry Pratchett, Neil Gaiman, and Rob Wilkins",
			[]string{"Terry Pratchett", "Neil Gaiman", "Rob Wilkins"}},
		// Mixed joiners, and the comma rule applied per chunk.
		{"Jane Doe; John Roe, Ann Poe", []string{"Jane Doe", "John Roe", "Ann Poe"}},
		{"Jane Doe; Alexandre Dumas, pere", []string{"Jane Doe", "Alexandre Dumas, pere"}},
		// Trailing/duplicated separators collapse rather than yielding blanks.
		{"A & B;", []string{"A", "B"}},
	}
	for _, tc := range cases {
		if got := splitNames(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitNames(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestFormatSeriesPosition(t *testing.T) {
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
		if got := formatSeriesPosition(tc.in); got != tc.want {
			t.Errorf("formatSeriesPosition(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRuntimeMinutes(t *testing.T) {
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
}

// TestExportLibraryBooks covers the composed envelope end to end: the header
// fields, the per-book projection, chapter counts, multi-file books counting
// once, and de-duplication of a second copy of the same book.
func TestExportLibraryBooks(t *testing.T) {
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

	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "1.2.3")
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

// TestExportLibraryBooksLeaksNoFilesystem is the guard that matters most: the
// file leaves the server, so no path, size, codec or format may appear in it.
func TestExportLibraryBooksLeaksNoFilesystem(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/srv/secret-root"})
	if _, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "Author/Secret Folder/book.m4b", Title: "Book",
		Author: "Someone", Format: "m4b", Codec: "aac", Size: 12345,
		CoverPath: "/srv/secret-root/cover.jpg", ContentHash: "deadbeef",
	}); err != nil {
		t.Fatal(err)
	}
	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	// Values that exist on the indexed row but must never reach the file. (Note
	// "format" as a JSON key is the envelope's own format marker, so the filesystem
	// fields are checked as quoted keys.)
	for _, leak := range []string{"secret-root", "Secret Folder", "book.m4b", "rel_path",
		"aac", "12345", "deadbeef", "cover", "content_hash", `"size"`, `"codec"`, `"is_folder"`} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("export leaks %q: %s", leak, raw)
		}
	}
}

// TestExportLibraryBooksPagesEveryBook checks the keyset paging loop drains a
// library larger than one page (exportPageSize) without repeating or dropping.
func TestExportLibraryBooksPagesEveryBook(t *testing.T) {
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
	exp, err := c.ExportLibraryBooks(ctx, lib.ID, "dev")
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
	c, ctx := newTestCatalog(t)
	if _, err := c.ExportLibraryBooks(ctx, 999, "dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
