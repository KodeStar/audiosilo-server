package catalog

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

func newTestCatalog(t *testing.T) (*Catalog, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, time.Now), ctx
}

func seedUser(t *testing.T, c *Catalog, ctx context.Context) int64 {
	t.Helper()
	res, err := c.db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at)
		 VALUES('u','x','user','t','t')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestCountBooksByLibrary(t *testing.T) {
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: "/tmp/a"})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: "/tmp/b"})
	c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "1.m4b", Title: "One"})
	c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "2.m4b", Title: "Two"})
	c.UpsertBook(ctx, &Book{LibraryID: libB.ID, RelPath: "3.m4b", Title: "Three"})

	counts, err := c.CountBooksByLibrary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[libA.ID] != 2 || counts[libB.ID] != 1 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
}

func TestListeningOverview(t *testing.T) {
	c, ctx := newTestCatalog(t)
	uid := seedUser(t, c, ctx)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "book.m4b", Title: "Mistborn", Author: "Sanderson"})
	if _, err := c.SaveProgress(ctx, uid, Progress{Ref: Ref{LibraryID: lib.ID, Path: "book.m4b"}, Position: 30, Duration: 100}); err != nil {
		t.Fatal(err)
	}

	rows, err := c.ListeningOverview(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 listening row, got %d", len(rows))
	}
	r := rows[0]
	if r.UserID != uid || r.Username != "u" || r.Title != "Mistborn" || r.Author != "Sanderson" {
		t.Fatalf("unexpected row: %+v", r)
	}
	if r.Position != 30 || r.Duration != 100 {
		t.Fatalf("unexpected progress: %+v", r)
	}
}

func TestUpsertAndGetBook(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	id, err := c.UpsertBook(ctx, &Book{
		LibraryID: lib.ID, RelPath: "a/b.m4b", Title: "Title", Author: "Auth", Series: "Ser", SeriesIndex: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Upserting the same rel_path updates rather than duplicates.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "a/b.m4b", Title: "Title2"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetBook(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Title2" {
		t.Fatalf("expected updated title, got %q", got.Title)
	}
}

func TestKeysetPaginationStable(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	for i := 0; i < 10; i++ {
		if _, err := c.UpsertBook(ctx, &Book{
			LibraryID: lib.ID, RelPath: fmt.Sprintf("b%02d.m4b", i),
			Title: fmt.Sprintf("Title %02d", i), Author: fmt.Sprintf("Author %02d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Sort: "author", Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range page.Books {
			if seen[b.RelPath] {
				t.Fatalf("duplicate across pages: %s", b.RelPath)
			}
			seen[b.RelPath] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 10 {
		t.Fatalf("expected 10 unique books across pages, got %d", len(seen))
	}
}

func TestSearchFTS(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "1.m4b", Title: "Unsouled", Author: "Will Wight", Series: "Cradle"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "2.m4b", Title: "Soulsmith", Author: "Will Wight", Series: "Cradle"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "3.m4b", Title: "The Final Empire", Author: "Brandon Sanderson"})

	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	// Prefix match on title.
	res, err := c.Search(ctx, "soul", all, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Title != "Soulsmith" {
		t.Fatalf("prefix search = %+v", res)
	}
	// Author match returns both Will Wight books.
	res, _ = c.Search(ctx, "wight", all, 10)
	if len(res) != 2 {
		t.Fatalf("author search expected 2, got %d", len(res))
	}
	// Scoping: no scopes yields nothing.
	if res, _ := c.Search(ctx, "wight", nil, 10); len(res) != 0 {
		t.Fatalf("expected no results without access, got %d", len(res))
	}
}

func TestRecentBooksCrossLibrary(t *testing.T) {
	c, ctx := newTestCatalog(t)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: "/tmp/a"})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: "/tmp/b"})

	// AddedAt drives the order (newest first), independent of insertion order.
	c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "old.m4b", Title: "Old", AddedAt: "2024-01-01T00:00:00Z"})
	c.UpsertBook(ctx, &Book{LibraryID: libB.ID, RelPath: "new.m4b", Title: "New", AddedAt: "2024-03-01T00:00:00Z"})
	c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "mid.m4b", Title: "Mid", AddedAt: "2024-02-01T00:00:00Z"})

	scopes := []Scope{{LibraryID: libA.ID, AllowAll: true}, {LibraryID: libB.ID, AllowAll: true}}
	got, err := c.RecentBooks(ctx, scopes, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"New", "Mid", "Old"} // newest added first, spanning both libraries
	if len(got) != len(want) {
		t.Fatalf("expected %d books, got %d: %+v", len(want), len(got), got)
	}
	for i, title := range want {
		if got[i].Title != title {
			t.Fatalf("position %d = %q, want %q (order %+v)", i, got[i].Title, title, got)
		}
	}

	// Scoping: a path-restricted scope only sees matching books; no scope sees none.
	scoped := []Scope{{LibraryID: libA.ID, Paths: []string{"mid.m4b"}}}
	if res, _ := c.RecentBooks(ctx, scoped, 10); len(res) != 1 || res[0].Title != "Mid" {
		t.Fatalf("path-scoped recent = %+v", res)
	}
	if res, _ := c.RecentBooks(ctx, nil, 10); len(res) != 0 {
		t.Fatalf("expected no results without access, got %d", len(res))
	}
}

