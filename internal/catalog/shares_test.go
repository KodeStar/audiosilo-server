package catalog

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// TestCleanRelPath pins the canonicalization every user-supplied rel path goes
// through before a scope check or a persisted path key. The denied cases are the
// security-critical ones: ".." must clamp at the root, never escape it, and must
// collapse before the literal prefix match in relpath.Within sees the path.
func TestCleanRelPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		// Canonical inputs pass through untouched.
		{"", ""},
		{"Author/Book.m4b", "Author/Book.m4b"},
		// Cosmetic variants normalize to the same key.
		{"/", ""},
		{".", ""},
		{"/Author/Book.m4b/", "Author/Book.m4b"},
		{"Author//Book.m4b", "Author/Book.m4b"},
		{"./Author/./Book.m4b", "Author/Book.m4b"},
		// Traversal clamps at the root instead of escaping it.
		{"../../../etc/passwd", "etc/passwd"},
		{"AuthorA/../AuthorB/x.m4b", "AuthorB/x.m4b"},
		{"Author/..", ""},
	}
	for _, c := range cases {
		if got := CleanRelPath(c.in); got != c.want {
			t.Errorf("CleanRelPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestScopeAllows(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestGrantWholeLibraryRegrantsPastAHeldRulelessShare guards the case observed in
// production: a "Library: <name>" share has lost its "" path rule and grants
// nothing, so re-granting must give the library again. Someone holds it, so it
// isn't healed (that would widen every holder, server#223): the grant gets a
// share of its own.
func TestGrantWholeLibraryRegrantsPastAHeldRulelessShare(t *testing.T) {
	t.Parallel()
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

	// Re-granting whole-library access gives the library again.
	if err := c.GrantWholeLibrary(ctx, uid, lib.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.GetShare(ctx, share.ID); got == nil || len(got.Paths) != 0 {
		t.Fatalf("the held rule-less share was healed: %+v", got)
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
	t.Parallel()
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
	t.Parallel()
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

func TestShareMembers(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	kids, _ := c.CreateShare(ctx, Share{Name: "Kids"})
	empty, _ := c.CreateShare(ctx, Share{Name: "Nobody"})
	sam := seedUserNamed(t, c, ctx, "sam")
	maya := seedUserNamed(t, c, ctx, "maya")
	for _, uid := range []int64{maya, sam} {
		if err := c.GrantShare(ctx, uid, kids.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.ShareMembers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m := got[kids.ID]; len(m) != 2 || m[0] != sam || m[1] != maya {
		t.Errorf("Kids members = %v, want [%d %d]", m, sam, maya)
	}
	if _, ok := got[empty.ID]; ok {
		t.Errorf("a share granted to nobody has members: %v", got[empty.ID])
	}
}

// Deleting a library drops its whole-library grant shares (they grant nothing
// once it's gone) but leaves the admin's own shares alone.
func TestDeleteLibraryDropsItsGrantShares(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp"})
	uid := seedUser(t, c, ctx)
	if err := c.GrantWholeLibrary(ctx, uid, lib.ID); err != nil {
		t.Fatal(err)
	}
	own, _ := c.CreateShare(ctx, Share{Name: "Favourites", Paths: []PathRule{{LibraryID: lib.ID, Path: ""}}})
	shares, _ := c.ListShares(ctx)
	var marked bool
	for _, s := range shares {
		if s.WholeLibraryID != nil && *s.WholeLibraryID == lib.ID && s.ID != own.ID {
			marked = true
		}
		if s.ID == own.ID && s.WholeLibraryID != nil {
			t.Error("an admin's own share is marked as a library grant")
		}
	}
	if !marked {
		t.Fatal("the grant share isn't marked with its library")
	}
	if err := c.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	shares, _ = c.ListShares(ctx)
	if len(shares) != 1 || shares[0].ID != own.ID {
		t.Fatalf("after deleting the library: %+v, want only Favourites", shares)
	}
}

// A library renamed and its old name reused by a new library: granting the new
// one must not reuse the first library's grant share (that would hand the new
// library to everyone who has the old one), and re-granting a renamed library
// reuses its own marked share rather than minting a duplicate.
func TestGrantWholeLibraryFollowsTheMarkNotTheName(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	fiction, _ := c.CreateLibrary(ctx, Library{Name: "Fiction", Root: "/tmp/a"})
	maya := seedUserNamed(t, c, ctx, "maya")
	sam := seedUserNamed(t, c, ctx, "sam")
	if err := c.GrantWholeLibrary(ctx, maya, fiction.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.UpdateLibrary(ctx, fiction.ID, LibraryPatch{Name: "Novels", Root: "/tmp/a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.GrantWholeLibrary(ctx, sam, fiction.ID); err != nil {
		t.Fatal(err)
	}
	shares, _ := c.ListShares(ctx)
	if len(shares) != 1 {
		t.Fatalf("re-granting a renamed library made %d shares, want 1: %+v", len(shares), shares)
	}

	kids, _ := c.CreateLibrary(ctx, Library{Name: "Fiction", Root: "/tmp/b"})
	if err := c.GrantWholeLibrary(ctx, sam, kids.ID); err != nil {
		t.Fatalf("granting a library that took a renamed library's old name: %v", err)
	}
	if sc, _ := c.UserScope(ctx, sam, kids.ID, false); !sc.AllowAll {
		t.Fatalf("sam didn't get the new library: %+v", sc)
	}
	if sc, _ := c.UserScope(ctx, maya, kids.ID, false); sc.AllowAll || len(sc.Paths) != 0 {
		t.Fatalf("maya reached the new library through the old grant share: %+v", sc)
	}
}

// Deleting a library keeps a grant share that also holds another library's
// rules: its members still have that library.
func TestDeleteLibraryKeepsAGrantShareWithOtherRules(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	main, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp/a"})
	kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/b"})
	uid := seedUser(t, c, ctx)
	if err := c.GrantWholeLibrary(ctx, uid, kids.ID); err != nil {
		t.Fatal(err)
	}
	shares, _ := c.ListShares(ctx)
	if err := c.AddSharePath(ctx, shares[0].ID, PathRule{LibraryID: main.ID, Path: "Gruffalo"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteLibrary(ctx, kids.ID); err != nil {
		t.Fatal(err)
	}
	if sc, _ := c.UserScope(ctx, uid, main.ID, false); len(sc.Paths) != 1 {
		t.Fatalf("deleting Kids took away the Main folder too: %+v", sc)
	}
}

// An admin's own share that happens to be named "Library: <name>" is never
// adopted as that library's grant (server#223): it keeps its rules, its holders
// and its unmarked state, and the grant gets its own "Library: <name> (<id>)".
func TestGrantWholeLibraryNeverAdoptsAnAdminShare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		description string
		rules       func(kids, other int64) []PathRule
	}{
		{"a sub-path rule", "", func(kids, _ int64) []PathRule {
			return []PathRule{{LibraryID: kids, Path: "Kids/Picture books"}}
		}},
		{"a sub-path rule, grant's description", wholeLibraryShareDescription, func(kids, _ int64) []PathRule {
			return []PathRule{{LibraryID: kids, Path: "Kids/Picture books"}}
		}},
		{"whole plus a sub-path", wholeLibraryShareDescription, func(kids, _ int64) []PathRule {
			return []PathRule{{LibraryID: kids, Path: ""}, {LibraryID: kids, Path: "Kids/Picture books"}}
		}},
		{"another library's rule", wholeLibraryShareDescription, func(_, other int64) []PathRule {
			return []PathRule{{LibraryID: other, Path: ""}}
		}},
		{"its own description, whole rule", "Everything for the kids", func(kids, _ int64) []PathRule {
			return []PathRule{{LibraryID: kids, Path: ""}}
		}},
		{"its own description, no rule", "", func(int64, int64) []PathRule { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/kids"})
			other, _ := c.CreateLibrary(ctx, Library{Name: "Other", Root: "/tmp/other"})
			child := seedUserNamed(t, c, ctx, "child")
			guest := seedUserNamed(t, c, ctx, "guest")
			rules := tc.rules(kids.ID, other.ID)
			mine, err := c.CreateShare(ctx, Share{Name: "Library: Kids", Description: tc.description, Paths: rules})
			if err != nil {
				t.Fatal(err)
			}
			if err := c.GrantShare(ctx, child, mine.ID); err != nil {
				t.Fatal(err)
			}
			before, _ := c.UserScope(ctx, child, kids.ID, false)

			if err := c.GrantWholeLibrary(ctx, guest, kids.ID); err != nil {
				t.Fatal(err)
			}

			// Denied: the admin's share is untouched, so the child's reach is too.
			got, err := c.GetShare(ctx, mine.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.WholeLibraryID != nil || len(got.Paths) != len(rules) {
				t.Fatalf("admin's share after the grant = %+v; want its own rules, unmarked", got)
			}
			for _, r := range rules {
				if !slices.Contains(got.Paths, r) {
					t.Fatalf("admin's share rules = %+v; want %+v", got.Paths, rules)
				}
			}
			after, _ := c.UserScope(ctx, child, kids.ID, false)
			if after.AllowAll != before.AllowAll || len(after.Paths) != len(before.Paths) {
				t.Fatalf("the child's Kids scope changed: %+v -> %+v", before, after)
			}
			if members, _ := c.ShareMembers(ctx); len(members[mine.ID]) != 1 || members[mine.ID][0] != child {
				t.Fatalf("admin's share holders = %v; want only the child", members[mine.ID])
			}

			// Allowed: the guest gets the whole library through a new, marked grant.
			if sc, _ := c.UserScope(ctx, guest, kids.ID, false); !sc.AllowAll {
				t.Fatalf("guest's Kids scope = %+v; want the whole library", sc)
			}
			grant, err := c.wholeLibraryShare(ctx, kids.ID, "Library: Kids")
			if err != nil {
				t.Fatal(err)
			}
			if grant.ID == mine.ID || grant.Name != fmt.Sprintf("Library: Kids (%d)", kids.ID) ||
				grant.WholeLibraryID == nil || *grant.WholeLibraryID != kids.ID {
				t.Fatalf("the grant = %+v", grant)
			}
			// A second grant reuses it by its mark.
			if err := c.GrantWholeLibrary(ctx, child, kids.ID); err != nil {
				t.Fatal(err)
			}
			if shares, _ := c.ListShares(ctx); len(shares) != 2 {
				t.Fatalf("shares after a second grant = %+v; want the admin's and one grant", shares)
			}
		})
	}
}

// An unmarked share that can only be an old grant (made before
// whole_library_id): the grant's description and only the library's "" rule
// (held or not), or no rule at all while nobody holds it (one an interrupted
// grant left) - is still adopted, marked and healed, keeping its holders.
func TestGrantWholeLibraryAdoptsAnOldGrant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		whole bool
	}{{"whole rule, held", true}, {"no rule, unheld", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/kids"})
			old := seedUserNamed(t, c, ctx, "old")
			guest := seedUserNamed(t, c, ctx, "guest")
			s := Share{Name: "Library: Kids", Description: wholeLibraryShareDescription}
			if tc.whole {
				s.Paths = []PathRule{{LibraryID: kids.ID, Path: ""}}
			}
			share, err := c.CreateShare(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			holders := []int64{guest}
			if tc.whole {
				if err := c.GrantShare(ctx, old, share.ID); err != nil {
					t.Fatal(err)
				}
				holders = append(holders, old)
			}
			if err := c.GrantWholeLibrary(ctx, guest, kids.ID); err != nil {
				t.Fatal(err)
			}
			shares, _ := c.ListShares(ctx)
			if len(shares) != 1 || shares[0].ID != share.ID || shares[0].WholeLibraryID == nil || *shares[0].WholeLibraryID != kids.ID ||
				len(shares[0].Paths) != 1 || shares[0].Paths[0] != (PathRule{LibraryID: kids.ID, Path: ""}) {
				t.Fatalf("shares = %+v; want the old grant adopted and marked", shares)
			}
			for _, u := range holders {
				if sc, _ := c.UserScope(ctx, u, kids.ID, false); !sc.AllowAll {
					t.Fatalf("user %d's Kids scope = %+v; want the whole library", u, sc)
				}
			}
		})
	}
}

// A rule-less share someone holds is never adopted, marked or not: adding the
// whole-library rule to it would hand the library to its holders (an admin's
// empty "Library: X" share with the grant's description, or a grant whose rule
// is gone). It stays as it is and the new holder gets a grant of its own.
func TestGrantWholeLibraryNeverAdoptsAHeldRulelessShare(t *testing.T) {
	t.Parallel()
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprintf("marked=%v", marked), func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/kids"})
			held := seedUserNamed(t, c, ctx, "held")
			guest := seedUserNamed(t, c, ctx, "guest")
			// Marked: a real grant whose rule is gone. Unmarked: an admin's empty
			// share with the grant's name and description.
			var empty *Share
			if marked {
				if err := c.GrantWholeLibrary(ctx, held, kids.ID); err != nil {
					t.Fatal(err)
				}
				s, err := c.shareByName(ctx, "Library: Kids")
				if err != nil {
					t.Fatal(err)
				}
				if err := c.RemoveSharePath(ctx, s.ID, PathRule{LibraryID: kids.ID, Path: ""}); err != nil {
					t.Fatal(err)
				}
				empty = s
			} else {
				s, err := c.CreateShare(ctx, Share{Name: "Library: Kids", Description: wholeLibraryShareDescription})
				if err != nil {
					t.Fatal(err)
				}
				if err := c.GrantShare(ctx, held, s.ID); err != nil {
					t.Fatal(err)
				}
				empty = s
			}

			if err := c.GrantWholeLibrary(ctx, guest, kids.ID); err != nil {
				t.Fatal(err)
			}

			// Denied: the held share stays rule-less (and unmarked, if it was), so
			// its holder still reaches nothing in Kids.
			got, err := c.GetShare(ctx, empty.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Paths) != 0 || (got.WholeLibraryID != nil) != marked {
				t.Fatalf("held rule-less share after the grant = %+v", got)
			}
			if sc, _ := c.UserScope(ctx, held, kids.ID, false); sc.AllowAll || len(sc.Paths) != 0 {
				t.Fatalf("the holder's Kids scope = %+v; want nothing", sc)
			}
			if members, _ := c.ShareMembers(ctx); len(members[empty.ID]) != 1 || members[empty.ID][0] != held {
				t.Fatalf("held share's holders = %v; want only the holder", members[empty.ID])
			}
			// Allowed: the guest gets the whole library through a new, marked grant.
			if sc, _ := c.UserScope(ctx, guest, kids.ID, false); !sc.AllowAll {
				t.Fatalf("guest's Kids scope = %+v; want the whole library", sc)
			}
			grant, err := c.wholeLibraryShare(ctx, kids.ID, "Library: Kids")
			if err != nil {
				t.Fatal(err)
			}
			if grant.ID == empty.ID || grant.Name != wholeLibraryIDName("Library: Kids", kids.ID) ||
				grant.WholeLibraryID == nil || *grant.WholeLibraryID != kids.ID ||
				len(grant.Paths) != 1 || grant.Paths[0] != (PathRule{LibraryID: kids.ID, Path: ""}) {
				t.Fatalf("the grant = %+v", grant)
			}
		})
	}
}

// A library's grant share an admin has changed since - narrowed to a folder, or
// given another library's folder too - is never reused for a new grant: adding
// the whole-library rule back would widen its holders again, and handing it out
// would give the new holder that other folder. The new holder gets a grant of
// its own; the changed share keeps its rules, and its holder their reach.
func TestGrantWholeLibraryNeverWidensAChangedGrant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(kids, other int64) (remove, add []PathRule)
	}{
		{"narrowed to a folder", func(kids, _ int64) ([]PathRule, []PathRule) {
			return []PathRule{{LibraryID: kids, Path: ""}}, []PathRule{{LibraryID: kids, Path: "Kids/Picture books"}}
		}},
		{"another library's folder too", func(_, other int64) ([]PathRule, []PathRule) {
			return nil, []PathRule{{LibraryID: other, Path: "Secret"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/kids"})
			other, _ := c.CreateLibrary(ctx, Library{Name: "Other", Root: "/tmp/other"})
			old := seedUserNamed(t, c, ctx, "old")
			guest := seedUserNamed(t, c, ctx, "guest")
			if err := c.GrantWholeLibrary(ctx, old, kids.ID); err != nil {
				t.Fatal(err)
			}
			shares, _ := c.ListShares(ctx)
			if len(shares) != 1 || shares[0].WholeLibraryID == nil {
				t.Fatalf("precondition: shares = %+v; want one marked grant", shares)
			}
			changed := shares[0].ID
			remove, add := tc.change(kids.ID, other.ID)
			for _, r := range remove {
				if err := c.RemoveSharePath(ctx, changed, r); err != nil {
					t.Fatal(err)
				}
			}
			for _, r := range add {
				if err := c.AddSharePath(ctx, changed, r); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := c.GetShare(ctx, changed)
			reach := func(user int64) []Scope {
				out := make([]Scope, 0, 2)
				for _, lib := range []int64{kids.ID, other.ID} {
					sc, err := c.UserScope(ctx, user, lib, false)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, sc)
				}
				return out
			}
			oldReach := reach(old)

			if err := c.GrantWholeLibrary(ctx, guest, kids.ID); err != nil {
				t.Fatal(err)
			}

			// Denied: the changed share keeps its rules, and the old holder their reach.
			if after, _ := c.GetShare(ctx, changed); !slices.Equal(after.Paths, before.Paths) {
				t.Fatalf("the changed share's rules = %+v; want %+v", after.Paths, before.Paths)
			}
			if got := reach(old); !slices.EqualFunc(got, oldReach, func(a, b Scope) bool {
				return a.AllowAll == b.AllowAll && slices.Equal(a.Paths, b.Paths)
			}) {
				t.Fatalf("the old holder's reach = %+v; want %+v", got, oldReach)
			}
			// Denied: the guest gets nothing of the other library.
			if sc := reach(guest)[1]; sc.AllowAll || len(sc.Paths) != 0 {
				t.Fatalf("guest's Other scope = %+v; want none", sc)
			}
			// Allowed: the guest gets the whole of Kids, through a grant of its own.
			if sc := reach(guest)[0]; !sc.AllowAll {
				t.Fatalf("guest's Kids scope = %+v; want the whole library", sc)
			}
			held, _ := c.UserShares(ctx, guest)
			if len(held) != 1 || held[0].ID == changed || held[0].WholeLibraryID == nil || *held[0].WholeLibraryID != kids.ID {
				t.Fatalf("guest's shares = %+v; want one marked grant of its own", held)
			}
		})
	}
}

// A grant an interrupted GrantWholeLibrary left under the "(<id>)" name (created,
// not yet marked, with or without its rule) next to an admin's own "Library:
// <name>" is found again and finished, rather than every later grant failing on
// the taken name.
func TestGrantWholeLibraryFinishesAnInterruptedGrant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		whole bool
	}{{"no rule yet", false}, {"rule, not marked", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, ctx := newTestCatalog(t)
			kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/kids"})
			guest := seedUserNamed(t, c, ctx, "guest")
			mine, err := c.CreateShare(ctx, Share{Name: "Library: Kids",
				Paths: []PathRule{{LibraryID: kids.ID, Path: "Kids/Picture books"}}})
			if err != nil {
				t.Fatal(err)
			}
			left := Share{Name: fmt.Sprintf("Library: Kids (%d)", kids.ID), Description: wholeLibraryShareDescription}
			if tc.whole {
				left.Paths = []PathRule{{LibraryID: kids.ID, Path: ""}}
			}
			orphan, err := c.CreateShare(ctx, left)
			if err != nil {
				t.Fatal(err)
			}

			if err := c.GrantWholeLibrary(ctx, guest, kids.ID); err != nil {
				t.Fatalf("granting next to an interrupted grant: %v", err)
			}
			got, err := c.GetShare(ctx, orphan.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.WholeLibraryID == nil || *got.WholeLibraryID != kids.ID ||
				!slices.Equal(got.Paths, []PathRule{{LibraryID: kids.ID, Path: ""}}) {
				t.Fatalf("the interrupted grant = %+v; want it marked with the whole-library rule", got)
			}
			if held, _ := c.UserShares(ctx, guest); len(held) != 1 || held[0].ID != orphan.ID {
				t.Fatalf("guest's shares = %+v; want the finished grant", held)
			}
			if mineNow, _ := c.GetShare(ctx, mine.ID); mineNow.WholeLibraryID != nil || len(mineNow.Paths) != 1 {
				t.Fatalf("the admin's share = %+v; want it untouched", mineNow)
			}
			if shares, _ := c.ListShares(ctx); len(shares) != 2 {
				t.Fatalf("shares = %+v; want the admin's and the one grant", shares)
			}
		})
	}
}

// An admin's own share under the "(<id>)" name is no more adopted than one under
// "Library: <name>": with both names taken by shares of their own, the grant is
// refused (ErrNameTaken, a 409) and both are left as they were.
func TestGrantWholeLibraryNeverAdoptsAnAdminShareByTheIDName(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	kids, _ := c.CreateLibrary(ctx, Library{Name: "Kids", Root: "/tmp/kids"})
	guest := seedUserNamed(t, c, ctx, "guest")
	var mine []*Share
	for _, s := range []Share{
		{Name: "Library: Kids", Paths: []PathRule{{LibraryID: kids.ID, Path: "Kids/Picture books"}}},
		{Name: fmt.Sprintf("Library: Kids (%d)", kids.ID), Description: wholeLibraryShareDescription,
			Paths: []PathRule{{LibraryID: kids.ID, Path: "Kids/Chapter books"}}},
	} {
		share, err := c.CreateShare(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		mine = append(mine, share)
	}

	if err := c.GrantWholeLibrary(ctx, guest, kids.ID); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("grant with both names taken = %v; want ErrNameTaken", err)
	}
	for _, m := range mine {
		got, err := c.GetShare(ctx, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.WholeLibraryID != nil || !slices.Equal(got.Paths, m.Paths) {
			t.Fatalf("admin's share %q = %+v; want %+v, unmarked", m.Name, got, m)
		}
	}
	if sc, _ := c.UserScope(ctx, guest, kids.ID, false); sc.AllowAll || len(sc.Paths) != 0 {
		t.Fatalf("guest's Kids scope = %+v; want none", sc)
	}
}

// Another library's grant share is never adopted, even once it has lost its rule
// (so only its mark says whose it is) and its library's old name, which the
// share still carries, has gone to a new library.
func TestGrantWholeLibraryNeverTakesAnotherLibrarysRulelessGrant(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	fiction, _ := c.CreateLibrary(ctx, Library{Name: "Fiction", Root: "/tmp/a"})
	maya := seedUserNamed(t, c, ctx, "maya")
	sam := seedUserNamed(t, c, ctx, "sam")
	if err := c.GrantWholeLibrary(ctx, maya, fiction.ID); err != nil {
		t.Fatal(err)
	}
	old, err := c.shareByName(ctx, "Library: Fiction")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveSharePath(ctx, old.ID, PathRule{LibraryID: fiction.ID, Path: ""}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.UpdateLibrary(ctx, fiction.ID, LibraryPatch{Name: "Novels", Root: "/tmp/a"}); err != nil {
		t.Fatal(err)
	}
	next, _ := c.CreateLibrary(ctx, Library{Name: "Fiction", Root: "/tmp/b"})

	if err := c.GrantWholeLibrary(ctx, sam, next.ID); err != nil {
		t.Fatal(err)
	}
	// Denied: maya reaches nothing of the new library through Fiction's old grant.
	if sc, _ := c.UserScope(ctx, maya, next.ID, false); sc.AllowAll || len(sc.Paths) != 0 {
		t.Fatalf("maya's scope in the new library = %+v; want none", sc)
	}
	if got, _ := c.GetShare(ctx, old.ID); got.WholeLibraryID == nil || *got.WholeLibraryID != fiction.ID || len(got.Paths) != 0 {
		t.Fatalf("Fiction's old grant = %+v; want it as it was", got)
	}
	// Allowed: sam gets the new library.
	if sc, _ := c.UserScope(ctx, sam, next.ID, false); !sc.AllowAll {
		t.Fatalf("sam's scope in the new library = %+v; want the whole library", sc)
	}
}
