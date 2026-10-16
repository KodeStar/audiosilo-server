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

func TestNormalizeLabels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"[]", ""},
		{`["  Sci-Fi  ", "Space   Opera", "sci-fi", "", "SPACE OPERA", "Tom & Jerry"]`, `["Sci-Fi","Space Opera","Tom & Jerry"]`},
	} {
		got, err := normalizeLabels(FieldTags, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("normalizeLabels(%s) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	many := make([]string, maxLabels+1)
	for i := range many {
		many[i] = "t" + strconv.Itoa(i)
	}
	raw, _ := json.Marshal(many)
	for name, in := range map[string]string{
		"not a list":    `"Fantasy"`,
		"not strings":   `[1, 2]`,
		"too long":      `["` + strings.Repeat("x", maxLabelLen+1) + `"]`,
		"control":       `["a\u0007b"]`,
		"too many":      string(raw),
		"numbers mixed": `["a", 3]`,
	} {
		if _, err := normalizeLabels(FieldGenres, in); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s: err = %v, want an invalid override", name, err)
		}
	}
	// A label of exactly the limit is fine.
	if _, err := normalizeLabels(FieldTags, `["`+strings.Repeat("é", maxLabelLen)+`"]`); err != nil {
		t.Errorf("a %d-character label: %v", maxLabelLen, err)
	}
}

// What a file gives is cleaned, never refused: an odd label is left out and a
// long list cut, so the rest of the book's labels stay.
func TestCleanLabels(t *testing.T) {
	t.Parallel()
	in := []string{"Fantasy", strings.Repeat("x", maxLabelLen+1), "a\x07b", " fantasy ", "Epic"}
	if got := CleanLabels(in); !reflect.DeepEqual(got, []string{"Fantasy", "Epic"}) {
		t.Errorf("CleanLabels = %q", got)
	}
	many := make([]string, maxLabels+5)
	for i := range many {
		many[i] = "t" + strconv.Itoa(i)
	}
	if got := CleanLabels(many); len(got) != maxLabels || got[0] != "t0" {
		t.Errorf("CleanLabels kept %d labels, want the first %d", len(got), maxLabels)
	}
	if CleanLabels(nil) != nil {
		t.Error("no labels should be nil")
	}
}

func TestMergeLabels(t *testing.T) {
	t.Parallel()
	got, err := mergeLabels(FieldTags, `["Owned","Read"]`, []string{"read", "Favourite"}, []string{" OWNED "})
	if err != nil || got != `["Read","Favourite"]` {
		t.Errorf("merge = %q, %v", got, err)
	}
	if got, _ := mergeLabels(FieldTags, `["Owned"]`, nil, []string{"owned"}); got != "" {
		t.Errorf("removing the last label = %q, want none", got)
	}
	full := make([]string, maxLabels)
	for i := range full {
		full[i] = "t" + strconv.Itoa(i)
	}
	if _, err := mergeLabels(FieldTags, EncodeLabels(full), []string{"one more"}, nil); !errors.Is(err, ErrInvalidOverride) {
		t.Errorf("past the cap: err = %v, want an invalid override", err)
	}
}

// TestNormalizeNewTextFields: subtitle, publisher and language are trimmed text;
// abridged is true, false or unknown.
func TestNormalizeNewTextFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ field, in, want string }{
		{FieldSubtitle, "  A Novel ", "A Novel"},
		{FieldPublisher, "Tantor Audio", "Tantor Audio"},
		{FieldLanguage, " en-GB ", "en-GB"},
		{FieldAbridged, "TRUE", "true"},
		{FieldAbridged, "false", "false"},
		{FieldAbridged, "", ""},
	} {
		if got, err := normalizeOverride(tc.field, tc.in); err != nil || got != tc.want {
			t.Errorf("%s %q = %q, %v; want %q", tc.field, tc.in, got, err, tc.want)
		}
	}
	for field, in := range map[string]string{
		FieldAbridged: "yes", FieldLanguage: strings.Repeat("x", maxLanguage+1), FieldSubtitle: "a\nb",
	} {
		if _, err := normalizeOverride(field, in); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s %q: err = %v, want an invalid override", field, in, err)
		}
	}
}

