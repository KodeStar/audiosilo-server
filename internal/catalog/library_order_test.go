package catalog

import "testing"

// ADMIN-12: a new library lands last in the display order, after libraries
// never reordered (all 0) and after a reordered set alike, not in its name's
// place.
func TestCreateLibraryLandsLast(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	names := func() []string {
		t.Helper()
		libs, err := c.ListLibraries(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(libs))
		for i, l := range libs {
			out[i] = l.Name
		}
		return out
	}
	create := func(name string) *Library {
		t.Helper()
		lib, err := c.CreateLibrary(ctx, Library{Name: name, Root: "/tmp/" + name})
		if err != nil {
			t.Fatal(err)
		}
		return lib
	}
	m := create("M")
	if m.SortOrder != 0 {
		t.Fatalf("first library sort_order = %d, want 0", m.SortOrder)
	}
	z := create("Z")
	a := create("A")
	if got := names(); len(got) != 3 || got[0] != "M" || got[1] != "Z" || got[2] != "A" {
		t.Fatalf("order = %v, want [M Z A]", got)
	}
	if err := c.ReorderLibraries(ctx, []int64{a.ID, z.ID, m.ID}); err != nil {
		t.Fatal(err)
	}
	b := create("B")
	if b.SortOrder != 3 {
		t.Fatalf("after a reorder: sort_order = %d, want 3", b.SortOrder)
	}
	if got := names(); len(got) != 4 || got[0] != "A" || got[3] != "B" {
		t.Fatalf("order = %v, want A first and B last", got)
	}
}