func TestRecentSortUsesAddedAt(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	// Insert in an order that differs from added_at order to prove the sort key.
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "1.m4b", Title: "First", AddedAt: "2024-02-01T00:00:00Z"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "2.m4b", Title: "Second", AddedAt: "2024-03-01T00:00:00Z"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "3.m4b", Title: "Third", AddedAt: "2024-01-01T00:00:00Z"})

	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Sort: "recent", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Second", "First", "Third"}
	if len(page.Books) != len(want) {
		t.Fatalf("expected %d, got %d", len(want), len(page.Books))
	}
	for i, title := range want {
		if page.Books[i].Title != title {
			t.Fatalf("position %d = %q, want %q", i, page.Books[i].Title, title)
		}
	}
}

func TestProgressLastWriteWins(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	uid := seedUser(t, c, ctx)
	ref := Ref{LibraryID: lib.ID, Path: "Author/Book.m4b"}

	t0 := time.Now().UTC()
	if _, err := c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 100, UpdatedAt: t0.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	// A stale (older) update must be ignored.
	saved, err := c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 50, UpdatedAt: t0.Add(-time.Minute).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Position != 100 {
		t.Fatalf("stale update should be ignored, position = %v", saved.Position)
	}
	// A newer update wins.
	saved, _ = c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 200, UpdatedAt: t0.Add(time.Minute).Format(time.RFC3339)})
	if saved.Position != 200 {
		t.Fatalf("newer update should win, position = %v", saved.Position)
	}
}

