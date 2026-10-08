package catalog

import (
	"errors"
	"reflect"
	"testing"
)

// TestMoreSeries: a book's other series are an override field like any other -
// validated to a canonical list, shown in series_list after its main series,
// found by the series= filter and search, ordered by its position in the series
// a list is filtered to, and counted in the series aggregate (as extra books).
func TestMoreSeries(t *testing.T) {
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
