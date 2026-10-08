package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/metadata"
)

// seedAdminLibrary indexes a small varied library: two libraries, mixed codecs,
// covers, chapters, matches and one edited book.
func seedAdminLibrary(t *testing.T) (*Catalog, context.Context, int64, int64) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	a, _ := c.CreateLibrary(ctx, Library{Name: "Fiction", Root: "/tmp/a"})
	b, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/b"})
	yes := true
	books := []*Book{
		{LibraryID: a.ID, RelPath: "Sanderson/Mistborn/1", Title: "The Final Empire", Author: "Brandon Sanderson",
			Series: "Mistborn", SeriesIndex: 1, Format: "m4b", Codec: "aac", Duration: 3000, AddedAt: "2024-01-01T00:00:00Z",
			HasCover: &yes, ASIN: "B000000001"},
		{LibraryID: a.ID, RelPath: "Sanderson/Mistborn/2", Title: "The Well of Ascension", Author: "Sanderson, Brandon",
			Series: "Mistborn", SeriesIndex: 2, Format: "m4b", Codec: "aac", Duration: 4000, AddedAt: "2024-02-01T00:00:00Z"},
		{LibraryID: a.ID, RelPath: "Sanderson/Mistborn/4", Title: "The Alloy of Law", Author: "Brandon Sanderson",
			Series: "Mistborn", SeriesIndex: 4, Format: "mp3", Codec: "mp3", Duration: 2000, AddedAt: "2024-03-01T00:00:00Z"},
		{LibraryID: a.ID, RelPath: "Herbert/Dune", Title: "Dune", Author: "Frank Herbert", Format: "flac", Codec: "ac3",
			Duration: 9000, AddedAt: "2023-06-01T00:00:00Z", CoverPath: "Herbert/Dune/cover.jpg"},
		{LibraryID: a.ID, RelPath: "loose.m4b", Title: "Untitled", Format: "m4b", Codec: "", Duration: 100, AddedAt: "2025-01-01T00:00:00Z"},
		{LibraryID: b.ID, RelPath: "Donaldson/Gruffalo", Title: "The Gruffalo", Author: "Julia Donaldson",
			Narrator: "Imelda Staunton", Format: "m4b", Codec: "aac", Duration: 600, AddedAt: "2022-01-01T00:00:00Z"},
	}
	for _, bk := range books {
		if bk.RelPath == "Sanderson/Mistborn/1" {
			bk.Chapters = []metadata.Chapter{{Index: 0, Title: "1"}, {Index: 1, Title: "2"}}
		}
		if _, err := c.UpsertBook(ctx, bk); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, a.ID, "Herbert/Dune", BookEdit{Set: map[string]string{FieldNarrator: "Scott Brick"}}); err != nil {
		t.Fatal(err)
	}
	return c, ctx, a.ID, b.ID
}

func paths(books []AdminBook) []string {
	out := []string{}
	for _, b := range books {
		out = append(out, b.Path)
	}
	return out
}