func TestMoveDurableState(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	uid := seedUser(t, c, ctx)
	old := Ref{LibraryID: lib.ID, Path: "old/Book.m4b"}
	if _, err := c.SaveProgress(ctx, uid, Progress{Ref: old, Position: 42}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddFavourite(ctx, uid, old); err != nil {
		t.Fatal(err)
	}
	if err := c.SetEnrichment(ctx, lib.ID, "old/Book.m4b", "B0ASIN", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.MoveDurableState(ctx, lib.ID, "old/Book.m4b", "new/Book.m4b"); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.GetProgress(ctx, uid, old); p != nil {
		t.Fatal("progress should no longer be at the old path")
	}
	moved, _ := c.GetProgress(ctx, uid, Ref{LibraryID: lib.ID, Path: "new/Book.m4b"})
	if moved == nil || moved.Position != 42 {
		t.Fatalf("progress should have moved to the new path: %+v", moved)
	}
	// The favourite follows the move too.
	favs, _ := c.ListAllFavourites(ctx, uid, []Scope{{LibraryID: lib.ID, AllowAll: true}})
	if len(favs) != 1 || favs[0].Path != "new/Book.m4b" {
		t.Fatalf("favourite should have moved to the new path: %+v", favs)
	}
	// Path-keyed enrichment follows the move: re-indexing the book at the new path
	// (without an ASIN) and re-applying must restore the ASIN attached pre-move.
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "new/Book.m4b", Title: "Book", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyEnrichments(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := c.GetBookByPath(ctx, lib.ID, "new/Book.m4b"); b == nil || b.ASIN != "B0ASIN" {
		t.Fatalf("enrichment should have moved to the new path: %+v", b)
	}
}

func TestFavouritesCRUDAndScope(t *testing.T) {
	c, ctx := newTestCatalog(t)
	uid := seedUser(t, c, ctx)
	libA, _ := c.CreateLibrary(ctx, Library{Name: "A", Root: "/tmp/a"})
	libB, _ := c.CreateLibrary(ctx, Library{Name: "B", Root: "/tmp/b"})
	// A book folder favourite is enriched from the index; a plain navigation
	// folder favourite has no matching book row (is_book=false).
	c.UpsertBook(ctx, &Book{LibraryID: libA.ID, RelPath: "Sanderson/Mistborn", Title: "Mistborn", Author: "Sanderson", Duration: 3600})
	bookRef := Ref{LibraryID: libA.ID, Path: "Sanderson/Mistborn"}
	folderRef := Ref{LibraryID: libA.ID, Path: "Sanderson"}
	otherLib := Ref{LibraryID: libB.ID, Path: "x.m4b"}

	for _, ref := range []Ref{bookRef, folderRef, otherLib} {
		if err := c.AddFavourite(ctx, uid, ref); err != nil {
			t.Fatal(err)
		}
	}
	// Re-favouriting is idempotent (no duplicate, no error).
	if err := c.AddFavourite(ctx, uid, bookRef); err != nil {
		t.Fatal(err)
	}

	allScopes := []Scope{{LibraryID: libA.ID, AllowAll: true}, {LibraryID: libB.ID, AllowAll: true}}
	favs, err := c.ListAllFavourites(ctx, uid, allScopes)
	if err != nil {
		t.Fatal(err)
	}
	if len(favs) != 3 {
		t.Fatalf("expected 3 favourites, got %d: %+v", len(favs), favs)
	}
	byPath := map[string]Favourite{}
	for _, f := range favs {
		byPath[f.Path] = f
	}
	if b := byPath["Sanderson/Mistborn"]; !b.IsBook || b.Title != "Mistborn" || b.Duration != 3600 {
		t.Fatalf("book favourite not enriched: %+v", b)
	}
	if f := byPath["Sanderson"]; f.IsBook || f.Title != "" {
		t.Fatalf("navigation-folder favourite should not be a book: %+v", f)
	}

	// Scope filtering: a path-restricted scope only sees matching favourites; no
	// scope sees none.
	scoped := []Scope{{LibraryID: libA.ID, Paths: []string{"Sanderson/Mistborn"}}}
	if res, _ := c.ListAllFavourites(ctx, uid, scoped); len(res) != 1 || res[0].Path != "Sanderson/Mistborn" {
		t.Fatalf("path-scoped favourites = %+v", res)
	}
	if res, _ := c.ListAllFavourites(ctx, uid, nil); len(res) != 0 {
		t.Fatalf("expected no favourites without access, got %d", len(res))
	}

	// Removal is idempotent.
	if err := c.RemoveFavourite(ctx, uid, bookRef); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveFavourite(ctx, uid, bookRef); err != nil {
		t.Fatal(err)
	}
	if res, _ := c.ListAllFavourites(ctx, uid, allScopes); len(res) != 2 {
		t.Fatalf("expected 2 favourites after removal, got %d", len(res))
	}
}

func TestUpdateLibrary(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	// Patch only the root; other fields are preserved.
	updated, err := c.UpdateLibrary(ctx, lib.ID, Library{Root: "/srv/books"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Root != "/srv/books" || updated.Name != "L" {
		t.Fatalf("unexpected update result: %+v", updated)
	}
	got, _ := c.GetLibrary(ctx, lib.ID)
	if got.Root != "/srv/books" {
		t.Fatalf("root not persisted: %q", got.Root)
	}
}

func TestFolderOverridesCRUD(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})

	if err := c.SetFolderOverride(ctx, lib.ID, "Author/Series", OverrideBook); err != nil {
		t.Fatal(err)
	}
	// Invalid modes are rejected with the typed sentinel (mapped to 400 by the API).
	if err := c.SetFolderOverride(ctx, lib.ID, "Other", "weird"); !errors.Is(err, ErrInvalidOverrideMode) {
		t.Fatalf("invalid mode: err = %v, want ErrInvalidOverrideMode", err)
	}
	got, err := c.FolderOverrides(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got["Author/Series"] != OverrideBook || len(got) != 1 {
		t.Fatalf("unexpected overrides: %+v", got)
	}
	// Upsert changes the mode in place.
	if err := c.SetFolderOverride(ctx, lib.ID, "Author/Series", OverrideCollection); err != nil {
		t.Fatal(err)
	}
	got, _ = c.FolderOverrides(ctx, lib.ID)
	if got["Author/Series"] != OverrideCollection {
		t.Fatalf("override not updated in place: %+v", got)
	}
	if err := c.DeleteFolderOverride(ctx, lib.ID, "Author/Series"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.FolderOverrides(ctx, lib.ID); len(got) != 0 {
		t.Fatalf("override not deleted: %+v", got)
	}
}

// TestUniqueNameReturnsErrNameTaken pins that a duplicate library or share name
// surfaces as the typed ErrNameTaken sentinel (which the API maps to 409), not a
// raw SQLite constraint error that leaked through as an opaque 500.
func TestUniqueNameReturnsErrNameTaken(t *testing.T) {
	c, ctx := newTestCatalog(t)

	if _, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/other"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate library name: err = %v, want ErrNameTaken", err)
	}

	if _, err := c.CreateShare(ctx, Share{Name: "Sci-Fi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateShare(ctx, Share{Name: "Sci-Fi"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate share name (create): err = %v, want ErrNameTaken", err)
	}
	// Renaming a second share onto an existing name is the same collision.
	other, err := c.CreateShare(ctx, Share{Name: "Fantasy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateShare(ctx, other.ID, Share{Name: "Sci-Fi"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate share name (rename): err = %v, want ErrNameTaken", err)
	}
}

// TestCreateShareWithPathsIsAtomic verifies CreateShare inserts the share and its
// path rules in one transaction: a rule referencing a non-existent library fails
// the FK insert and rolls back the whole share, leaving nothing behind (the
// orphan the old transport-layer compensating delete tried to clean up by hand).
func TestCreateShareWithPathsIsAtomic(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "Main", Root: "/tmp"})

	// Happy path: the share and its rule both land.
	created, err := c.CreateShare(ctx, Share{Name: "Good", Paths: []PathRule{{LibraryID: lib.ID, Path: "Author"}}})
	if err != nil {
		t.Fatalf("create share with valid path: %v", err)
	}
	if rules, _ := c.ListSharePaths(ctx, created.ID); len(rules) != 1 || rules[0].Path != "Author" {
		t.Fatalf("path rule not persisted: %+v", rules)
	}

	// Rollback: a rule with a dangling library_id fails the FK insert, so the
	// share row must not survive.
	if _, err := c.CreateShare(ctx, Share{Name: "Bad", Paths: []PathRule{{LibraryID: 999999, Path: "x"}}}); err == nil {
		t.Fatal("expected a FK error for a rule referencing a non-existent library")
	}
	shares, err := c.ListShares(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shares {
		if s.Name == "Bad" {
			t.Fatalf("rolled-back share leaked: %+v", shares)
		}
	}
}

// TestPathFilterMatchesScopeAllows is the parity guard for the two encodings of
// the same path-containment predicate: the Go gate Scope.Allows and the SQL
// pathFilterSQL used by ListBooks/Search/RecentBooks. A book at path P under a
// scope must be returned by the scoped list query IFF Scope.Allows(P) — if the
// two ever diverge (e.g. a future change to segment-boundary or escape handling
// applied to only one), this fails. Covers wildcard, boundary, and exact cases.
func TestPathFilterMatchesScopeAllows(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})

	rules := []string{"Sci_Fi", "Fantasy/Sanderson", "100%Pure"}
	paths := []string{
		"Sci_Fi/A/Dune.m4b",         // under a rule with a LIKE wildcard ('_')
		"SciXFi/B/Leak.m4b",         // wildcard-matching sibling — must NOT match
		"Fantasy/Sanderson/Way.m4b", // under an exact-prefix rule
		"Fantasy/SandersonX/No.m4b", // boundary sibling — must NOT match
		"Fantasy/Hobb/Other.m4b",    // different subtree — must NOT match
		"100%Pure/C/Yes.m4b",        // under a rule with a LIKE wildcard ('%')
		"100XPure/D/No.m4b",         // '%'-matching sibling — must NOT match
	}
	for i, p := range paths {
		c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, Title: fmt.Sprintf("T%d", i), Author: "A"})
	}

	scope := Scope{LibraryID: lib.ID, Paths: rules}
	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Scope: &scope, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	inSQL := map[string]bool{}
	for _, b := range page.Books {
		inSQL[b.RelPath] = true
	}
	for _, p := range paths {
		if got, want := inSQL[p], scope.Allows(p); got != want {
			t.Fatalf("divergence at %q: ListBooks returned=%v, Scope.Allows=%v", p, got, want)
		}
	}
}

func TestDeleteLibraryRemovesBooksAndFTS(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "1.m4b", Title: "Unsouled", Author: "Will Wight"})
	if err := c.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetLibrary(ctx, lib.ID); err != ErrNotFound {
		t.Fatalf("expected library gone, got %v", err)
	}
	// The FTS index must not retain orphaned rows for the deleted library.
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	if hits, _ := c.Search(ctx, "unsouled", all, 10); len(hits) != 0 {
		t.Fatalf("expected no FTS hits after delete, got %d", len(hits))
	}
}

func TestDeleteBooksNotIn(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "keep.m4b", Title: "Keep"})
	c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "gone.m4b", Title: "Gone"})
	n, err := c.DeleteBooksNotIn(ctx, lib.ID, map[string]bool{"keep.m4b": true})
	if err != nil || n != 1 {
		t.Fatalf("expected 1 removed, got %d err=%v", n, err)
	}
	page, _ := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID})
	if len(page.Books) != 1 || page.Books[0].RelPath != "keep.m4b" {
		t.Fatalf("unexpected remaining books: %+v", page.Books)
	}
}
