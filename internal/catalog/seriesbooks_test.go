package catalog

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// seedSeries indexes books (lib, path, series, index) and returns the two
// libraries, "Second" ordered before "First".
func seedSeries(t *testing.T) (*Catalog, context.Context, *Library, *Library) {
	t.Helper()
	c, ctx := newTestCatalog(t)
	first, _ := c.CreateLibrary(ctx, Library{Name: "First", Root: "/tmp/a"})
	second, _ := c.CreateLibrary(ctx, Library{Name: "Second", Root: "/tmp/b"})
	if err := c.ReorderLibraries(ctx, []int64{second.ID, first.ID}); err != nil {
		t.Fatal(err)
	}
	for _, b := range []Book{
		{LibraryID: first.ID, RelPath: "Granted/Expanse 2", Series: "the  expanse!", SeriesIndex: 2},
		{LibraryID: first.ID, RelPath: "Granted/Expanse 1", Series: "The Expanse", SeriesIndex: 1},
		{LibraryID: first.ID, RelPath: "Granted/Origins", Series: "Expanse Origins", SeriesIndex: 1},
		{LibraryID: first.ID, RelPath: "Secret/Expanse 3", Series: "The Expanse", SeriesIndex: 3},
		{LibraryID: first.ID, RelPath: "Granted/Loose", Series: "", SeriesIndex: 0},
		{LibraryID: second.ID, RelPath: "Shelf/Expanse 4", Series: "Thé Éxpanse", SeriesIndex: 4},
	} {
		b.Title, b.AddedAt = b.RelPath, "2024-01-01T00:00:00Z"
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	return c, ctx, first, second
}

func relPaths(books []Book) []string {
	out := make([]string, len(books))
	for i, b := range books {
		out[i] = b.RelPath
	}
	return out
}

// TestSeriesBooksScope is the allowed+denied pair: an unrestricted caller gets
// every book of the folded series across libraries, in library sort order then
// path; a caller granted one folder never gets a book outside it.
func TestSeriesBooksScope(t *testing.T) {
	t.Parallel()
	c, ctx, first, second := seedSeries(t)
	names := []string{"The Expanse", "Unrelated"}

	all := []Scope{{LibraryID: first.ID, AllowAll: true}, {LibraryID: second.ID, AllowAll: true}}
	books, err := c.SeriesBooks(ctx, all, names)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Shelf/Expanse 4", "Granted/Expanse 1", "Granted/Expanse 2", "Secret/Expanse 3"}
	if got := relPaths(books); !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed = %q, want %q", got, want)
	}
	if books[0].Series != "Thé Éxpanse" || books[0].SeriesIndex != 4 || books[0].Files != nil {
		t.Fatalf("book = %+v", books[0])
	}

	// Denied: granted only "Granted" in the first library (and nothing in the
	// second), "Secret/Expanse 3" and the second library's copy never appear.
	scoped := []Scope{{LibraryID: first.ID, Paths: []string{"Granted"}}}
	books, err = c.SeriesBooks(ctx, scoped, names)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := relPaths(books), []string{"Granted/Expanse 1", "Granted/Expanse 2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("scoped = %q, want %q", got, want)
	}

	// Nothing to match, or no access, is no books (and no query error).
	for _, tc := range []struct {
		scopes []Scope
		names  []string
	}{{all, nil}, {all, []string{"!!"}}, {nil, names}, {[]Scope{{LibraryID: first.ID}}, names}} {
		if books, err := c.SeriesBooks(ctx, tc.scopes, tc.names); err != nil || len(books) != 0 {
			t.Fatalf("SeriesBooks(%+v, %q) = %q, %v", tc.scopes, tc.names, relPaths(books), err)
		}
	}
}

