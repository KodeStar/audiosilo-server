package catalog

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// seedMonsters indexes He Who Fights with Monsters as the report found it: five
// books matched (the community's spelling, "with", and their positions), the
// other eight tagged "He Who Fights With Monsters" or, with a typo, "He Who Fights
// WIth Monsters", with no position but their number in the title; plus two
// series that must stay apart from each other.
func seedMonsters(t *testing.T) (*Catalog, context.Context, int64) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/m"})
	if err != nil {
		t.Fatal(err)
	}
	add := func(path, title, series string, idx float64) {
		t.Helper()
		b := &Book{LibraryID: lib.ID, RelPath: path, Title: title, Series: series, SeriesIndex: idx,
			Author: "Shirtaloon", Duration: 3600, Format: "m4b", AddedAt: "2024-01-01T00:00:00Z"}
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []int{2, 3, 4, 5, 7, 8, 10, 13} {
		series, sep := "He Who Fights With Monsters", ":"
		if n == 3 || n == 7 {
			series = "He Who Fights WIth Monsters"
		}
		if n == 10 {
			sep = " -"
		}
		add(monsterPath(n), fmt.Sprintf("He Who Fights with Monsters %d%s A LitRPG Adventure (He Who Fights with Monsters, Book %d)", n, sep, n), series, 0)
	}
	for _, n := range []int{1, 6, 9, 11, 12} {
		path := monsterPath(n)
		add(path, "He Who Fights with Monsters", "He Who Fights With Monsters", 0)
		if err := c.EditBook(ctx, lib.ID, path, BookEdit{Source: SourceCommunity, Set: map[string]string{
			FieldSeries: "He Who Fights with Monsters", FieldSeriesIndex: fmt.Sprint(n), FieldASIN: fmt.Sprintf("B0%08d", n),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	add("Expanse/1", "Leviathan Wakes", "The Expanse", 1)
	add("Expanse/2", "Caliban's War", "Expanse", 2)
	add("Disc/8", "Guards! Guards!", "Discworld", 8)
	add("Disc/watch", "Men at Arms", "Discworld: City Watch", 2)
	return c, ctx, lib.ID
}

// monsterPath is book n's folder, named as the report's library names it (no
// position of its own: the scan's path reading gives none).
func monsterPath(n int) string {
	return fmt.Sprintf("Shirtaloon/FWM%02d - He Who Fights With Monsters - Book %d", n, n)
}

var allMonsters = func() []float64 {
	var out []float64
	for n := 1.0; n <= 13; n++ {
		out = append(out, n)
	}
	return out
}()

// TestSeriesSpellingsAggregate: three spellings of one series are one series,
// named as its matched books spell it, with every book's position (the untagged
// ones' from their titles), count and length; series alike only give or take an
// article, or by one extending the other, stay apart.
func TestSeriesSpellingsAggregate(t *testing.T) {
	t.Parallel()
	c, ctx, lib := seedMonsters(t)
	for _, memberships := range []bool{false, true} {
		got, err := c.Series(ctx, lib, nil, memberships)
		if err != nil {
			t.Fatal(err)
		}
		want := []SeriesCount{
			{Name: "Discworld", Author: "Shirtaloon", Books: 1, Duration: 3600, Positions: []float64{8}},
			{Name: "Discworld: City Watch", Author: "Shirtaloon", Books: 1, Duration: 3600, Positions: []float64{2}},
			{Name: "Expanse", Author: "Shirtaloon", Books: 1, Duration: 3600, Positions: []float64{2}},
			{Name: "He Who Fights with Monsters", Author: "Shirtaloon", Books: 13, Duration: 13 * 3600, Positions: allMonsters},
			{Name: "The Expanse", Author: "Shirtaloon", Books: 1, Duration: 3600, Positions: []float64{1}},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("memberships=%v: series =\n%+v\nwant\n%+v", memberships, got, want)
		}
	}
}

// TestSeriesName: matched books' spelling first, then the most books', then the
// first alphabetically.
func TestSeriesName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   map[string]spelling
		want string
	}{
		{map[string]spelling{"A b": {books: 9}, "a B": {books: 1, matched: 1}}, "a B"},
		{map[string]spelling{"A b": {books: 2}, "a B": {books: 3}}, "a B"},
		{map[string]spelling{"a B": {books: 2}, "A b": {books: 2}}, "A b"},
		{map[string]spelling{"Solo": {books: 1}}, "Solo"},
	} {
		if got := seriesName(tc.in); got != tc.want {
			t.Errorf("seriesName(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSeriesSpellingsFilter: whichever spelling a series is asked for by, the
// admin list (in series order), the player's series filter (with and without
// memberships) and the batch of series pages answer with all thirteen books;
// a series alike only give or take an article answers with its own.
func TestSeriesSpellingsFilter(t *testing.T) {
	t.Parallel()
	c, ctx, lib := seedMonsters(t)
	wantPaths := func() []string {
		var out []string
		for _, n := range allMonsters {
			out = append(out, monsterPath(int(n)))
		}
		return out
	}()
	scope := &Scope{LibraryID: lib, AllowAll: true}
	names := []string{"He Who Fights with Monsters", "He Who Fights With Monsters", "He Who Fights WIth Monsters", "he who fights with monsters!"}
	for _, name := range names {
		// The admin list, in the series' order, across pages.
		var got []string
		var positions []float64
		cursor := ""
		for {
			page, err := c.ListAdminBooks(ctx, AdminListOptions{Filter: BookFilter{Series: name}, Sort: "series", Limit: 5, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range page.Books {
				got = append(got, b.Path)
				positions = append(positions, b.SeriesIndex)
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		if !slices.Equal(got, wantPaths) || !slices.Equal(positions, allMonsters) {
			t.Errorf("admin list for %q = %q at %v", name, got, positions)
		}
		facets, err := c.BookFacets(ctx, BookFilter{Series: name})
		if err != nil || facets.Total != 13 {
			t.Errorf("facets for %q: total %d, %v", name, facets.Total, err)
		}
		for _, memberships := range []bool{false, true} {
			page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib, Series: name, Memberships: memberships, Scope: scope, Sort: "title"})
			if err != nil {
				t.Fatal(err)
			}
			if paths := relPaths(page.Books); len(paths) != 13 {
				t.Errorf("player list for %q (memberships %v) = %q", name, memberships, paths)
			}
		}
	}
	asked := append(names, "The Expanse", "Nothing")
	pages, err := c.ListSeriesBooks(ctx, lib, asked, 50, scope)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pages {
		want := 13
		switch p.Name {
		case "The Expanse":
			want = 1
		case "Nothing":
			want = 0
		}
		if p.Name != asked[i] || len(p.Books) != want {
			t.Errorf("series page %d = %q with %d books, want %d", i, p.Name, len(p.Books), want)
		}
	}
}

// TestSeriesSpellingsNext: the book after #4 of a series is #5 whichever way each
// is spelled, and #5 by its title alone.
func TestSeriesSpellingsNext(t *testing.T) {
	t.Parallel()
	c, ctx, lib := seedMonsters(t)
	four, err := c.GetBookByPath(ctx, lib, monsterPath(4))
	if err != nil {
		t.Fatal(err)
	}
	next, numbered, err := c.NextInSeries(ctx, lib, four, Scope{LibraryID: lib, AllowAll: true})
	if err != nil || next == nil || next.RelPath != monsterPath(5) || !numbered {
		t.Fatalf("next after #4 = %+v, %v, %v", next, numbered, err)
	}
	two, _ := c.GetBookByPath(ctx, lib, monsterPath(2))
	if next, _, _ := c.NextInSeries(ctx, lib, two, Scope{LibraryID: lib, AllowAll: true}); next == nil || next.RelPath != monsterPath(3) {
		t.Fatalf("next after #2 (With) = %+v, want #3 (WIth)", next)
	}
}

// TestPlaceByTitles: a book's title numbers it in its series when nothing else
// does, read as the tag's; a tag's own position wins; and books indexed before
// the rule get it from the startup pass, which leaves the rest alone.
func TestPlaceByTitles(t *testing.T) {
	t.Parallel()
	c, ctx, lib := seedMonsters(t)
	d, err := c.AdminBookDetail(ctx, lib, monsterPath(4))
	if err != nil {
		t.Fatal(err)
	}
	if f := d.Fields[FieldSeriesIndex]; f.Value != "4" || f.Source != SourceTag || f.Locked {
		t.Fatalf("series_index = %+v, want 4 from the tag (its title)", f)
	}
	// A position the tags give wins over the title's.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib, RelPath: "Tagged", Title: "Saga 3: Rise", Series: "Saga", SeriesIndex: 7, AddedAt: "x"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib, "Tagged"); b.SeriesIndex != 7 {
		t.Fatalf("tagged position = %v, want 7", b.SeriesIndex)
	}
	// Rows from before the rule: the pass places them, once.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET series_index = 0 WHERE rel_path IN (?, ?)`, monsterPath(4), monsterPath(10)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{2, 0} {
		n, err := c.PlaceByTitles(ctx)
		if err != nil || n != want {
			t.Fatalf("PlaceByTitles = %d, %v; want %d", n, err, want)
		}
	}
	for path, want := range map[string]float64{monsterPath(4): 4, monsterPath(10): 10, "Disc/8": 8} {
		if b, _ := c.GetBookByPath(ctx, lib, path); b.SeriesIndex != want {
			t.Errorf("%s at %v, want %v", path, b.SeriesIndex, want)
		}
	}
}
