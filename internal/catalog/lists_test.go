package catalog

import (
	"strings"
	"testing"
)

// itemPaths reads a collection's items as paths, in order, as its owner sees them.
func itemPaths(t *testing.T, c *Catalog, id, owner int64, scopes []Scope) []string {
	t.Helper()
	_, items, err := c.CollectionDetail(t.Context(), id, owner, scopes)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.Path)
	}
	return out
}

// countRows counts a table's rows matching a condition (test-only SQL).
func countRows(t *testing.T, c *Catalog, query string, args ...any) int {
	t.Helper()
	var n int
	if err := c.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A move carries up-next entries and collection items to the new path, keeping
// their positions; where a list already holds the new path, that entry (and its
// position) stays and the moved one goes.
func TestMoveCarriesLists(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	ann, bob := seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob")
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	queue := func(user int64, paths ...string) {
		t.Helper()
		for _, p := range paths {
			if err := c.AddToQueue(ctx, user, ref(p), nil, all); err != nil {
				t.Fatal(err)
			}
		}
	}
	// ann has the old path only; bob has both, the new one first (a stale entry
	// of a removed book at the destination).
	queue(ann, "x", "old", "y")
	queue(bob, "new", "x", "old")
	annCol, _ := c.CreateCollection(ctx, ann, "Ann's", "")
	bobCol, _ := c.CreateCollection(ctx, bob, "Bob's", "")
	for _, p := range []string{"x", "old", "y"} {
		if err := c.AddCollectionItem(ctx, annCol.ID, ann, ref(p), nil, all); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"old", "new"} {
		if err := c.AddCollectionItem(ctx, bobCol.ID, bob, ref(p), nil, all); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.MoveDurableState(ctx, lib.ID, "old", "new"); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "ann's queue", queuePaths(t, c, ann, all), "x", "new", "y")
	wantPaths(t, "bob's queue", queuePaths(t, c, bob, all), "new", "x")
	wantPaths(t, "ann's collection", itemPaths(t, c, annCol.ID, ann, all), "x", "new", "y")
	wantPaths(t, "bob's collection", itemPaths(t, c, bobCol.ID, bob, all), "new")
	if n := countRows(t, c, `SELECT (SELECT COUNT(*) FROM up_next WHERE rel_path = 'old') +
	                               (SELECT COUNT(*) FROM collection_items WHERE rel_path = 'old')`); n != 0 {
		t.Fatalf("%d rows left on the old path", n)
	}
}

// A join carries the parts' up-next entries and collection items to the joined
// book: the first part's entry keeps its place, a later part's collides with it
// and goes; an unplaced part's stays on its own path.
func TestJoinCarriesLists(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	ann := seedNamedUser(t, c, "ann")
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	const into = "Author/Book"
	cd1, cd2, cd3 := into+"/CD1", into+"/CD2", into+"/CD3"
	ref := func(p string) Ref { return Ref{LibraryID: lib.ID, Path: p} }
	for _, p := range []string{"a", cd2, "b", cd1, cd3} {
		if err := c.AddToQueue(ctx, ann, ref(p), nil, all); err != nil {
			t.Fatal(err)
		}
	}
	col, _ := c.CreateCollection(ctx, ann, "Discs", "")
	for _, p := range []string{cd1, "a", cd2} {
		if err := c.AddCollectionItem(ctx, col.ID, ann, ref(p), nil, all); err != nil {
			t.Fatal(err)
		}
	}
	parts := []JoinPart{{Path: cd1}, {Path: cd2, Offset: 100}, {Path: cd3, Unplaced: true, Last: true}}
	if err := c.JoinDurableState(ctx, lib.ID, into, parts, 0); err != nil {
		t.Fatal(err)
	}
	wantPaths(t, "queue", queuePaths(t, c, ann, all), "a", "b", into, cd3)
	wantPaths(t, "collection", itemPaths(t, c, col.ID, ann, all), into, "a")
}

// Deleting a user purges their queue, their collections (items and shares with
// them) and their share rows on other users' collections; deleting a library
// purges its queue entries and collection items (the collections stay).
func TestListsPurgedWithUserAndLibrary(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	keep, _ := c.CreateLibrary(ctx, Library{Name: "K", Root: "/tmp/k"})
	ann, bob := seedNamedUser(t, c, "ann"), seedNamedUser(t, c, "bob")
	ref := func(l *Library, p string) Ref { return Ref{LibraryID: l.ID, Path: p} }
	both := []Scope{{LibraryID: lib.ID, AllowAll: true}, {LibraryID: keep.ID, AllowAll: true}}
	for _, u := range []int64{ann, bob} {
		if err := c.AddToQueue(ctx, u, ref(lib, "a"), nil, both); err != nil {
			t.Fatal(err)
		}
		if err := c.AddToQueue(ctx, u, ref(keep, "k"), nil, both); err != nil {
			t.Fatal(err)
		}
	}
	annCol, _ := c.CreateCollection(ctx, ann, "Ann's", "")
	bobCol, _ := c.CreateCollection(ctx, bob, "Bob's", "")
	for _, col := range []*Collection{annCol, bobCol} {
		owner := col.Owner.ID
		other := ann
		if owner == ann {
			other = bob
		}
		if err := c.AddCollectionItem(ctx, col.ID, owner, ref(lib, "a"), nil, both); err != nil {
			t.Fatal(err)
		}
		if err := c.AddCollectionItem(ctx, col.ID, owner, ref(keep, "k"), nil, both); err != nil {
			t.Fatal(err)
		}
		if err := c.SetCollectionShares(ctx, col.ID, owner, []int64{other}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := c.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, ann); err != nil { // as auth.DeleteUser
		t.Fatal(err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM up_next WHERE user_id = ?`,
		`SELECT COUNT(*) FROM collections WHERE user_id = ?`,
		`SELECT COUNT(*) FROM collection_shares WHERE user_id = ?`,
		// Ann's collection's items and shares went with it.
		`SELECT COUNT(*) FROM collection_items WHERE collection_id = ?`,
		`SELECT COUNT(*) FROM collection_shares WHERE collection_id = ?`,
	} {
		arg := ann
		if strings.Contains(q, "collection_id") {
			arg = annCol.ID
		}
		if n := countRows(t, c, q, arg); n != 0 {
			t.Errorf("after user delete, %s = %d, want 0", q, n)
		}
	}
	if n := countRows(t, c, `SELECT COUNT(*) FROM up_next WHERE user_id = ?`, bob); n != 2 {
		t.Fatalf("bob's queue = %d rows, want 2", n)
	}

	if err := c.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, c, `SELECT (SELECT COUNT(*) FROM up_next WHERE library_id = ?1) +
	                               (SELECT COUNT(*) FROM collection_items WHERE library_id = ?1)`, lib.ID); n != 0 {
		t.Fatalf("%d rows of the deleted library left", n)
	}
	wantPaths(t, "bob's queue", queuePaths(t, c, bob, []Scope{{LibraryID: keep.ID, AllowAll: true}}), "k")
	wantPaths(t, "bob's collection", itemPaths(t, c, bobCol.ID, bob, []Scope{{LibraryID: keep.ID, AllowAll: true}}), "k")
}