func listAll(t *testing.T, c *Catalog, ctx context.Context, opt AdminListOptions) []string {
	t.Helper()
	var out []string
	for range 20 {
		page, err := c.ListAdminBooks(ctx, opt)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, paths(page.Books)...)
		if page.NextCursor == "" {
			return out
		}
		opt.Cursor = page.NextCursor
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func TestListAdminBooksFilters(t *testing.T) {
	c, ctx, libA, _ := seedAdminLibrary(t)
	yes, no := true, false
	for name, tc := range map[string]struct {
		f    BookFilter
		want []string
	}{
		"library":         {BookFilter{LibraryID: libA, Formats: []string{"flac", "mp3"}}, []string{"Herbert/Dune", "Sanderson/Mistborn/4"}},
		"search":          {BookFilter{Query: "ascension"}, []string{"Sanderson/Mistborn/2"}},
		"search edited":   {BookFilter{Query: "brick"}, []string{"Herbert/Dune"}},
		"author exact":    {BookFilter{Author: "Brandon Sanderson"}, []string{"Sanderson/Mistborn/4", "Sanderson/Mistborn/1"}},
		"not playable":    {BookFilter{DirectPlayable: &no}, []string{"Herbert/Dune"}},
		"has cover":       {BookFilter{HasCover: &yes}, []string{"Herbert/Dune", "Sanderson/Mistborn/1"}},
		"has chapters":    {BookFilter{HasChapters: &yes}, []string{"Sanderson/Mistborn/1"}},
		"matched":         {BookFilter{Matched: &yes}, []string{"Sanderson/Mistborn/1"}},
		"edited":          {BookFilter{Edited: &yes}, []string{"Herbert/Dune"}},
		"duration range":  {BookFilter{MinDuration: 2500, MaxDuration: 4000}, []string{"Sanderson/Mistborn/1", "Sanderson/Mistborn/2"}},
		"added range":     {BookFilter{AddedAfter: "2024-02-01", AddedBefore: "2024-03-01"}, []string{"Sanderson/Mistborn/2"}},
		"punctuation q":   {BookFilter{Query: "!!", LibraryID: libA, Codecs: []string{"ac3"}}, []string{"Herbert/Dune"}},
		"no cover in lib": {BookFilter{LibraryID: libA, HasCover: &no, Codecs: []string{"aac"}}, []string{"Sanderson/Mistborn/2"}},
	} {
		page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: tc.f})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := paths(page.Books); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestListAdminBooksRowShape(t *testing.T) {
	c, ctx, libA, _ := seedAdminLibrary(t)
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{LibraryID: libA, Query: "dune"}})
	if err != nil || len(page.Books) != 1 {
		t.Fatalf("%v %v", page, err)
	}
	got := page.Books[0]
	got.id = 0
	want := AdminBook{LibraryID: libA, LibraryName: "Fiction", Path: "Herbert/Dune", Title: "Dune", Author: "Frank Herbert",
		Narrator: "Scott Brick", Duration: 9000, Format: "flac", Codec: "ac3", AddedAt: "2023-06-01T00:00:00Z",
		HasCover: true, FileCount: 1, Edited: true, EditedFields: fieldList{FieldNarrator}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row = %+v\nwant  %+v", got, want)
	}
}

// A row without overrides says so with an empty list, never null (the console
// iterates it).
func TestListAdminBooksEditedFieldsNeverNull(t *testing.T) {
	c, ctx, libA, _ := seedAdminLibrary(t)
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{LibraryID: libA, Edited: new(bool)}})
	if err != nil || len(page.Books) == 0 {
		t.Fatalf("%v %v", page, err)
	}
	raw, _ := json.Marshal(page.Books[0])
	if !strings.Contains(string(raw), `"edited_fields":[]`) {
		t.Fatalf("unedited row = %s", raw)
	}
}

// TestListAdminBooksKeyset: every ordering pages through the whole set exactly
// once at a page size of 1, in the same order as one big page.
func TestListAdminBooksKeyset(t *testing.T) {
	c, ctx, _, _ := seedAdminLibrary(t)
	for sortName := range adminSorts {
		for _, desc := range []bool{false, true} {
			opt := AdminListOptions{Sort: sortName, Desc: desc}
			opt.Limit = 200
			whole, err := c.ListAdminBooks(ctx, opt)
			if err != nil {
				t.Fatal(err)
			}
			opt.Limit = 1
			paged := listAll(t, c, ctx, opt)
			if want := paths(whole.Books); !reflect.DeepEqual(paged, want) || len(want) != 6 {
				t.Errorf("%s desc=%v: paged %v, whole %v", sortName, desc, paged, want)
			}
		}
	}
	// Series order: by series then position, books with no series last - in either
	// direction (a descending sort reverses the series, not where the blanks go).
	got := listAll(t, c, ctx, AdminListOptions{Sort: "series", Limit: 2})
	want := []string{"Sanderson/Mistborn/1", "Sanderson/Mistborn/2", "Sanderson/Mistborn/4"}
	if !reflect.DeepEqual(got[:3], want) {
		t.Fatalf("series order = %v", got)
	}
	got = listAll(t, c, ctx, AdminListOptions{Sort: "series", Desc: true, Limit: 2})
	want = []string{"Sanderson/Mistborn/4", "Sanderson/Mistborn/2", "Sanderson/Mistborn/1"}
	if !reflect.DeepEqual(got[:3], want) {
		t.Fatalf("descending series order = %v (books with no series must stay last)", got)
	}
	got = listAll(t, c, ctx, AdminListOptions{Sort: "narrator", Desc: true, Limit: 2})
	// Z to A: Scott Brick (Dune's edit), then Imelda Staunton, then the 4 unnarrated.
	if !reflect.DeepEqual(got[:2], []string{"Herbert/Dune", "Donaldson/Gruffalo"}) {
		t.Fatalf("descending narrator order = %v (the narrated books come first)", got)
	}
}

