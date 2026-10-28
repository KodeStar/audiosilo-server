package catalog

import (
	"fmt"
	"strings"
	"testing"
)

// TestRecentBooksPlan: RecentBooks' query walks an added-at index and stops at the
// limit for any number of scopes, never a read of every book in scope into a sort.
// One scope uses idx_books_lib_added (the planner's own pick); two or more walk
// idx_books_added (INDEXED BY), where left to itself the planner ORs the arms over
// idx_books_series_key and sorts the lot.
func TestRecentBooksPlan(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	for name, tc := range map[string]struct {
		scopes []Scope
		index  string
	}{
		"one scope":  {[]Scope{{LibraryID: 1, AllowAll: true}}, "idx_books_lib_added"},
		"two scopes": {[]Scope{{LibraryID: 1, AllowAll: true}, {LibraryID: 2, AllowAll: true}}, "idx_books_added"},
		"shared paths": {[]Scope{
			{LibraryID: 1, AllowAll: true},
			{LibraryID: 2, Paths: []string{"A", "B/C"}},
			{LibraryID: 3, AllowAll: true},
		}, "idx_books_added"},
	} {
		q, args := recentBooksQuery(tc.scopes, 50)
		plan := queryPlan(t, c, q, args)
		joined := strings.Join(plan, "\n")
		if !strings.Contains(joined, "USING INDEX "+tc.index) {
			t.Errorf("%s: plan does not walk %s:\n%s", name, tc.index, joined)
		}
		for _, bad := range []string{"TEMP B-TREE", "MULTI-INDEX OR", "idx_books_series_key"} {
			if strings.Contains(joined, bad) {
				t.Errorf("%s: plan has %s (reads every book in scope, then sorts):\n%s", name, bad, joined)
			}
		}
	}
}

// TestRecentBooksTwoScopes: the INDEXED BY query's answer across two book
// libraries and a podcasts library: newest added first across libraries, each
// scope's path rules kept, the limit honoured, and no podcast episode (a books
// row in a podcasts library) however new.
func TestRecentBooksTwoScopes(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: t.TempDir()})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: t.TempDir()})
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	add := func(lib int64, rel string, day int) {
		t.Helper()
		b := Book{LibraryID: lib, RelPath: rel, Title: rel, AddedAt: fmt.Sprintf("2024-01-%02dT00:00:00Z", day)}
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	add(libA.ID, "a1", 1)
	add(libB.ID, "b2", 2)
	add(libA.ID, "a3", 3)
	add(libB.ID, "hidden/b4", 4) // outside B's shared path
	add(libB.ID, "shared/b5", 5)
	add(libA.ID, "a6", 6)
	add(pods.ID, "Show/ep7", 7) // newest of all, but an episode
	add(pods.ID, "Show/ep8", 8)

	scopes := []Scope{
		{LibraryID: libA.ID, AllowAll: true},
		{LibraryID: libB.ID, Paths: []string{"shared", "b2"}},
		{LibraryID: pods.ID, AllowAll: true},
	}
	titles := func(limit int) []string {
		t.Helper()
		got, err := c.RecentBooks(ctx, scopes, limit)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, b := range got {
			out = append(out, b.Title)
		}
		return out
	}
	if got, want := strings.Join(titles(10), ","), "a6,shared/b5,a3,b2,a1"; got != want {
		t.Fatalf("recent = %s, want %s", got, want)
	}
	if got, want := strings.Join(titles(3), ","), "a6,shared/b5,a3"; got != want {
		t.Fatalf("recent limit 3 = %s, want %s", got, want)
	}
	// A books scope and a podcasts scope (a podcast-capable client's Home) is two
	// scopes too: the episodes still never show.
	only := []Scope{{LibraryID: libB.ID, AllowAll: true}, {LibraryID: pods.ID, AllowAll: true}}
	got, err := c.RecentBooks(ctx, only, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "shared/b5" || got[1].Title != "hidden/b4" {
		t.Fatalf("books + podcasts recent = %+v", got)
	}
}
