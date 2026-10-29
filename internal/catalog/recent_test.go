package catalog

import (
	"fmt"
	"strings"
	"testing"
)

// TestRecentBooksPlan: RecentBooks' query reads each scope inside its own
// library and stops at the limit: one scope walks idx_books_lib_added; with
// more, each scope is an arm that walks it too, but for an arm of one path rule,
// which seeks the path (sqlite_autoindex_books_1). Never the OR plan's read of
// every book in scope (idx_books_series_key), a walk of every library's books
// (idx_books_added) or a scan of books.
func TestRecentBooksPlan(t *testing.T) {
	t.Parallel()
	c, _ := newTestCatalog(t)
	for name, tc := range map[string]struct {
		scopes []Scope
		walks  int  // arms walking idx_books_lib_added
		seek   bool // an arm seeking its one path
	}{
		"one scope":  {[]Scope{{LibraryID: 1, AllowAll: true}}, 1, false},
		"two scopes": {[]Scope{{LibraryID: 1, AllowAll: true}, {LibraryID: 2, AllowAll: true}}, 2, false},
		"shared paths": {[]Scope{
			{LibraryID: 1, AllowAll: true},
			{LibraryID: 2, Paths: []string{"A"}},
			{LibraryID: 3, Paths: []string{"A", "B/C"}},
		}, 2, true},
	} {
		q, args := recentBooksQuery(tc.scopes, 50)
		plan := queryPlan(t, c, q, args)
		joined := strings.Join(plan, "\n")
		if n := strings.Count(joined, "INDEX idx_books_lib_added (library_id=?)"); n != tc.walks {
			t.Errorf("%s: %d arms walk idx_books_lib_added, want %d:\n%s", name, n, tc.walks, joined)
		}
		if got := strings.Contains(joined, "INDEX sqlite_autoindex_books_1 (library_id=? AND rel_path"); got != tc.seek {
			t.Errorf("%s: seeks a path = %v, want %v:\n%s", name, got, tc.seek, joined)
		}
		for _, bad := range []string{"idx_books_series_key", "idx_books_added"} {
			if strings.Contains(joined, bad) {
				t.Errorf("%s: plan uses %s, which reads past each scope's newest books:\n%s", name, bad, joined)
			}
		}
		for _, line := range plan {
			if strings.HasPrefix(line, "SCAN b") {
				t.Errorf("%s: plan scans the books: %q", name, line)
			}
		}
		if len(tc.scopes) == 1 && strings.Contains(joined, "TEMP B-TREE") {
			t.Errorf("%s: one scope sorts:\n%s", name, joined)
		}
	}
}

// TestRecentBooksScopes: the answer across two book libraries and a podcasts
// library: newest added first across libraries, each scope's path rules kept
// (one path or several), the limit honoured, no book of a library outside the
// scopes and no podcast episode (a books row in a podcasts library) however
// new.
func TestRecentBooksScopes(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: t.TempDir()})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: t.TempDir()})
	libC, _ := c.CreateLibrary(ctx, Library{Name: "C", Root: t.TempDir()})
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: t.TempDir(), Kind: KindPodcasts})
	if err != nil {
		t.Fatal(err)
	}
	add := func(lib int64, rel string, day int) {
		t.Helper()
		upsert(t, c, &Book{LibraryID: lib, RelPath: rel, Title: rel, AddedAt: fmt.Sprintf("2024-01-%02dT00:00:00Z", day)})
	}
	add(libA.ID, "a1", 1)
	add(libB.ID, "b2", 2)
	add(libA.ID, "a3", 3)
	add(libB.ID, "hidden/b4", 4) // outside B's shared paths
	add(libB.ID, "shared/b5", 5)
	add(libA.ID, "a6", 6)
	add(pods.ID, "Show/ep7", 7) // newer, but an episode
	add(pods.ID, "Show/ep8", 8)
	add(libC.ID, "c9", 9) // newest, in no scope

	recent := func(scopes []Scope, limit int) []string {
		t.Helper()
		got, err := c.RecentBooks(ctx, scopes, limit)
		if err != nil {
			t.Fatal(err)
		}
		return relPaths(got)
	}
	scopes := []Scope{
		{LibraryID: libA.ID, AllowAll: true},
		{LibraryID: libB.ID, Paths: []string{"shared", "b2"}},
		{LibraryID: pods.ID, AllowAll: true},
	}
	wantPaths(t, "recent", recent(scopes, 10), "a6", "shared/b5", "a3", "b2", "a1")
	wantPaths(t, "recent, limit 3", recent(scopes, 3), "a6", "shared/b5", "a3")
	onePath := []Scope{{LibraryID: libA.ID, AllowAll: true}, {LibraryID: libB.ID, Paths: []string{"shared"}}}
	wantPaths(t, "recent, one path", recent(onePath, 10), "a6", "shared/b5", "a3", "a1")
	// A books scope and a podcasts scope (a podcast-capable client's Home) is two
	// scopes too: the episodes still never show.
	booksPods := []Scope{{LibraryID: libB.ID, AllowAll: true}, {LibraryID: pods.ID, AllowAll: true}}
	wantPaths(t, "books + podcasts", recent(booksPods, 2), "shared/b5", "hidden/b4")
}
