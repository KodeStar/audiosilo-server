package catalog

import (
	"context"
	"testing"
)

func TestScopeAllows(t *testing.T) {
	s := Scope{Paths: []string{"A. F. Kay/Divine Apostasy", "Other/One Book.m4b"}}
	cases := []struct {
		path string
		want bool
	}{
		{"A. F. Kay/Divine Apostasy", true},                    // the rule itself
		{"A. F. Kay/Divine Apostasy/AF01 - Shade/x.m4b", true}, // under the subtree
		{"Other/One Book.m4b", true},                           // the single item
		{"A. F. Kay/Other Series", false},                      // sibling, not granted
		{"A. F. Kay", false},                                   // ancestor is not access
		{"A. F. Kay/Divine Apostasy Extra", false},             // prefix but not segment boundary
	}
	for _, c := range cases {
		if got := s.Allows(c.path); got != c.want {
			t.Errorf("Allows(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if !(Scope{AllowAll: true}).Allows("anything/at/all") {
		t.Error("AllowAll should allow everything")
	}
}

func TestScopeVisibleInBrowse(t *testing.T) {
	s := Scope{Paths: []string{"A. F. Kay/Divine Apostasy"}}
	// Ancestors of a rule are navigable; the granted subtree is visible; siblings
	// are hidden.
	for path, want := range map[string]bool{
		"A. F. Kay":                      true,  // ancestor → navigable
		"A. F. Kay/Divine Apostasy":      true,  // granted
		"A. F. Kay/Divine Apostasy/AF01": true,  // under grant
		"Brandon Sanderson":              false, // unrelated
		"A. F. Kay/Other Series":         false, // sibling of grant
	} {
		if got := s.VisibleInBrowse(path); got != want {
			t.Errorf("VisibleInBrowse(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestSharesAndUserScope(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	uid := seedUser(t, c, ctx)

	// No grants → empty scope (no access); admin → AllowAll.
	sc, _ := c.UserScope(ctx, uid, lib.ID, false)
	if sc.AllowAll || len(sc.Paths) != 0 {
		t.Fatalf("expected empty scope before grant, got %+v", sc)
	}
	if adm, _ := c.UserScope(ctx, 999, lib.ID, true); !adm.AllowAll {
		t.Fatal("admin scope should be AllowAll")
	}

	// Create a share granting one subtree, grant it to the user.
	share, err := c.CreateShare(ctx, Share{Name: "Kids"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddSharePath(ctx, share.ID, PathRule{LibraryID: lib.ID, Path: "Kids"}); err != nil {
		t.Fatal(err)
	}
	if err := c.GrantShare(ctx, uid, share.ID); err != nil {
		t.Fatal(err)
	}

	sc, _ = c.UserScope(ctx, uid, lib.ID, false)
	if sc.AllowAll || len(sc.Paths) != 1 || sc.Paths[0] != "Kids" {
		t.Fatalf("expected scope with one 'Kids' rule, got %+v", sc)
	}
	if !sc.Allows("Kids/Dr Seuss/Cat.m4b") || sc.Allows("Adult/Book.m4b") {
		t.Fatal("scope should allow only the Kids subtree")
	}

	// GrantWholeLibrary sugar yields AllowAll.
	uid2 := seedUserNamed(t, c, ctx, "u2")
	if err := c.GrantWholeLibrary(ctx, uid2, lib.ID); err != nil {
		t.Fatal(err)
	}
	if sc2, _ := c.UserScope(ctx, uid2, lib.ID, false); !sc2.AllowAll {
		t.Fatalf("whole-library grant should be AllowAll, got %+v", sc2)
	}

	// Accessible libraries reflect grants.
	libs, _ := c.AccessibleLibraries(ctx, uid, false)
	if len(libs) != 1 || libs[0].ID != lib.ID {
		t.Fatalf("expected one accessible library, got %+v", libs)
	}
}

// TestGrantWholeLibraryHealsRulelessShare guards the case observed in production:
// a "Library: <name>" share exists but has lost its "" path rule (however it
// arose - a server/schema update, a manual edit, an FK cascade). Such a share
// grants nothing, so re-granting whole-library access must re-add the rule rather
// than just hand out the rule-less share.
func TestGrantWholeLibraryHealsRulelessShare(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "audiobooks", Root: "/tmp"})
	uid := seedUser(t, c, ctx)
	if err := c.GrantWholeLibrary(ctx, uid, lib.ID); err != nil {
		t.Fatal(err)
	}

	// Drive the share into the broken state: it exists, but with no path rule.
	share, err := c.shareByName(ctx, "Library: audiobooks")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveSharePath(ctx, share.ID, PathRule{LibraryID: lib.ID, Path: ""}); err != nil {
		t.Fatal(err)
	}
	if sc, _ := c.UserScope(ctx, uid, lib.ID, false); sc.AllowAll || len(sc.Paths) != 0 {
		t.Fatalf("precondition: rule-less share should grant nothing, got %+v", sc)
	}

	// Re-granting whole-library access must heal the share.
	if err := c.GrantWholeLibrary(ctx, uid, lib.ID); err != nil {
		t.Fatal(err)
	}
	if sc, _ := c.UserScope(ctx, uid, lib.ID, false); !sc.AllowAll {
		t.Fatalf("re-grant should yield AllowAll, got %+v", sc)
	}
	libs, _ := c.AccessibleLibraries(ctx, uid, false)
	if len(libs) != 1 || libs[0].ID != lib.ID {
		t.Fatalf("expected the library to be accessible after re-grant, got %+v", libs)
	}
}

func TestScopedListBooks(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Kids/A/Cat.m4b", Title: "Cat", Author: "Seuss"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Adult/B/Dune.m4b", Title: "Dune", Author: "Herbert"})

	scope := Scope{LibraryID: lib.ID, Paths: []string{"Kids"}}
	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Scope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 1 || page.Books[0].Title != "Cat" {
		t.Fatalf("scoped list should return only Kids books, got %+v", page.Books)
	}
}

// TestScopedListBooksLikeWildcards is the denied regression for the LIKE-escape
// fix in pathFilterSQL: a share path containing a SQL LIKE metacharacter ('_')
// must NOT over-match a sibling folder. Granting "Sci_Fi" must not leak books
// under "SciXFi" (which an unescaped `LIKE 'Sci_Fi/%'` would match, since '_'
// is a single-char wildcard), keeping the SQL list filter consistent with the
// authoritative Go gate Scope.Allows.
func TestScopedListBooksLikeWildcards(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Sci_Fi/A/Dune.m4b", Title: "Dune", Author: "Herbert"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "SciXFi/B/Leak.m4b", Title: "Leak", Author: "Nobody"})

	scope := Scope{LibraryID: lib.ID, Paths: []string{"Sci_Fi"}}

	// Allowed: the granted subtree is returned.
	if !scope.Allows("Sci_Fi/A/Dune.m4b") {
		t.Fatal("scope should allow the granted Sci_Fi subtree")
	}
	// Denied: the sibling differing only at the '_' position is not granted.
	if scope.Allows("SciXFi/B/Leak.m4b") {
		t.Fatal("scope must not allow the wildcard-matching sibling SciXFi")
	}

	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Scope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 1 || page.Books[0].Title != "Dune" {
		t.Fatalf("scoped list must exclude the SciXFi sibling, got %+v", page.Books)
	}

	hits, err := c.Search(ctx, "Leak", []Scope{scope}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("scoped search must not leak the SciXFi sibling, got %+v", hits)
	}

	recent, err := c.RecentBooks(ctx, []Scope{scope}, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range recent {
		if b.RelPath == "SciXFi/B/Leak.m4b" {
			t.Fatalf("scoped recent must not leak the SciXFi sibling, got %+v", recent)
		}
	}
}

func seedUserNamed(t *testing.T, c *Catalog, ctx context.Context, name string) int64 {
	t.Helper()
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at)
		 VALUES(?, 'x','user','t','t')`, name)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}
