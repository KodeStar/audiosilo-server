package catalog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestMoreSeries: a book's other series are an override field like any other -
// validated to a canonical list, shown in series_list after its main series,
// found by the series= filter and search, ordered by its position in the series
// a list is filtered to, and counted in the series aggregate (as extra books).
func TestMoreSeries(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	const watch = "City Watch"
	for _, b := range []*Book{
		{RelPath: "gg", Title: "Guards! Guards!", Series: "Discworld", SeriesIndex: 8},
		{RelPath: "maa", Title: "Men at Arms", Series: watch, SeriesIndex: 2},
		{RelPath: "mort", Title: "Mort", Series: "Discworld", SeriesIndex: 4},
	} {
		b.LibraryID, b.Format = lib.ID, "m4b"
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	// A blank name, a repeat and the padding are cleaned away.
	set := func(path, v string) error {
		return c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldMoreSeries: v}})
	}
	if err := set("gg", ` [{"name":" City Watch ","position":1},{"name":"","position":3},{"name":"City Watch","position":9}] `); err != nil {
		t.Fatal(err)
	}
	d, _ := c.AdminBookDetail(ctx, lib.ID, "gg")
	if got := d.Fields[FieldMoreSeries]; got.Value != `[{"name":"City Watch","position":1}]` || got.Source != SourceEdited {
		t.Fatalf("stored = %+v", got)
	}
	if want := []SeriesRef{{"Discworld", 8}, {watch, 1}}; !reflect.DeepEqual(d.Book.SeriesList, want) {
		t.Fatalf("series_list = %+v, want %+v", d.Book.SeriesList, want)
	}
	// Written as the console writes it (no HTML escaping), so its drafts compare.
	if got, err := normalizeMoreSeries(`[{"name":"Tom & Jerry <1>","position":1}]`); err != nil || got != `[{"name":"Tom & Jerry <1>","position":1}]` {
		t.Fatalf("canonical form = %s %v", got, err)
	}
	for name, bad := range map[string]string{
		"not a list": `{"name":"x"}`, "bad position": `[{"name":"x","position":"2"}]`,
		"out of range": `[{"name":"x","position":100001}]`, "control chars": "[{\"name\":\"a\\u0000b\"}]",
	} {
		if err := set("mort", bad); !errors.Is(err, ErrInvalidOverride) {
			t.Errorf("%s: err = %v, want ErrInvalidOverride", name, err)
		}
	}

	list := func(opt AdminListOptions) []string {
		t.Helper()
		page, err := c.ListAdminBooks(ctx, opt)
		if err != nil {
			t.Fatal(err)
		}
		return paths(page.Books)
	}
	// City Watch: Guards! Guards! (#1 there) before Men at Arms (#2), though its
	// main series puts it at 8.
	if got := list(AdminListOptions{Filter: BookFilter{Series: watch}, Sort: "series"}); !reflect.DeepEqual(got, []string{"gg", "maa"}) {
		t.Errorf("City Watch in order = %v", got)
	}
	if got := list(AdminListOptions{Filter: BookFilter{Series: watch}, Sort: "series", Desc: true, Limit: 1}); !reflect.DeepEqual(got, []string{"maa"}) {
		t.Errorf("City Watch descending first page = %v", got)
	}
	if got := listAll(t, c, ctx, AdminListOptions{Filter: BookFilter{Series: watch}, Sort: "series", Limit: 1}); !reflect.DeepEqual(got, []string{"gg", "maa"}) {
		t.Errorf("City Watch paged = %v", got)
	}
	if got := list(AdminListOptions{Filter: BookFilter{Series: "Discworld"}, Sort: "series"}); !reflect.DeepEqual(got, []string{"mort", "gg"}) {
		t.Errorf("Discworld in order = %v", got)
	}
	if got := list(AdminListOptions{Filter: BookFilter{Query: "city watch"}}); !reflect.DeepEqual(got, []string{"gg", "maa"}) {
		t.Errorf("search by another series = %v", got)
	}

	counts := func(memberships bool) map[string][2]int {
		t.Helper()
		series, err := c.Series(ctx, lib.ID, nil, memberships)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][2]int{}
		for _, s := range series {
			out[s.Name] = [2]int{s.Books, s.ExtraBooks}
		}
		return out
	}
	if got, want := counts(true), map[string][2]int{"Discworld": {2, 0}, watch: {2, 1}}; !reflect.DeepEqual(got, want) {
		t.Errorf("series with memberships = %v, want %v", got, want)
	}
	if got, want := counts(false), map[string][2]int{"Discworld": {2, 0}, watch: {1, 0}}; !reflect.DeepEqual(got, want) {
		t.Errorf("series by main series only = %v, want %v", got, want)
	}

	// Reverting drops the list everywhere.
	if err := c.EditBook(ctx, lib.ID, "gg", BookEdit{Revert: []string{FieldMoreSeries}}); err != nil {
		t.Fatal(err)
	}
	if got := list(AdminListOptions{Filter: BookFilter{Series: watch}}); !reflect.DeepEqual(got, []string{"maa"}) {
		t.Errorf("City Watch after the revert = %v", got)
	}
}