// TestListAdminBooksSurnameAndPublished: "surname" files authors by surname
// (either spelling, Le Guin under L) and "published" runs oldest first by the
// work's date, else the tags' release date, a bare year before that year's dates
// and undated books last; both page exactly.
func TestListAdminBooksSurnameAndPublished(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	for _, b := range []*Book{
		{RelPath: "a", Title: "Alloy", Author: "Brandon Sanderson", Series: "Mistborn", SeriesIndex: 4, Published: "2011-11-08"},
		{RelPath: "b", Title: "Empire", Author: "Sanderson, Brandon", Series: "Mistborn", SeriesIndex: 1, Published: "2006-07-17"},
		{RelPath: "c", Title: "Earthsea", Author: "Ursula K. Le Guin", Published: "1968"},
		// No published date: the tags' release date stands in.
		{RelPath: "d", Title: "Ancillary", Author: "Ann Leckie", Released: "2013"},
		// The work's date wins over the recording's.
		{RelPath: "e", Title: "Dune", Author: "Frank Herbert", Published: "1965-08-01", Released: "2007"},
		{RelPath: "f", Title: "Dune Messiah", Author: "Frank Herbert", Published: "1965"},
		{RelPath: "g", Title: "Anonymous", Author: ""},
		{RelPath: "h", Title: "Undated", Author: "Zed Smith"},
	} {
		b.LibraryID, b.Format = lib.ID, "m4b"
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		sort string
		desc bool
		want []string
	}{
		// One surname key: its spellings then sort as written (merge them on Authors).
		{"surname", false, []string{"e", "f", "d", "c", "a", "b", "h", "g"}},
		{"surname", true, []string{"h", "b", "a", "c", "d", "f", "e", "g"}},
		{"published", false, []string{"f", "e", "c", "b", "a", "d", "g", "h"}},
		// Newest first, undated books still last.
		{"published", true, []string{"d", "a", "b", "c", "e", "f", "h", "g"}},
	} {
		got := listAll(t, c, ctx, AdminListOptions{Sort: tc.sort, Desc: tc.desc, Limit: 3})
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s desc=%v = %v, want %v", tc.sort, tc.desc, got, tc.want)
		}
	}
}

func TestListAdminBooksBadCursor(t *testing.T) {
	c, ctx, _, _ := seedAdminLibrary(t)
	page, _ := c.ListAdminBooks(ctx, AdminListOptions{Sort: "title", Limit: 1})
	for name, opt := range map[string]AdminListOptions{
		"other sort":  {Sort: "author", Cursor: page.NextCursor},
		"other order": {Sort: "title", Desc: true, Cursor: page.NextCursor},
		"garbage":     {Sort: "title", Cursor: "!!!"},
		"not json":    {Sort: "title", Cursor: "bm90IGpzb24"},
	} {
		if _, err := c.ListAdminBooks(ctx, opt); !errors.Is(err, ErrInvalidCursor) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor", name, err)
		}
	}
	if _, err := c.ListAdminBooks(ctx, AdminListOptions{Sort: "rel_path"}); !errors.Is(err, ErrUnknownSort) {
		t.Errorf("unknown sort: err = %v, want ErrUnknownSort", err)
	}
}

func TestBookFacets(t *testing.T) {
	c, ctx, libA, libB := seedAdminLibrary(t)
	f, err := c.BookFacets(ctx, BookFilter{LibraryID: libA, Formats: []string{"m4b"}})
	if err != nil {
		t.Fatal(err)
	}
	if f.Total != 3 {
		t.Fatalf("total = %d, want 3 (m4b in Fiction)", f.Total)
	}
	// The library facet ignores the library filter (but keeps the format filter).
	if want := []LibraryCount{{libA, 3}, {libB, 1}}; !reflect.DeepEqual(f.Libraries, want) {
		t.Errorf("libraries = %v, want %v", f.Libraries, want)
	}
	// The format facet ignores the format filter (but keeps the library filter).
	if want := []FacetCount{{"m4b", 3}, {"flac", 1}, {"mp3", 1}}; !reflect.DeepEqual(f.Formats, want) {
		t.Errorf("formats = %v, want %v", f.Formats, want)
	}
	if want := []FacetCount{{"aac", 2}, {"", 1}}; !reflect.DeepEqual(f.Codecs, want) {
		t.Errorf("codecs = %v, want %v", f.Codecs, want)
	}
	if f.HasCover != (BoolFacet{Yes: 1, No: 2}) || f.Matched != (BoolFacet{Yes: 1, No: 2}) ||
		f.HasChapters != (BoolFacet{Yes: 1, No: 2}) || f.DirectPlayable != (BoolFacet{Yes: 3}) {
		t.Errorf("bool facets = %+v", f)
	}
	// A bool facet ignores its own filter.
	no := false
	f, _ = c.BookFacets(ctx, BookFilter{Edited: &no})
	if f.Total != 5 || f.Edited != (BoolFacet{Yes: 1, No: 5}) {
		t.Errorf("edited facet under edited=false: total=%d %+v", f.Total, f.Edited)
	}
}