func TestNextInOneSeries(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/a"})
	for _, b := range []Book{
		{RelPath: "S/1", Series: "S", SeriesIndex: 1},
		{RelPath: "S/1 copy", Series: "S", SeriesIndex: 1},
		{RelPath: "S/3 b", Series: "S", SeriesIndex: 3},
		{RelPath: "S/3 a", Series: "S", SeriesIndex: 3},
		{RelPath: "Hidden/2", Series: "S", SeriesIndex: 2},
		{RelPath: "S/5", Series: "S", SeriesIndex: 5},
		{RelPath: "S/x", Series: "s", SeriesIndex: 4}, // another (exact) series
		{RelPath: "U/a", Series: "U", SeriesIndex: 0},
		{RelPath: "U/b", Series: "U", SeriesIndex: 0},
	} {
		b.LibraryID, b.Title, b.AddedAt = lib.ID, b.RelPath, "2024-01-01T00:00:00Z"
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	granted := Scope{LibraryID: lib.ID, Paths: []string{"S", "U"}}
	for name, tc := range map[string]struct {
		path         string
		series       string
		index        float64
		scope        Scope
		want         string
		wantNumbered bool
	}{
		"smallest later index":      {"S/1", "S", 1, all, "Hidden/2", true},
		"denied: outside the grant": {"S/1", "S", 1, granted, "S/3 a", true},
		"ties by path":              {"Hidden/2", "S", 2, all, "S/3 a", true},
		"end of the series":         {"S/5", "S", 5, all, "", true},
		"nothing else in scope":     {"S/1", "S", 1, Scope{LibraryID: lib.ID, Paths: []string{"S/1"}}, "", false},
		"unnumbered series":         {"U/a", "U", 1, all, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			next, numbered, err := c.nextInOneSeries(ctx, lib.ID, tc.path, tc.series, tc.index, tc.scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if next != nil {
				got = next.RelPath
			}
			if got != tc.want || numbered != tc.wantNumbered {
				t.Fatalf("next = %q, numbered = %v; want %q, %v", got, numbered, tc.want, tc.wantNumbered)
			}
		})
	}
	// A skipped book is passed over for the next later one, in position order
	// (ties by path); skipping every later one is numbered with none later.
	for skipped, want := range map[string]string{"Hidden/2": "S/3 a", "Hidden/2|S/3 a": "S/3 b", "Hidden/2|S/3 a|S/3 b|S/5": ""} {
		next, numbered, err := c.nextInOneSeries(ctx, lib.ID, "S/1", "S", 1, all, func(b *Book) bool {
			return slices.Contains(strings.Split(skipped, "|"), b.RelPath)
		})
		if err != nil || !numbered || (next == nil) != (want == "") || (next != nil && next.RelPath != want) {
			t.Fatalf("skipping %q: next = %+v, numbered = %v, %v; want %q", skipped, next, numbered, err, want)
		}
	}
}

// TestNextInOneSeriesMemberships: a book is in a series through its main series or
// an entry of its more_series, at its position in THAT series, both ways round;
// the end of a membership series is numbered; the scope narrows both branches;
// and a list repeating the main series counts the book once.
func TestNextInOneSeriesMemberships(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Disc", Root: "/tmp/d"})
	for _, b := range []Book{
		{RelPath: "gg", Series: "Discworld", SeriesIndex: 8},            // City Watch 1 (list)
		{RelPath: "maa", Series: "City Watch", SeriesIndex: 2},          // main
		{RelPath: "foc", Series: "Discworld", SeriesIndex: 19},          // City Watch 3 (list)
		{RelPath: "foc b", Series: "Discworld", SeriesIndex: 1},         // City Watch 3 (list): tie with foc
		{RelPath: "nw", Series: "City Watch", SeriesIndex: 6},           // main, also lists City Watch 0.5
		{RelPath: "Hidden/jingo", Series: "Discworld", SeriesIndex: 21}, // City Watch 2.5 (list)
	} {
		b.LibraryID, b.Title, b.AddedAt = lib.ID, b.RelPath, "2024-01-01T00:00:00Z"
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	for path, list := range map[string]string{
		"gg":           `[{"name":"City Watch","position":1}]`,
		"foc":          `[{"name":"City Watch","position":3}]`,
		"foc b":        `[{"name":"Other","position":9},{"name":"City Watch","position":3}]`,
		"nw":           `[{"name":"City Watch","position":0.5}]`,
		"Hidden/jingo": `[{"name":"City Watch","position":2.5}]`,
	} {
		if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldMoreSeries: list}}); err != nil {
			t.Fatal(err)
		}
	}
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	granted := Scope{LibraryID: lib.ID, Paths: []string{"gg", "maa", "foc", "foc b", "nw"}}
	for name, tc := range map[string]struct {
		path         string
		series       string
		index        float64
		scope        Scope
		want         string
		wantNumbered bool
	}{
		// gg is City Watch 1 only through its list; maa is #2 by its main series.
		"list to main": {"gg", "City Watch", 1, all, "maa", true},
		// Positions within City Watch, not series_index: Jingo (Discworld 21) is
		// City Watch 2.5, before Feet of Clay (Discworld 19, City Watch 3).
		"main to list":              {"maa", "City Watch", 2, all, "Hidden/jingo", true},
		"denied: outside the grant": {"maa", "City Watch", 2, granted, "foc", true},
		// foc and foc b are both City Watch 3: ties by path.
		"ties by path": {"Hidden/jingo", "City Watch", 2.5, all, "foc", true},
		// nw is City Watch 6 by its main series; its list's 0.5 is never read.
		"list after a tie": {"foc b", "City Watch", 3, all, "nw", true},
		"end of a series":  {"nw", "City Watch", 6, all, "", true},
		// The main series of the list books, the same query.
		"main series": {"foc b", "Discworld", 1, all, "gg", true},
		// Allowed: a grant on gg and Hidden reaches Jingo through its list. Denied:
		// a grant on gg alone reaches no other member, so City Watch gives gg no
		// order to follow (not numbered).
		"allowed: a list book in the grant": {"gg", "City Watch", 1, Scope{LibraryID: lib.ID, Paths: []string{"gg", "Hidden"}}, "Hidden/jingo", true},
		"denied: alone in scope":            {"gg", "City Watch", 1, Scope{LibraryID: lib.ID, Paths: []string{"gg"}}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			next, numbered, err := c.nextInOneSeries(ctx, lib.ID, tc.path, tc.series, tc.index, tc.scope, nil)
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if next != nil {
				got = next.RelPath
			}
			if got != tc.want || numbered != tc.wantNumbered {
				t.Fatalf("next = %q, numbered = %v; want %q, %v", got, numbered, tc.want, tc.wantNumbered)
			}
			if next != nil && next.RelPath == "gg" && len(next.SeriesList) != 2 {
				t.Fatalf("next scanned without its series list: %+v", next)
			}
		})
	}
	// nw, in City Watch by its main series and again in its list, is one member.
	q, args := numberedSeriesMembers(lib.ID, "", "City Watch", all)
	var n int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+q+`) WHERE rel_path = 'nw'`, args...).Scan(&n); err != nil || n != 1 {
		t.Fatalf("nw counted %d times (%v)", n, err)
	}
}

