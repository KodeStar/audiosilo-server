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
	// The first read fills the cache and the next two are answered from it; each
	// caller changes the library it was given, and none sees another's change.
	for i := range 3 {
		if i > 0 {
			gen, _ := c.Generation()
			if _, ok := c.libs.Get(gen, lib.ID); !ok {
				t.Fatalf("read %d: the library isn't cached", i)
			}
		}
		l := get()
		if l.Name != "Books" || !slices.Equal(l.IgnorePatterns, []string{"*.tmp"}) {
			t.Fatalf("read %d: a caller's change reached the cache: %+v", i, l)
		}
		l.Name, l.IgnorePatterns[0] = "Changed", "changed"
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