func TestPeopleAggregate(t *testing.T) {
	c, ctx, libA, _ := seedAdminLibrary(t)
	agg, err := c.People(ctx, PeopleAuthors, libA, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []PersonCount{{"Brandon Sanderson", 2, 5000}, {"Frank Herbert", 1, 9000}, {"Sanderson, Brandon", 1, 4000}}
	if !reflect.DeepEqual(agg.People, want) || agg.Unknown != 1 {
		t.Fatalf("authors = %+v unknown=%d", agg.People, agg.Unknown)
	}
	wantS := []MergeSuggestion{{Names: []string{"Brandon Sanderson", "Sanderson, Brandon"}, Suggested: "Brandon Sanderson", Books: 3}}
	if !reflect.DeepEqual(agg.Suggestions, wantS) {
		t.Fatalf("suggestions = %+v", agg.Suggestions)
	}
	all, _ := c.People(ctx, PeopleNarrators, 0, nil)
	if len(all.People) != 2 || all.Unknown != 4 {
		t.Fatalf("narrators = %+v unknown=%d", all.People, all.Unknown)
	}
	if _, err := c.People(ctx, "rel_path", 0, nil); err == nil {
		t.Fatal("an unknown people field must be refused")
	}
}

func TestPersonKey(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"Brandon Sanderson", "Sanderson, Brandon"},
		{"J.R.R. Tolkien", "J. R. R. Tolkien"},
		{"Ursula K. Le Guin", "ursula k le guin"},
	} {
		if personKey(tc.a) != personKey(tc.b) {
			t.Errorf("%q and %q should share a key (%q vs %q)", tc.a, tc.b, personKey(tc.a), personKey(tc.b))
		}
	}
	if personKey("Alexandre Dumas, pere") == personKey("pere Alexandre Dumas") {
		t.Error("a multi-word name before the comma must not be turned round")
	}
	// Every script's letters count: spellings of one non-Latin name group, but two
	// different ones never collide on an ASCII residue ("jr") or on nothing.
	for _, tc := range []struct{ a, b string }{{"村上 春樹", "村上春樹"}, {"Лев Толстой", "Толстой, Лев"}} {
		if personKey(tc.a) == "" || personKey(tc.a) != personKey(tc.b) {
			t.Errorf("%q and %q should share a key (%q vs %q)", tc.a, tc.b, personKey(tc.a), personKey(tc.b))
		}
	}
	for _, tc := range []struct{ a, b string }{{"Иван Петров Jr.", "Сергей Иванов Jr."}, {"村上春樹", "東野圭吾"}, {"Michael Ball", "Michael Ballé"}} {
		if personKey(tc.a) == personKey(tc.b) {
			t.Errorf("%q and %q are different people but share the key %q", tc.a, tc.b, personKey(tc.a))
		}
	}
	if personKey("...") != "" {
		t.Error("a name with no letter or digit has no key (never grouped)")
	}
}