// TestNextInSeries: the next book follows every series a book is in, its main
// series first: when the main series has ended another series it is in answers,
// a later book in the main series wins over one in another, a series the book
// has no position in is skipped, the end of every series is numbered, and the
// grant narrows a series reached through a list like any other (allowed and
// denied).
func TestNextInSeries(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Disc", Root: "/tmp/d"})
	add := func(path, series string, idx float64, more string) {
		t.Helper()
		b := &Book{LibraryID: lib.ID, RelPath: path, Title: path, Series: series, SeriesIndex: idx, AddedAt: "2024-01-01T00:00:00Z"}
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
		if more != "" {
			if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldMoreSeries: more}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	add("Saga/1", "Discworld", 8, `[{"name":"City Watch","position":1}]`)
	add("Saga/2", "Discworld", 3, "")
	add("Saga/2/Feet of Clay", "Ankh", 1, `[{"name":"City Watch","position":3}]`)
	add("Private/Men at Arms", "City Watch", 2, "")
	add("Lone/Mort", "Discworld", 0, `[{"name":"Death","position":1}]`)
	add("Lone/Reaper Man", "Death", 2, "")
	add("Loose/One", "Loose", 0, "")
	add("Loose/Two", "Loose", 0, "")
	all := Scope{LibraryID: lib.ID, AllowAll: true}
	granted := Scope{LibraryID: lib.ID, Paths: []string{"Saga", "Lone"}}
	next := func(path string, scope Scope) (string, bool) {
		t.Helper()
		book, err := c.GetBookByPath(ctx, lib.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		n, numbered, err := c.NextInSeries(ctx, lib.ID, book, scope)
		if err != nil {
			t.Fatal(err)
		}
		if n == nil {
			return "", numbered
		}
		if !numbered {
			t.Errorf("%s: next %s found but not numbered", path, n.RelPath)
		}
		return n.RelPath, numbered
	}
	for name, tc := range map[string]struct {
		path         string
		scope        Scope
		want         string
		wantNumbered bool
	}{
		// Discworld holds nothing after #8 (Saga/2 is #3), so City Watch answers:
		// Men at Arms, #2 by its main series.
		"main ended: a listed series": {"Saga/1", all, "Private/Men at Arms", true},
		// Men at Arms is outside the grant; Feet of Clay, City Watch #3 through
		// its list, is inside it.
		"denied: outside the grant": {"Saga/1", granted, "Saga/2/Feet of Clay", true},
		// Unnumbered in its main series: the series it is numbered in.
		"unnumbered main series skipped": {"Lone/Mort", all, "Lone/Reaper Man", true},
		// Nothing else in Ankh, and Feet of Clay is City Watch's last.
		"every series ended":  {"Saga/2/Feet of Clay", all, "", true},
		"no numbered series":  {"Loose/One", all, "", false},
		"alone in its series": {"Lone/Reaper Man", Scope{LibraryID: lib.ID, Paths: []string{"Lone/Reaper Man"}}, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got, numbered := next(tc.path, tc.scope); got != tc.want || numbered != tc.wantNumbered {
				t.Fatalf("next = %q, numbered = %v; want %q, %v", got, numbered, tc.want, tc.wantNumbered)
			}
		})
	}
	// The main series continuing wins, though City Watch continues too.
	add("Saga/9", "Discworld", 9, "")
	if got, _ := next("Saga/1", all); got != "Saga/9" {
		t.Fatalf("main continuing = %q, want Saga/9", got)
	}
}

// TestNextInSeriesNeverStepsBack: a later book in a lower-ranked series that sits
// at or before the book in a series ranked above it is no next book; it is passed
// over and the series' next later book judged. Narnia owned 1-6 (not The Last
// Battle), its main series chronological, its listed one the publication order:
// after The Silver Chair (chronological #6, publication #4) the publication
// order's later books, The Horse and His Boy (publication #5) and The Magician's
// Nephew (#6), are chronological #3 and #1, so nothing goes forward and the book
// is numbered-ended. In a second library that also holds The Last Battle, tagged
// only publication #7, both are passed over and The Last Battle answers. A later
// book in a third series that is later in the second too is followed (allowed),
// one earlier in it is not (denied).
func TestNextInSeriesNeverStepsBack(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Narnia", Root: "/tmp/n"})
	withLast, _ := c.CreateLibrary(ctx, Library{Name: "Narnia with The Last Battle", Root: "/tmp/n7"})
	into := lib.ID
	add := func(path, series string, idx float64, more string) {
		t.Helper()
		b := &Book{LibraryID: into, RelPath: path, Title: path, Series: series, SeriesIndex: idx, AddedAt: "2024-01-01T00:00:00Z"}
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
		if more != "" {
			if err := c.EditBook(ctx, into, path, BookEdit{Set: map[string]string{FieldMoreSeries: more}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	pub := func(pos int) string {
		return `[{"name":"Narnia (Publication)","position":` + strconv.Itoa(pos) + `}]`
	}
	add("Narnia/1 The Magician's Nephew", "Narnia", 1, pub(6))
	add("Narnia/2 The Lion, the Witch and the Wardrobe", "Narnia", 2, pub(1))
	add("Narnia/3 The Horse and His Boy", "Narnia", 3, pub(5))
	add("Narnia/4 Prince Caspian", "Narnia", 4, pub(2))
	add("Narnia/5 The Voyage of the Dawn Treader", "Narnia", 5, pub(3))
	add("Narnia/6 The Silver Chair", "Narnia", 6, pub(4))
	// Three series: A ended after #5; B's next (X, B #2) is A #2, a step back;
	// C's next (Y, C #2) is B #3, later than the current book's B #1.
	add("Three/Current", "A", 5, `[{"name":"B","position":1},{"name":"C","position":1}]`)
	add("Three/X", "A", 2, `[{"name":"B","position":2}]`)
	add("Three/Y", "B", 3, `[{"name":"C","position":2}]`)
	into = withLast.ID
	add("Narnia/1 The Magician's Nephew", "Narnia", 1, pub(6))
	add("Narnia/3 The Horse and His Boy", "Narnia", 3, pub(5))
	add("Narnia/6 The Silver Chair", "Narnia", 6, pub(4))
	add("Narnia/7 The Last Battle", "Narnia (Publication)", 7, "")
	for name, tc := range map[string]struct {
		lib, path, want string
		wantNumbered    bool
	}{
		"Silver Chair: no loop back":          {"", "Narnia/6 The Silver Chair", "", true},
		"main series continues":               {"", "Narnia/2 The Lion, the Witch and the Wardrobe", "Narnia/3 The Horse and His Boy", true},
		"allowed: later in both series":       {"", "Three/Current", "Three/Y", true},
		"the steps back passed over, not all": {"last", "Narnia/6 The Silver Chair", "Narnia/7 The Last Battle", true},
		"the main series continues there too": {"last", "Narnia/1 The Magician's Nephew", "Narnia/3 The Horse and His Boy", true},
	} {
		t.Run(name, func(t *testing.T) {
			id := lib.ID
			if tc.lib == "last" {
				id = withLast.ID
			}
			book, err := c.GetBookByPath(ctx, id, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			next, numbered, err := c.NextInSeries(ctx, id, book, Scope{LibraryID: id, AllowAll: true})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if next != nil {
				got = next.RelPath
			}
			if got != tc.want || numbered != tc.wantNumbered {
				t.Fatalf("next = %q, numbered = %v; want %q, %v", got, numbered, tc.want, tc.wantNumbered)
			}
		})
	}
	// stepsBack itself: a candidate later in the earlier series is a step
	// forward, one at or before it (or numbered in no shared series) is judged
	// only where both are numbered.
	earlier := []SeriesRef{{Name: "Narnia", Position: 2}, {Name: "Unnumbered", Position: 0}}
	for name, tc := range map[string]struct {
		theirs []SeriesRef
		want   bool
	}{
		"allowed: later in the earlier series": {[]SeriesRef{{Name: "Narnia", Position: 4}, {Name: "P", Position: 2}}, false},
		"denied: before":                       {[]SeriesRef{{Name: "Narnia", Position: 1}, {Name: "P", Position: 2}}, true},
		"denied: the same position":            {[]SeriesRef{{Name: "Narnia", Position: 2}, {Name: "P", Position: 2}}, true},
		"unnumbered in the shared series":      {[]SeriesRef{{Name: "Narnia", Position: 0}, {Name: "P", Position: 2}}, false},
		"the current book unnumbered there":    {[]SeriesRef{{Name: "Unnumbered", Position: 1}, {Name: "P", Position: 2}}, false},
		"no shared series":                     {[]SeriesRef{{Name: "P", Position: 2}, {Name: "Q", Position: 1}}, false},
	} {
		if got := stepsBack(earlier, &Book{SeriesList: tc.theirs}); got != tc.want {
			t.Errorf("%s: stepsBack = %v, want %v", name, got, tc.want)
		}
	}
}

// TestNextInSeriesPlan: both branches of the membership query are index
// searches, never a walk of the library's books: the main series by
// idx_books_series, the lists by the full-text index's series column, each
// candidate looked up by rowid in the partial idx_books_more_series (a name with
// no phrase to match reads that partial index alone).
func TestNextInSeriesPlan(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	for _, scope := range []Scope{{LibraryID: 1, AllowAll: true}, {LibraryID: 1, Paths: []string{"A", "B"}}} {
		for series, lists := range map[string][]string{
			"S":  {"SCAN books_fts VIRTUAL TABLE", "SEARCH b USING INDEX idx_books_more_series (library_id=? AND rowid=?)"},
			"!!": {"SEARCH b USING INDEX idx_books_more_series (library_id=?)"},
		} {
			q, args := laterSeriesMembers(1, "x", series, 1, scope)
			plan := queryPlan(t, c, q, args)
			joined := strings.Join(plan, "\n")
			for _, want := range append([]string{"SEARCH b USING INDEX idx_books_series"}, lists...) {
				if !strings.Contains(joined, want) {
					t.Errorf("%s: plan does not use %s:\n%s", series, want, joined)
				}
			}
			for _, line := range plan {
				if strings.HasPrefix(line, "SCAN b") && !strings.Contains(line, "INDEX") {
					t.Errorf("%s: plan walks the books table: %q", series, line)
				}
			}
		}
	}
}

// queryPlan is the detail lines of q's EXPLAIN QUERY PLAN.
func queryPlan(t *testing.T, c *Catalog, q string, args []any) []string {
	t.Helper()
	rows, err := c.db.QueryContext(context.Background(), `EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}