// TestMoreSeriesPlayer: the player's list matches a book's other series only when
// the client asks (memberships), a player book carries series_list only when it
// is in more than one series, and SeriesBooks (rail placement) finds a book in a
// rail's series only through its more_series too, in placement order and within
// the grant (allowed and denied).
func TestMoreSeriesPlayer(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	for _, b := range []*Book{
		{RelPath: "gg", Title: "Guards! Guards!", Series: "Discworld", SeriesIndex: 8},
		{RelPath: "maa", Title: "Men at Arms", Series: "City Watch", SeriesIndex: 2},
	} {
		b.LibraryID, b.Format = lib.ID, "m4b"
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.EditBook(ctx, lib.ID, "gg", BookEdit{Set: map[string]string{FieldMoreSeries: `[{"name":"City Watch","position":1}]`}}); err != nil {
		t.Fatal(err)
	}
	list := func(memberships bool) []string {
		t.Helper()
		page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Series: "City Watch", Memberships: memberships, Sort: "title"})
		if err != nil {
			t.Fatal(err)
		}
		return relPaths(page.Books)
	}
	if got := list(false); !reflect.DeepEqual(got, []string{"maa"}) {
		t.Errorf("main series only = %v", got)
	}
	if got := list(true); !reflect.DeepEqual(got, []string{"gg", "maa"}) {
		t.Errorf("with memberships = %v", got)
	}
	// A list that only repeats the main series adds no series_list.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "foc", Title: "Feet of Clay", Series: "City Watch", SeriesIndex: 3, Format: "m4b"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, lib.ID, "foc", BookEdit{Set: map[string]string{FieldMoreSeries: `[{"name":"City Watch","position":3}]`}}); err != nil {
		t.Fatal(err)
	}
	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Sort: "title"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range page.Books {
		switch b.RelPath {
		case "gg":
			if want := []SeriesRef{{"Discworld", 8}, {"City Watch", 1}}; !reflect.DeepEqual(b.SeriesList, want) {
				t.Errorf("gg series_list = %+v", b.SeriesList)
			}
		case "maa", "foc":
			if b.SeriesList != nil {
				t.Errorf("%s, in one series, carries series_list %+v", b.RelPath, b.SeriesList)
			}
		}
	}
	// City Watch's rail finds its books by their main series and gg through its
	// other series, all in placement order: library sort order, then path - so
	// Night Watch, in City Watch only through its list but in the library sorted
	// first, comes before every book of the other.
	attic, _ := c.CreateLibrary(ctx, Library{Name: "Attic", Root: "/tmp/a"})
	if err := c.ReorderLibraries(ctx, []int64{attic.ID, lib.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: attic.ID, RelPath: "nw", Title: "Night Watch", Series: "Discworld", SeriesIndex: 29, Format: "m4b"}); err != nil {
		t.Fatal(err)
	}
	if err := c.EditBook(ctx, attic.ID, "nw", BookEdit{Set: map[string]string{FieldMoreSeries: `[{"name":"City Watch","position":6}]`}}); err != nil {
		t.Fatal(err)
	}
	rail := func(scopes []Scope, name string) []string {
		t.Helper()
		found, err := c.SeriesBooks(ctx, scopes, []string{name})
		if err != nil {
			t.Fatal(err)
		}
		return relPaths(found)
	}
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}, {LibraryID: attic.ID, AllowAll: true}}
	if got, want := rail(all, "City Watch"), []string{"nw", "foc", "gg", "maa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("City Watch rail books = %v, want %v", got, want)
	}
	// Denied: a caller granted only maa (and nothing of the attic) never gets a
	// book outside the grant through its more_series.
	granted := []Scope{{LibraryID: lib.ID, Paths: []string{"maa"}}, {LibraryID: attic.ID, Paths: []string{"elsewhere"}}}
	if got, want := rail(granted, "city  watch!"), []string{"maa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("granted maa only: City Watch rail books = %v, want %v", got, want)
	}
	if got := rail(all, "Unknown"); len(got) != 0 {
		t.Errorf("no series named so, found %v", got)
	}
}