func TestSeriesAggregate(t *testing.T) {
	c, ctx, _, _ := seedAdminLibrary(t)
	series, err := c.Series(ctx, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []SeriesCount{{Name: "Mistborn", Author: "Brandon Sanderson", Books: 3, Duration: 9000, Positions: []float64{1, 2, 4}}}
	if !reflect.DeepEqual(series, want) {
		t.Fatalf("series = %+v", series)
	}
}

// TestAggregatesScoped: with a scope, People and Series count only the books the
// scope grants (denied: one outside it contributes nothing, nor does another
// library's, even with libraryID 0), and nil counts every book (allowed).
func TestAggregatesScoped(t *testing.T) {
	c, ctx, libA, libB := seedAdminLibrary(t)
	mistborn := &Scope{LibraryID: libA, Paths: []string{"Sanderson/Mistborn"}}
	agg, err := c.People(ctx, PeopleAuthors, libA, mistborn)
	if err != nil {
		t.Fatal(err)
	}
	want := []PersonCount{{"Brandon Sanderson", 2, 5000}, {"Sanderson, Brandon", 1, 4000}}
	if !reflect.DeepEqual(agg.People, want) || agg.Unknown != 0 {
		t.Fatalf("scoped authors = %+v unknown=%d", agg.People, agg.Unknown)
	}
	// A whole-library scope still keeps to its own library.
	agg, _ = c.People(ctx, PeopleNarrators, 0, &Scope{LibraryID: libA, AllowAll: true})
	if want := []PersonCount{{"Scott Brick", 1, 9000}}; !reflect.DeepEqual(agg.People, want) || agg.Unknown != 4 {
		t.Fatalf("library-A narrators = %+v unknown=%d", agg.People, agg.Unknown)
	}
	agg, _ = c.People(ctx, PeopleNarrators, 0, nil)
	if len(agg.People) != 2 {
		t.Fatalf("unscoped narrators = %+v", agg.People)
	}
	// An empty grant counts nothing; so does another library's scope.
	for _, s := range []*Scope{{LibraryID: libA}, {LibraryID: libB, AllowAll: true}} {
		agg, _ = c.People(ctx, PeopleAuthors, libA, s)
		if len(agg.People) != 0 || agg.Unknown != 0 {
			t.Errorf("scope %+v: authors = %+v unknown=%d, want none", s, agg.People, agg.Unknown)
		}
	}

	series, err := c.Series(ctx, libA, &Scope{LibraryID: libA, Paths: []string{"Sanderson/Mistborn/1", "Sanderson/Mistborn/4"}})
	if err != nil {
		t.Fatal(err)
	}
	wantS := []SeriesCount{{Name: "Mistborn", Author: "Brandon Sanderson", Books: 2, Duration: 5000, Positions: []float64{1, 4}}}
	if !reflect.DeepEqual(series, wantS) {
		t.Fatalf("scoped series = %+v", series)
	}
	series, _ = c.Series(ctx, libA, &Scope{LibraryID: libA, Paths: []string{"Herbert"}})
	if series == nil || len(series) != 0 {
		t.Fatalf("series outside the grant = %#v, want empty", series)
	}
}

// TestDirectPlayableSQLAgreesWithGo: the list's direct_playable (SQL) answers what
// /item tells players (media.DirectPlayable) for every kind of codec.
func TestDirectPlayableSQLAgreesWithGo(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	codecs := []string{"", "aac", "MP3", "flac", "opus", "vorbis", "pcm_s16le", "ac3", "eac3", "wmav2"}
	for _, codec := range codecs {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "b/" + codec, Title: codec, Codec: codec, AddedAt: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := c.ListAdminBooks(ctx, AdminListOptions{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range page.Books {
		if want := media.DirectPlayable(b.Codec); b.DirectPlayable != want {
			t.Errorf("codec %q: SQL says %v, media.DirectPlayable says %v", b.Codec, b.DirectPlayable, want)
		}
	}
}

// On a tie the suggested spelling is the natural "Given Surname" one, never the
// reversed "Surname, Given" (which merely sorts first); more books still wins.
func TestMergeSuggestionPrefersNaturalOrder(t *testing.T) {
	for name, tc := range map[string]struct {
		people []PersonCount
		want   string
	}{
		"tie":        {[]PersonCount{{Name: "Carroll, Lewis", Books: 1}, {Name: "Lewis Carroll", Books: 1}}, "Lewis Carroll"},
		"more books": {[]PersonCount{{Name: "Carroll, Lewis", Books: 3}, {Name: "Lewis Carroll", Books: 1}}, "Carroll, Lewis"},
		"plain tie":  {[]PersonCount{{Name: "J. R. R. Tolkien", Books: 1}, {Name: "J.R.R. Tolkien", Books: 1}}, "J. R. R. Tolkien"},
	} {
		got := mergeSuggestions(tc.people)
		if len(got) != 1 || got[0].Suggested != tc.want {
			t.Errorf("%s: suggestions = %+v, want %q suggested", name, got, tc.want)
		}
	}
}
