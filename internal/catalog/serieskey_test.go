package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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
	// A match that replaced the title (without its number) and gave no position:
	// the scanned title still numbers it.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib, RelPath: "Foo/4", Title: "Foo 4: The Bar", Series: "Foo", AddedAt: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib, "Foo/4", BookEdit{Source: SourceCommunity, Set: map[string]string{FieldTitle: "The Bar", FieldASIN: "B000000099"}}); err != nil {
		t.Fatal(err)
	}
	// Rows from before the rule: the pass places them, once.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET series_index = 0 WHERE rel_path IN (?, ?, ?)`, monsterPath(4), monsterPath(10), "Foo/4"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{3, 0} {
		n, err := c.PlaceByTitles(ctx)
		if err != nil || n != want {
			t.Fatalf("PlaceByTitles = %d, %v; want %d", n, err, want)
		}
	}
	for path, want := range map[string]float64{monsterPath(4): 4, monsterPath(10): 10, "Disc/8": 8, "Foo/4": 4} {
		if b, _ := c.GetBookByPath(ctx, lib, path); b.SeriesIndex != want {
			t.Errorf("%s at %v, want %v", path, b.SeriesIndex, want)
		}
	}
}

// unkey puts the index back as migration 0041 leaves a database indexed before
// it: no book keyed, no list held by key, KeySeries not run.
func unkey(t *testing.T, c *Catalog, ctx context.Context) {
	t.Helper()
	for _, q := range []string{`UPDATE books SET series_key = ''`, `DELETE FROM book_more_series`,
		`DELETE FROM server_state WHERE key = 'series_keys'`} {
		if _, err := c.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

// TestKeySeries: a database indexed before series keys finds no series by its
// key until the startup pass keys every book, once; that pass also puts a list
// naming one series twice, in two spellings, back to one entry, so the next book
// in it is the first spelling's.
func TestKeySeries(t *testing.T) {
	t.Parallel()
	c, ctx, lib := seedMonsters(t)
	if err := c.EditBook(ctx, lib, "Disc/8", BookEdit{Set: map[string]string{FieldMoreSeries: `[{"name":"City Watch","position":1}]`}}); err != nil {
		t.Fatal(err)
	}
	twice := `[{"name":"City Watch","position":1},{"name":"city watch","position":5}]`
	for _, q := range []string{`UPDATE book_overrides SET value = ? WHERE path = 'Disc/8' AND field = 'more_series'`,
		`UPDATE books SET more_series = ? WHERE rel_path = 'Disc/8'`} {
		if _, err := c.db.ExecContext(ctx, q, twice); err != nil {
			t.Fatal(err)
		}
	}
	unkey(t, c, ctx)
	scope := &Scope{LibraryID: lib, AllowAll: true}
	count := func(series string) int {
		t.Helper()
		page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib, Series: series, Memberships: true, Scope: scope, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Books)
	}
	if n := count("He Who Fights with Monsters"); n != 0 {
		t.Fatalf("before keying: %d books found by key, want 0 (the test's unkeyed state)", n)
	}
	for _, want := range []int{17, 0} { // every book, then nothing: it has run
		n, err := c.KeySeries(ctx)
		if err != nil || n != want {
			t.Fatalf("KeySeries = %d, %v; want %d", n, err, want)
		}
	}
	// Keys stored by other rules or Unicode tables (or the first build's bare
	// "done") are keyed again, once.
	for _, stale := range []any{true, "0/old"} {
		if err := putServerState(ctx, c.db, seriesKeysKey, stale, c.stamp()); err != nil {
			t.Fatal(err)
		}
		for _, want := range []int{17, 0} {
			if n, err := c.KeySeries(ctx); err != nil || n != want {
				t.Fatalf("KeySeries over %v = %d, %v; want %d", stale, n, err, want)
			}
		}
	}
	if n := count("He Who Fights WIth Monsters"); n != 13 {
		t.Errorf("after keying: %d books, want 13", n)
	}
	if n := count("CITY WATCH"); n != 1 { // Guards! Guards!, by its list
		t.Errorf("City Watch: %d books, want 1", n)
	}
	b, err := c.GetBookByPath(ctx, lib, "Disc/8")
	if err != nil {
		t.Fatal(err)
	}
	once := `[{"name":"City Watch","position":1}]`
	var stored string
	if err := c.db.QueryRowContext(ctx, `SELECT value FROM book_overrides WHERE path = 'Disc/8' AND field = 'more_series'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != once || EncodeMoreSeries(b.AllSeries()[1:]) != once {
		t.Errorf("list = %s (row %v), want %s", stored, b.AllSeries(), once)
	}
	positions, err := queryRows(ctx, c.db, func(r *sql.Rows, p *float64) error { return r.Scan(p) },
		`SELECT m.position FROM book_more_series m JOIN books b ON b.id = m.book_id WHERE b.rel_path = 'Disc/8'`)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(positions, []float64{1}) {
		t.Errorf("keyed list positions = %v, want [1]", positions)
	}
	two, _ := c.GetBookByPath(ctx, lib, monsterPath(2))
	if next, _, _ := c.NextInSeries(ctx, lib, two, *scope); next == nil || next.RelPath != monsterPath(3) {
		t.Errorf("next after #2 = %+v, want #3", next)
	}
}

// seriesScale is a library of books in series of 25, each spelled three ways, a
// tenth of the books also in a list series of 50 (one per 500 books, so a series
// is the same size whatever the library's).
type seriesScale struct {
	c     *Catalog
	ctx   context.Context
	lib   int64
	books int
	mid   int // a series in the middle, as its three spellings
	scope *Scope
}

func newSeriesScale(t *testing.T, books int) *seriesScale {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Big", Root: "/tmp/big"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?1)
		INSERT INTO books(library_id, rel_path, title, author, series, series_index, more_series, indexed_at, added_at)
		SELECT ?2, 'B/' || i, 'Book ' || i, 'Author ' || (i % 3000),
		       CASE i % 3 WHEN 0 THEN 'Series ' || (i / 25) WHEN 1 THEN 'series ' || (i / 25) ELSE 'SERIES-' || (i / 25) END,
		       i % 25 + 1,
		       CASE WHEN i % 10 = 0 THEN '[{"name":"Shared ' || (i / 10 % (?1 / 500)) || '","position":' || (i / 10 / (?1 / 500) + 1) || '}]' ELSE '[]' END,
		       'x', '2024-01-01T00:00:00Z'
		  FROM n`, books, lib.ID); err != nil {
		t.Fatal(err)
	}
	return &seriesScale{c: c, ctx: ctx, lib: lib.ID, books: books, mid: books / 25 / 2, scope: &Scope{LibraryID: lib.ID, AllowAll: true}}
}

// Limits on how much longer the large library in TestSeriesKeyScale may take than
// the small one, ten times its size: a pass over the books grows about ten times
// and would grow a hundred if it went quadratic; a read of one series stays about
// the same and would grow ten times if it walked the library.
const (
	scaleLimitPass = 30
	scaleLimitRead = 5
)

// scaleRounds is how many times TestSeriesKeyScale asks before it fails. A real
// change in how a read grows is there every round; a busy runner isn't.
const scaleRounds = 7

// TestSeriesKeyScale runs keying and the series reads on 50,000 books, checks
// their answers, and compares their times with a tenth of the books on the same
// machine at the same moment: how a read grows with the library is its own
// property, where a limit in milliseconds was one of the runner (a plain build
// keys them in about half a second here and in about three on CI; the Health
// count, 85 ms here, took 205-464 ms there). A read made slower at every size is
// not seen here: the plan tests hold each read's shape (TestNextInSeriesPlan,
// TestIssueCountsPlan). The race detector, about 40 times slower here, takes a
// tenth of the books, one size, and only logs the times.
func TestSeriesKeyScale(t *testing.T) {
	t.Parallel()
	var sizes []*seriesScale
	if raceEnabled {
		sizes = []*seriesScale{newSeriesScale(t, 5_000)}
	} else {
		sizes = []*seriesScale{newSeriesScale(t, 5_000), newSeriesScale(t, 50_000)}
	}
	// timed runs f on each size in turn (before, untimed, ahead of it), so a busy
	// moment falls on both, keeping each size's best until the large one's is
	// within limit times the small one's.
	timed := func(what string, limit float64, before func(*seriesScale) error, f func(*seriesScale) error) {
		t.Helper()
		best := make([]time.Duration, len(sizes))
		for round := range scaleRounds {
			for i, s := range sizes {
				if before != nil {
					if err := before(s); err != nil {
						t.Fatalf("%s, %d books: %v", what, s.books, err)
					}
				}
				start := time.Now()
				if err := f(s); err != nil {
					t.Fatalf("%s, %d books: %v", what, s.books, err)
				}
				if took := time.Since(start); round == 0 || took < best[i] {
					best[i] = took
				}
			}
			if raceEnabled {
				t.Logf("%s: %v", what, best[0])
				return
			}
			if float64(best[1]) <= limit*float64(best[0]) {
				t.Logf("%s: %v on %d books, %v on %d", what, best[0], sizes[0].books, best[1], sizes[1].books)
				return
			}
		}
		t.Errorf("%s: %v on %d books but %v on %d, want at most %v times as long",
			what, best[0], sizes[0].books, best[1], sizes[1].books, limit)
	}
	timed("KeySeries", scaleLimitPass, func(s *seriesScale) error {
		// Every round keys every book from nothing, as the first one does.
		if _, err := s.c.db.ExecContext(s.ctx, `DELETE FROM server_state WHERE key = ?`, seriesKeysKey); err != nil {
			return err
		}
		_, err := s.c.db.ExecContext(s.ctx, `UPDATE books SET series_key = ''`)
		return err
	}, func(s *seriesScale) error {
		n, err := s.c.KeySeries(s.ctx)
		if err == nil && n != s.books {
			err = fmt.Errorf("keyed %d books, want %d", n, s.books)
		}
		return err
	})
	timed("player list", scaleLimitRead, nil, func(s *seriesScale) error {
		page, err := s.c.ListBooks(s.ctx, ListOptions{LibraryID: s.lib, Series: fmt.Sprint("Series ", s.mid), Memberships: true, Scope: s.scope, Limit: 50})
		if err == nil && len(page.Books) != 25 {
			err = fmt.Errorf("%d books, want 25", len(page.Books))
		}
		return err
	})
	timed("admin list in series order, every library", scaleLimitRead, nil, func(s *seriesScale) error {
		page, err := s.c.ListAdminBooks(s.ctx, AdminListOptions{Filter: BookFilter{Series: fmt.Sprint("series-", s.mid)}, Sort: "series", Limit: 60})
		if err == nil && (len(page.Books) != 25 || page.Books[0].SeriesIndex != 1) {
			err = fmt.Errorf("%d books, first at %v", len(page.Books), page.Books[0].SeriesIndex)
		}
		return err
	})
	timed("a list series", scaleLimitRead, nil, func(s *seriesScale) error {
		page, err := s.c.ListBooks(s.ctx, ListOptions{LibraryID: s.lib, Series: "shared 7", Memberships: true, Scope: s.scope, Limit: 25})
		if err == nil && (len(page.Books) != 25 || page.NextCursor == "") {
			err = fmt.Errorf("%d books, next %q", len(page.Books), page.NextCursor)
		}
		return err
	})
	timed("Health: series spelled several ways", scaleLimitPass, nil, func(s *seriesScale) error {
		ic, err := s.c.IssueCounts(s.ctx, []string{IssueSeriesSpellings})
		if err == nil && ic[0].Count != s.books {
			err = fmt.Errorf("count %d, want %d", ic[0].Count, s.books)
		}
		return err
	})
	timed("next in series", scaleLimitRead, nil, func(s *seriesScale) error {
		book, err := s.c.GetBookByPath(s.ctx, s.lib, fmt.Sprint("B/", s.mid*25+1)) // #2 in the middle series
		if err != nil {
			return err
		}
		next, _, err := s.c.NextInSeries(s.ctx, s.lib, book, *s.scope)
		if err == nil && (next == nil || next.SeriesIndex != 3) {
			err = fmt.Errorf("next = %+v", next)
		}
		return err
	})
}

// TestIssueCountsPlan: the Health counts are one walk of the books with two index
// seeks per book for series_spellings, however long the series, never a walk of
// the series or the library per book; led by the walk, since an IN list for the
// books-kind libraries (inBookLibrary) lets the planner lead with the library
// index instead, which reads every book through it.
func TestIssueCountsPlan(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	plan := queryPlan(t, c, issueCountsSQL([]string{IssueSeriesSpellings}), nil)
	joined := strings.Join(plan, "\n")
	for _, want := range []string{
		"SEARCH s USING COVERING INDEX idx_books_series_key (library_id=? AND series_key=? AND series<?)",
		"SEARCH s USING COVERING INDEX idx_books_series_key (library_id=? AND series_key=? AND series>?)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan does not use %s:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "SCAN b") || strings.Contains(joined, "SEARCH b") {
		t.Errorf("plan is not led by a walk of the books:\n%s", joined)
	}
	if strings.Contains(joined, "SCAN s") {
		t.Errorf("plan walks the books per book:\n%s", joined)
	}
}