// TestSeriesSwap: an edit that makes one of a book's other series its main one
// swaps the two (seriesSwap) - on one book, and per book in a bulk edit, by
// setting series or by reverting it to a listed one - with the derived values
// written as the edit's own overrides; an edit that names more_series, a series
// the list doesn't hold, or a community source swaps nothing, one that names
// series_index keeps its own position, and an old main series that can't be
// listed refuses the edit.
func TestSeriesSwap(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	uid := seedUser(t, c, ctx)
	const watch, disc = "City Watch", "Discworld"
	add := func(path, series string, idx float64, more string) {
		t.Helper()
		b := &Book{LibraryID: lib.ID, RelPath: path, Title: path, Series: series, SeriesIndex: idx, Format: "m4b"}
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
		if more != "" {
			if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldMoreSeries: more}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	detail := func(path string) *AdminBookDetail {
		t.Helper()
		d, err := c.AdminBookDetail(ctx, lib.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	fields := func(path string) map[string]FieldValue { t.Helper(); return detail(path).Fields }
	// check asserts a book's main series, position and other series.
	check := func(path, series, idx, more string) {
		t.Helper()
		f := fields(path)
		if f[FieldSeries].Value != series || f[FieldSeriesIndex].Value != idx || f[FieldMoreSeries].Value != more {
			t.Errorf("%s = %q #%q more %s, want %q #%q more %s", path,
				f[FieldSeries].Value, f[FieldSeriesIndex].Value, f[FieldMoreSeries].Value, series, idx, more)
		}
	}
	edit := func(path string, e BookEdit) {
		t.Helper()
		if err := c.EditBook(ctx, lib.ID, path, e); err != nil {
			t.Fatal(err)
		}
	}
	setSeries := func(name string) map[string]string { return map[string]string{FieldSeries: name} }

	// One book: City Watch becomes the main series at its position there, and
	// Discworld takes its place in the list, at the position the book had in it.
	add("gg", disc, 8, `[{"name":"Omnibus","position":0},{"name":"City Watch","position":1},{"name":"Extra","position":2}]`)
	edit("gg", BookEdit{Set: setSeries(watch), UserID: uid})
	check("gg", watch, "1", `[{"name":"Omnibus","position":0},{"name":"Discworld","position":8},{"name":"Extra","position":2}]`)
	for _, field := range []string{FieldMoreSeries, FieldSeriesIndex} {
		if f := fields("gg")[field]; f.Source != SourceEdited || !f.Locked || f.EditedBy != "u" {
			t.Errorf("derived %s = %+v, want an edit by u", field, f)
		}
	}
	if got, want := detail("gg").Book.SeriesList, []SeriesRef{{watch, 1}, {"Omnibus", 0}, {disc, 8}, {"Extra", 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("series_list = %+v, want %+v", got, want)
	}
	// Back again swaps back.
	edit("gg", BookEdit{Set: setSeries(disc)})
	check("gg", disc, "8", `[{"name":"Omnibus","position":0},{"name":"City Watch","position":1},{"name":"Extra","position":2}]`)
	// Reverting series swaps back too: the scanned Discworld is listed, so it
	// is the main series again at its position there, City Watch back in the
	// list. Setting City Watch again (the revert's undo) swaps again: a round
	// trip.
	edit("gg", BookEdit{Set: setSeries(watch)})
	edit("gg", BookEdit{Revert: []string{FieldSeries}, UserID: uid})
	check("gg", disc, "8", `[{"name":"Omnibus","position":0},{"name":"City Watch","position":1},{"name":"Extra","position":2}]`)
	if f := fields("gg")[FieldSeries]; f.Locked || f.Source != SourceTag {
		t.Errorf("series after the revert = %+v, want the scanned tag", f)
	}
	if got, want := detail("gg").Book.SeriesList, []SeriesRef{{disc, 8}, {"Omnibus", 0}, {watch, 1}, {"Extra", 2}}; !reflect.DeepEqual(got, want) {
		t.Errorf("series_list after reverting series = %+v, want %+v", got, want)
	}
	edit("gg", BookEdit{Set: setSeries(watch)})
	check("gg", watch, "1", `[{"name":"Omnibus","position":0},{"name":"Discworld","position":8},{"name":"Extra","position":2}]`)
	// A revert follows a set's rules: leaving a series the list doesn't hold, or
	// reverting more_series too, swaps nothing; naming series_index keeps the
	// edit's own position.
	add("revplain", disc, 8, `[{"name":"City Watch","position":1}]`)
	edit("revplain", BookEdit{Set: setSeries("Ankh")})
	edit("revplain", BookEdit{Revert: []string{FieldSeries}})
	check("revplain", disc, "8", `[{"name":"City Watch","position":1}]`)
	add("revmore", disc, 8, `[{"name":"City Watch","position":1}]`)
	edit("revmore", BookEdit{Set: setSeries(watch)})
	edit("revmore", BookEdit{Revert: []string{FieldSeries, FieldMoreSeries}})
	check("revmore", disc, "1", "")
	add("revidx", disc, 8, `[{"name":"City Watch","position":1}]`)
	edit("revidx", BookEdit{Set: setSeries(watch)})
	edit("revidx", BookEdit{Set: map[string]string{FieldSeriesIndex: "5"}, Revert: []string{FieldSeries}})
	check("revidx", disc, "5", `[{"name":"City Watch","position":1}]`)

	// A bulk edit swaps each book that lists the series, and only those.
	add("b1", disc, 3, `[{"name":"City Watch","position":4}]`)
	add("b2", disc, 5, `[{"name":"Omnibus","position":1}]`)
	add("b3", disc, 6, "")
	if err := c.EditBooks(ctx, []Ref{{lib.ID, "b1"}, {lib.ID, "b2"}, {lib.ID, "b3"}},
		BookEdit{Set: setSeries(watch), UserID: uid}); err != nil {
		t.Fatal(err)
	}
	check("b1", watch, "4", `[{"name":"Discworld","position":3}]`)
	check("b2", watch, "5", `[{"name":"Omnibus","position":1}]`)
	check("b3", watch, "6", "")
	if f := fields("b2")[FieldSeriesIndex]; f.Locked {
		t.Errorf("b2's position was written without a swap: %+v", f)
	}

	// An edit naming more_series (set or reverted) swaps nothing.
	add("own", disc, 2, `[{"name":"City Watch","position":7}]`)
	edit("own", BookEdit{Set: map[string]string{FieldSeries: watch, FieldMoreSeries: `[{"name":"Omnibus","position":1}]`}})
	check("own", watch, "2", `[{"name":"Omnibus","position":1}]`)
	add("rev", disc, 2, `[{"name":"City Watch","position":7}]`)
	edit("rev", BookEdit{Set: setSeries(watch), Revert: []string{FieldMoreSeries}})
	check("rev", watch, "2", "")

	// An edit naming series_index keeps its own position (or the scanned one).
	add("idx", disc, 2, `[{"name":"City Watch","position":7}]`)
	edit("idx", BookEdit{Set: map[string]string{FieldSeries: watch, FieldSeriesIndex: "9"}})
	check("idx", watch, "9", `[{"name":"Discworld","position":2}]`)
	add("idxrev", disc, 2, `[{"name":"City Watch","position":7}]`)
	edit("idxrev", BookEdit{Set: setSeries(watch), Revert: []string{FieldSeriesIndex}})
	check("idxrev", watch, "2", `[{"name":"Discworld","position":2}]`)

	// No main series before: the entry just leaves the list.
	add("none", "", 0, `[{"name":"City Watch","position":2},{"name":"Omnibus","position":3}]`)
	edit("none", BookEdit{Set: setSeries(watch)})
	check("none", watch, "2", `[{"name":"Omnibus","position":3}]`)

	// The old main series already listed (shadowed) gives way to the old main
	// at the new one's place, so it is listed once, at the book's position.
	add("shadow", disc, 8, `[{"name":"Discworld","position":3},{"name":"City Watch","position":1}]`)
	edit("shadow", BookEdit{Set: setSeries(watch)})
	check("shadow", watch, "1", `[{"name":"Discworld","position":8}]`)

	// The same series again, another spelling, or a blank series: no swap.
	add("same", watch, 1, `[{"name":"City Watch","position":4},{"name":"Discworld","position":8}]`)
	edit("same", BookEdit{Set: setSeries(watch)})
	check("same", watch, "1", `[{"name":"City Watch","position":4},{"name":"Discworld","position":8}]`)
	add("case", disc, 8, `[{"name":"City Watch","position":1}]`)
	edit("case", BookEdit{Set: setSeries("city watch")})
	check("case", "city watch", "8", `[{"name":"City Watch","position":1}]`)
	add("blank", disc, 8, `[{"name":"City Watch","position":1}]`)
	edit("blank", BookEdit{Set: setSeries("")})
	check("blank", "", "8", `[{"name":"City Watch","position":1}]`)

	// A community edit (a match apply) never swaps: the match plan lays out
	// series itself.
	add("comm", disc, 8, `[{"name":"City Watch","position":1}]`)
	edit("comm", BookEdit{Set: setSeries(watch), Source: SourceCommunity})
	check("comm", watch, "8", `[{"name":"City Watch","position":1}]`)

	// An old main series that can't be listed (a tag name too long for an
	// entry, a position out of range) refuses the edit as an invalid series,
	// and a bulk edit holding such a book changes none.
	long := strings.Repeat("x", maxShortField+1)
	add("long", long, 8, `[{"name":"City Watch","position":1}]`)
	add("far", disc, maxSeriesIndex+1, `[{"name":"City Watch","position":1}]`)
	add("fine", disc, 8, `[{"name":"City Watch","position":1}]`)
	for _, path := range []string{"long", "far"} {
		err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: setSeries(watch)})
		var oe *OverrideError
		if !errors.As(err, &oe) || oe.Field != FieldSeries || !strings.Contains(oe.Reason, "can't move to the other series") {
			t.Errorf("%s: unlistable old main = %v, want an invalid series", path, err)
		}
	}
	check("long", long, "8", `[{"name":"City Watch","position":1}]`)
	// The bulk refusal names the book that refused, so the admin can find it.
	if err := c.EditBooks(ctx, []Ref{{lib.ID, "fine"}, {lib.ID, "far"}}, BookEdit{Set: setSeries(watch)}); !errors.Is(err, ErrInvalidOverride) || !strings.Contains(err.Error(), "(far)") {
		t.Errorf("bulk edit with an unlistable book = %v, want ErrInvalidOverride naming far", err)
	}
	check("fine", disc, "8", `[{"name":"City Watch","position":1}]`)
}

// TestSeriesSwapUneditedValues: a swap's derived value equal to what its field
// resolves to without an override is written as a revert of that field, not an
// edit. A path-derived Sherlock Holmes #5 with an edited other series Other #1:
// making Other the main series edits both (Other #1 is no path value, and the
// list now holds Sherlock Holmes #5); reverting series swaps back, and #5 is the
// path's own position again, so series_index comes back unlocked from the path
// while the list, an edit before the swap, stays one. A position equal to the
// path's but in another series is still an edit. A derived list as empty as the
// unedited one is a revert too.
func TestSeriesSwapUneditedValues(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Shelf", Root: "/tmp/s"})
	uid := seedUser(t, c, ctx)
	const p = "Arthur Conan Doyle/Sherlock Holmes/05 - The Hound of the Baskervilles"
	add := func(path, series string, idx float64, more string) {
		t.Helper()
		b := &Book{LibraryID: lib.ID, RelPath: path, IsFolder: true, Title: "The Hound of the Baskervilles",
			Author: "Arthur Conan Doyle", Series: series, SeriesIndex: idx, AddedAt: "2024-01-01T00:00:00Z"}
		if _, err := c.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := c.EditBook(ctx, lib.ID, path, BookEdit{Set: map[string]string{FieldMoreSeries: more}, UserID: uid}); err != nil {
			t.Fatal(err)
		}
	}
	type want struct {
		value, source string
		locked        bool
	}
	check := func(path, step string, wants map[string]want) {
		t.Helper()
		d, err := c.AdminBookDetail(ctx, lib.ID, path)
		if err != nil {
			t.Fatal(err)
		}
		for field, w := range wants {
			if f := d.Fields[field]; f.Value != w.value || f.Source != w.source || f.Locked != w.locked {
				t.Errorf("%s: %s = %q (%s, locked %v), want %q (%s, locked %v)", step, field, f.Value, f.Source, f.Locked, w.value, w.source, w.locked)
			}
		}
	}
	edit := func(path string, e BookEdit) {
		t.Helper()
		e.UserID = uid
		if err := c.EditBook(ctx, lib.ID, path, e); err != nil {
			t.Fatal(err)
		}
	}

	add(p, "Sherlock Holmes", 5, `[{"name":"Other","position":1}]`)
	check(p, "before", map[string]want{
		FieldSeries:      {"Sherlock Holmes", SourcePath, false},
		FieldSeriesIndex: {"5", SourcePath, false},
		FieldMoreSeries:  {`[{"name":"Other","position":1}]`, SourceEdited, true},
	})
	edit(p, BookEdit{Set: map[string]string{FieldSeries: "Other"}})
	check(p, "swapped", map[string]want{
		FieldSeries:      {"Other", SourceEdited, true},
		FieldSeriesIndex: {"1", SourceEdited, true},
		FieldMoreSeries:  {`[{"name":"Sherlock Holmes","position":5}]`, SourceEdited, true},
	})
	edit(p, BookEdit{Revert: []string{FieldSeries}})
	check(p, "swapped back", map[string]want{
		FieldSeries:      {"Sherlock Holmes", SourcePath, false},
		FieldSeriesIndex: {"5", SourcePath, false},
		FieldMoreSeries:  {`[{"name":"Other","position":1}]`, SourceEdited, true},
	})
	// Setting the path's series back (not reverting it) follows the same rule:
	// series is the edit's own, the position it brings back is the path's.
	edit(p, BookEdit{Set: map[string]string{FieldSeries: "Other"}})
	edit(p, BookEdit{Set: map[string]string{FieldSeries: "Sherlock Holmes"}})
	check(p, "set back", map[string]want{
		FieldSeries:      {"Sherlock Holmes", SourceEdited, true},
		FieldSeriesIndex: {"5", SourcePath, false},
		FieldMoreSeries:  {`[{"name":"Other","position":1}]`, SourceEdited, true},
	})

	// The same number in another series is no file value: the path's 5 numbers
	// Sherlock Holmes, so making a listed Other #5 the main series edits the
	// position (locked), as the path says nothing of Other.
	const same = "Arthur Conan Doyle/Sherlock Holmes/05 - The Valley of Fear"
	add(same, "Sherlock Holmes", 5, `[{"name":"Other","position":5}]`)
	edit(same, BookEdit{Set: map[string]string{FieldSeries: "Other"}})
	check(same, "another series' equal number", map[string]want{
		FieldSeries:      {"Other", SourceEdited, true},
		FieldSeriesIndex: {"5", SourceEdited, true},
		FieldMoreSeries:  {`[{"name":"Sherlock Holmes","position":5}]`, SourceEdited, true},
	})

	// No main series before, one entry: the swap empties the list, as the
	// unedited value is, so the list's override is reverted.
	const lone = "Arthur Conan Doyle/Loose"
	add(lone, "", 0, `[{"name":"Other","position":2}]`)
	edit(lone, BookEdit{Set: map[string]string{FieldSeries: "Other"}})
	check(lone, "lone swapped", map[string]want{
		FieldSeries:      {"Other", SourceEdited, true},
		FieldSeriesIndex: {"2", SourceEdited, true},
		FieldMoreSeries:  {"", "", false},
	})
}