// TestLabelLayers: genres from the tags, the ABS file's over them (with its
// tags, subtitle, publisher, language and abridged), an edit over both, and an
// edit kept through a rescan that brings other genres. The effective values
// reach the player's book (list and single reads alike).
func TestLabelLayers(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	const p = "Author/Book"
	scan := func(genres []string, abs *metadata.ABSFile) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &Book{
			LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: "Book", Author: "Author", Genres: genres,
			ABS: ABSReading{Sig: "1:1", File: abs},
		}); err != nil {
			t.Fatal(err)
		}
	}
	scan([]string{"Fantasy", "Epic"}, nil)
	d, err := c.AdminBookDetail(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Fields[FieldGenres]; got.Value != `["Fantasy","Epic"]` || got.Source != SourceTag {
		t.Errorf("scanned genres = %+v", got)
	}
	if got := d.Book.Genres; !reflect.DeepEqual(got, []string{"Fantasy", "Epic"}) {
		t.Errorf("admin row genres = %q", got)
	}

	scan([]string{"Fantasy"}, &metadata.ABSFile{
		Subtitle: "A Tale", Publisher: "Tantor", Language: "English", Abridged: true,
		Genres: []string{"Horror"}, Tags: []string{"Owned", "owned", "To Read"},
	})
	d, _ = c.AdminBookDetail(ctx, lib.ID, p)
	for field, want := range map[string]FieldValue{
		FieldSubtitle:  {Value: "A Tale", Source: SourceABS},
		FieldPublisher: {Value: "Tantor", Source: SourceABS},
		FieldLanguage:  {Value: "English", Source: SourceABS},
		FieldAbridged:  {Value: "true", Source: SourceABS},
		FieldGenres:    {Value: `["Horror"]`, Source: SourceABS},
		FieldTags:      {Value: `["Owned","To Read"]`, Source: SourceABS},
	} {
		if got := d.Fields[field]; got.Value != want.Value || got.Source != want.Source {
			t.Errorf("%s = %q (%s), want %q (%s)", field, got.Value, got.Source, want.Value, want.Source)
		}
	}
	b, err := c.GetBookByPath(ctx, lib.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if b.Subtitle != "A Tale" || b.Publisher != "Tantor" || b.Language != "English" || b.Abridged == nil || !*b.Abridged ||
		!slices.Equal(b.Genres, []string{"Horror"}) || !slices.Equal(b.Tags, []string{"Owned", "To Read"}) {
		t.Errorf("player book = %+v", b)
	}

	// An edit goes over the file, and a rescan with other genres keeps it.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Set: map[string]string{
		FieldTags: `["Mine"]`, FieldAbridged: "false",
	}}); err != nil {
		t.Fatal(err)
	}
	scan([]string{"Mystery"}, &metadata.ABSFile{Tags: []string{"Theirs"}})
	b, _ = c.GetBookByPath(ctx, lib.ID, p)
	if !slices.Equal(b.Tags, []string{"Mine"}) || b.Abridged == nil || *b.Abridged || !slices.Equal(b.Genres, []string{"Mystery"}) {
		t.Errorf("after a rescan: tags %q, abridged %v, genres %q; want the edits kept and the new genre", b.Tags, b.Abridged, b.Genres)
	}

	// Nothing says abridged: unknown, omitted on the wire.
	if err := c.EditBook(ctx, lib.ID, p, BookEdit{Revert: []string{FieldAbridged}}); err != nil {
		t.Fatal(err)
	}
	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID})
	if err != nil || len(page.Books) != 1 {
		t.Fatalf("list: %v %v", page, err)
	}
	raw, _ := json.Marshal(page.Books[0])
	if strings.Contains(string(raw), `"abridged"`) || !strings.Contains(string(raw), `"tags":["Mine"]`) ||
		!strings.Contains(string(raw), `"genres":["Mystery"]`) {
		t.Errorf("list book JSON = %s", raw)
	}
}

