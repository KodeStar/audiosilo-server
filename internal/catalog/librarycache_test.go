package catalog

import (
	"errors"
	"slices"
	"testing"
)

// GetLibrary answers from its cache only while nothing is written: an edit
// through the catalog, a write of any table, a deletion each show at once, and
// a caller changing the library it was given changes nothing kept.
func TestGetLibraryCachedUntilAWrite(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: "/books", IgnorePatterns: []string{"*.tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	get := func() *Library {
		t.Helper()
		l, err := c.GetLibrary(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	first := get()
	first.Name, first.IgnorePatterns[0] = "Changed", "changed"
	if again := get(); again.Name != "Books" || !slices.Equal(again.IgnorePatterns, []string{"*.tmp"}) {
		t.Fatalf("a caller's change reached the cache: %+v", again)
	}

	if _, _, err := c.UpdateLibrary(ctx, lib.ID, LibraryPatch{Name: "Renamed", Root: "/books", DefaultView: lib.DefaultView}); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Name != "Renamed" {
		t.Fatalf("after an edit: %q", got.Name)
	}
	// A write the catalog doesn't make itself (any statement on the handle).
	if _, err := c.db.ExecContext(ctx, `UPDATE libraries SET root = '/elsewhere' WHERE id = ?`, lib.ID); err != nil {
		t.Fatal(err)
	}
	if got := get(); got.Root != "/elsewhere" {
		t.Fatalf("after a direct write: %q", got.Root)
	}
	if err := c.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetLibrary(ctx, lib.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after a delete: %v", err)
	}
}
