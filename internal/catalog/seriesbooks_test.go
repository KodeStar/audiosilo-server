package catalog

import (
	"context"
	"reflect"
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

func TestNextInSeries(t *testing.T) {
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
			next, numbered, err := c.NextInSeries(ctx, lib.ID, tc.path, tc.series, tc.index, tc.scope)
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
}