// labelBooks seeds a library for the filter, search and bulk tests: three books
// with their own tags and genres.
func labelBooks(t *testing.T) (*Catalog, int64) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, b := range []struct {
		path   string
		genres []string
		tags   string
	}{
		{"A", []string{"Fantasy"}, `["Owned","Favourite"]`},
		{"B", []string{"Science Fiction", "fantasy"}, `["owned"]`},
		{"C", []string{"Horror"}, ""},
	} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: b.path, Title: "Book " + b.path, Genres: b.genres}); err != nil {
			t.Fatal(err)
		}
		if b.tags != "" {
			if err := c.EditBook(ctx, lib.ID, b.path, BookEdit{Set: map[string]string{FieldTags: b.tags}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	return c, lib.ID
}

func listPaths(t *testing.T, c *Catalog, opt ListOptions) []string {
	t.Helper()
	page, err := c.ListBooks(t.Context(), opt)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range page.Books {
		out = append(out, b.RelPath)
	}
	slices.Sort(out)
	return out
}

// TestLabelFilters: genre= and tag= find a book by its label in any case, on the
// player's list and the admin list alike, and a search finds a book by a tag.
func TestLabelFilters(t *testing.T) {
	t.Parallel()
	c, lib := labelBooks(t)
	for name, tc := range map[string]struct {
		opt  ListOptions
		want []string
	}{
		"genre":          {ListOptions{Genre: "FANTASY"}, []string{"A", "B"}},
		"tag":            {ListOptions{Tag: "owned"}, []string{"A", "B"}},
		"both":           {ListOptions{Genre: "fantasy", Tag: "favourite"}, []string{"A"}},
		"spaced":         {ListOptions{Genre: "science   fiction"}, []string{"B"}},
		"none":           {ListOptions{Tag: "nobody's"}, nil},
		"genre not tag":  {ListOptions{Tag: "Horror"}, nil},
		"tag with scope": {ListOptions{Tag: "owned", Scope: &Scope{LibraryID: lib, Paths: []string{"B"}}}, []string{"B"}},
	} {
		tc.opt.LibraryID = lib
		if got := listPaths(t, c, tc.opt); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	page, err := c.ListAdminBooks(t.Context(), AdminListOptions{Filter: BookFilter{LibraryID: lib, Tag: "OWNED", Genre: "horror"}})
	if err != nil || len(page.Books) != 0 {
		t.Errorf("admin owned+horror = %v, %v; want none", page, err)
	}
	page, _ = c.ListAdminBooks(t.Context(), AdminListOptions{Filter: BookFilter{LibraryID: lib, Tag: "favourite"}})
	if len(page.Books) != 1 || page.Books[0].Path != "A" || !slices.Equal(page.Books[0].Tags, []string{"Owned", "Favourite"}) {
		t.Errorf("admin favourite = %+v", page.Books)
	}

	found, err := c.Search(t.Context(), "favour", []Scope{{LibraryID: lib, AllowAll: true}}, 10)
	if err != nil || len(found) != 1 || found[0].RelPath != "A" {
		t.Errorf("search by tag = %v, %v; want A", found, err)
	}
	found, _ = c.Search(t.Context(), "horror", []Scope{{LibraryID: lib, AllowAll: true}}, 10)
	if len(found) != 1 || found[0].RelPath != "C" {
		t.Errorf("search by genre = %v; want C", found)
	}
}

// TestLabelFacets: each label counted by key, named by the spelling most of its
// books use, without its own filter.
func TestLabelFacets(t *testing.T) {
	t.Parallel()
	c, lib := labelBooks(t)
	f, err := c.BookFacets(t.Context(), BookFilter{LibraryID: lib, Genre: "horror"})
	if err != nil {
		t.Fatal(err)
	}
	wantGenres := []FacetCount{{"Fantasy", 2}, {"Horror", 1}, {"Science Fiction", 1}}
	if !reflect.DeepEqual(f.Genres, wantGenres) {
		t.Errorf("genres = %+v, want %+v", f.Genres, wantGenres)
	}
	if len(f.Tags) != 0 {
		t.Errorf("tags of the horror book = %+v, want none", f.Tags)
	}
	f, _ = c.BookFacets(t.Context(), BookFilter{LibraryID: lib})
	if want := []FacetCount{{"Owned", 2}, {"Favourite", 1}}; !reflect.DeepEqual(f.Tags, want) {
		t.Errorf("tags = %+v, want %+v", f.Tags, want)
	}

	// The genre filter keeps to the library, but the library facet counts every
	// library's books with that genre.
	other, _ := c.CreateLibrary(t.Context(), Library{Name: "M", Root: "/tmp/m"})
	if _, err := c.UpsertBook(t.Context(), &Book{LibraryID: other.ID, RelPath: "D", Title: "Book D", Genres: []string{"FANTASY"}}); err != nil {
		t.Fatal(err)
	}
	f, _ = c.BookFacets(t.Context(), BookFilter{LibraryID: lib, Genre: "fantasy"})
	if want := []LibraryCount{{lib, 2}, {other.ID, 1}}; !reflect.DeepEqual(f.Libraries, want) {
		t.Errorf("libraries = %+v, want %+v", f.Libraries, want)
	}
	if want := []FacetCount{{"Fantasy", 2}, {"Horror", 1}, {"Science Fiction", 1}}; !reflect.DeepEqual(f.Genres, want) {
		t.Errorf("genres in the library = %+v, want %+v", f.Genres, want)
	}
	if got := listPaths(t, c, ListOptions{LibraryID: lib, Genre: "fantasy"}); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("genre filter = %v, want the library's books only", got)
	}
}

// TestBulkLabelEdit: adding and removing labels keeps each book's own others,
// writes nothing on a book it leaves as it was (nor rebuilds its row), and refuses to be mixed with a
// set or a revert of the same field.
func TestBulkLabelEdit(t *testing.T) {
	t.Parallel()
	c, lib := labelBooks(t)
	ctx := t.Context()
	refs := []Ref{{LibraryID: lib, Path: "A"}, {LibraryID: lib, Path: "B"}, {LibraryID: lib, Path: "C"}}
	if err := c.EditBooks(ctx, refs, BookEdit{
		Add:    map[string][]string{FieldTags: {"Read", " read "}},
		Remove: map[string][]string{FieldTags: {"OWNED"}, FieldGenres: {"horror"}},
	}); err != nil {
		t.Fatal(err)
	}
	want := map[string][2][]string{ // tags, genres
		"A": {{"Favourite", "Read"}, {"Fantasy"}},
		"B": {{"Read"}, {"Science Fiction", "fantasy"}},
		"C": {{"Read"}, nil},
	}
	for path, w := range want {
		b, _ := c.GetBookByPath(ctx, lib, path)
		if !slices.Equal(b.Tags, w[0]) || !slices.Equal(b.Genres, w[1]) {
			t.Errorf("%s: tags %q genres %q, want %q %q", path, b.Tags, b.Genres, w[0], w[1])
		}
	}
	// C's genre removal is an edit (the scan's Horror stays off through a rescan);
	// A's genres were untouched, so they stay unlocked.
	d, _ := c.AdminBookDetail(ctx, lib, "A")
	if d.Fields[FieldGenres].Locked {
		t.Error("A's genres were locked by a removal that changed nothing on it")
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib, RelPath: "C", Title: "Book C", Genres: []string{"Horror"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib, "C"); b.Genres != nil {
		t.Errorf("C after a rescan: genres %q, want the removal kept", b.Genres)
	}

	// Adding a tag every book already has leaves each as it was: no override
	// written, and the row not even rebuilt (a stale title stays stale).
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET title = 'stale' WHERE rel_path = 'A'`); err != nil {
		t.Fatal(err)
	}
	stamps := func() string {
		t.Helper()
		var out string
		if err := c.db.QueryRowContext(ctx, `SELECT group_concat(path || field || updated_at) FROM book_overrides`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := stamps()
	if err := c.EditBooks(ctx, refs, BookEdit{Add: map[string][]string{FieldTags: {"READ"}}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib, "A"); b.Title != "stale" {
		t.Errorf("A's title = %q: the row was rebuilt by an edit that changed nothing on it", b.Title)
	}
	if after := stamps(); after != before {
		t.Errorf("overrides changed by an edit that changed nothing: %s -> %s", before, after)
	}

	for name, edit := range map[string]BookEdit{
		"set and add":       {Set: map[string]string{FieldTags: `["x"]`}, Add: map[string][]string{FieldTags: {"y"}}},
		"revert and remove": {Revert: []string{FieldGenres}, Remove: map[string][]string{FieldGenres: {"y"}}},
		"add and remove":    {Add: map[string][]string{FieldTags: {"Same"}}, Remove: map[string][]string{FieldTags: {"same"}}},
		"not a label":       {Add: map[string][]string{FieldTitle: {"y"}}},
		"too long":          {Add: map[string][]string{FieldTags: {strings.Repeat("x", maxLabelLen+1)}}},
	} {
		if err := c.EditBooks(ctx, refs, edit); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s: err = %v, want an invalid override", name, err)
		}
	}
}

// TestSetGenres: the backfill of a book indexed before genres were read puts
// them in its snapshot (source tag, an edit still over them) and marks it
// checked, so Signatures stops asking.
func TestSetGenres(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	for _, p := range []string{"Old", "None"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, Title: p}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET genres_checked = 0`); err != nil {
		t.Fatal(err)
	}
	sigs, _ := c.Signatures(ctx, lib.ID)
	if !sigs["Old"].GenresUnchecked {
		t.Fatal("a book before 0049 should be unchecked")
	}
	if err := c.SetGenres(ctx, lib.ID, map[string][]string{"Old": {"Fantasy", "fantasy"}, "None": nil}); err != nil {
		t.Fatal(err)
	}
	d, _ := c.AdminBookDetail(ctx, lib.ID, "Old")
	if got := d.Fields[FieldGenres]; got.Value != `["Fantasy"]` || got.Source != SourceTag {
		t.Errorf("backfilled genres = %+v", got)
	}
	if got := d.Fields[FieldTitle]; got.Value != "Old" {
		t.Errorf("title after the backfill = %+v", got)
	}
	sigs, _ = c.Signatures(ctx, lib.ID)
	if sigs["Old"].GenresUnchecked || sigs["None"].GenresUnchecked {
		t.Error("checked books should no longer be unchecked")
	}
	if got := listPaths(t, c, ListOptions{LibraryID: lib.ID, Genre: "fantasy"}); !slices.Equal(got, []string{"Old"}) {
		t.Errorf("genre filter after the backfill = %v", got)
	}
}
