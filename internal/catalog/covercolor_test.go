package catalog

import (
	"slices"
	"sync/atomic"
	"testing"
)

// coverState reads a book's cover colour and version through GetBookByPath
// (bookCols), failing the test on an error.
func coverState(t *testing.T, c *Catalog, libID int64, path string) (*CoverColor, string) {
	t.Helper()
	b, err := c.GetBookByPath(t.Context(), libID, path)
	if err != nil {
		t.Fatal(err)
	}
	return b.CoverColor, b.CoverVersion
}

// coverSource is the book's CoverSource, failing the test when there is none.
func coverSource(t *testing.T, c *Catalog, libID int64, path string) CoverSource {
	t.Helper()
	srcs, err := c.CoverSources(t.Context(), libID, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	src, ok := srcs[path]
	if !ok {
		t.Fatalf("no cover source for %q", path)
	}
	return src
}

// recordCover records cc for the art src was read under.
func recordCover(t *testing.T, c *Catalog, libID int64, path string, src CoverSource, cc CoverColor) {
	t.Helper()
	if err := c.RecordCoverColors(t.Context(), []CoverColorRecord{{
		LibraryID: libID, Path: path, Art: src.Art, Color: cc}}); err != nil {
		t.Fatal(err)
	}
}

// TestRecordCoverColors: a book has a cover_version from the moment it is
// indexed, before any thumbnail; a thumbnail's colours land on it and read back
// on every book shape (item, list, search, recent), with or without an accent;
// CoverSources reports them as recorded.
func TestRecordCoverColors(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "Alpha", MTime: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "b.m4b", Title: "Beta", MTime: 2}); err != nil {
		t.Fatal(err)
	}
	ccA, vA := coverState(t, c, lib.ID, "a.m4b")
	_, vB := coverState(t, c, lib.ID, "b.m4b")
	if ccA != nil || len(vA) != 10 || vB == vA {
		t.Fatalf("new books: colour %+v, versions %q / %q; want no colour and two distinct versions", ccA, vA, vB)
	}
	if src := coverSource(t, c, lib.ID, "a.m4b"); src.Colored || CoverVersion(src.Art) != vA {
		t.Fatalf("new book cover source = %+v, want uncoloured with art hashing to %q", src, vA)
	}
	full := CoverColor{Bg: "#141e50", Accent: "#f08c14", OnAccent: "#000000"}
	recordCover(t, c, lib.ID, "a.m4b", coverSource(t, c, lib.ID, "a.m4b"), full)
	recordCover(t, c, lib.ID, "b.m4b", coverSource(t, c, lib.ID, "b.m4b"), CoverColor{Bg: "#757575"})

	if cc, v := coverState(t, c, lib.ID, "a.m4b"); cc == nil || *cc != full || v != vA {
		t.Fatalf("item cover = %+v %q, want %+v %q", cc, v, full, vA)
	}
	if cc, _ := coverState(t, c, lib.ID, "b.m4b"); cc == nil || *cc != (CoverColor{Bg: "#757575"}) {
		t.Fatalf("bg-only cover = %+v", cc)
	}
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	page, err := c.ListBooks(ctx, ListOptions{LibraryID: lib.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found, err := c.Search(ctx, "alpha", all, 10)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := c.RecentBooks(ctx, all, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Books) != 2 || len(found) != 1 || len(recent) != 2 {
		t.Fatalf("list/search/recent = %d/%d/%d books, want 2/1/2", len(page.Books), len(found), len(recent))
	}
	for name, books := range map[string][]Book{"list": page.Books, "search": found, "recent": recent} {
		for _, b := range books {
			if b.CoverColor == nil || b.CoverVersion == "" {
				t.Fatalf("%s: %s has no cover colour/version", name, b.RelPath)
			}
		}
	}
	if src := coverSource(t, c, lib.ID, "a.m4b"); !src.Colored {
		t.Fatalf("cover source = %+v, want colored", src)
	}
}

// TestCoverVersionFollowsArt: a re-index that changes neither the book's mtime,
// size nor sidecar keeps its version and colour; one that changes any moves the
// version, and the colour read for the old art stops counting without being
// cleared.
func TestCoverVersionFollowsArt(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	book := Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 1, Size: 10, CoverPath: "a.jpg"}
	upsert := func(b Book) string {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
		_, v := coverState(t, c, lib.ID, "a.m4b")
		return v
	}
	v1 := upsert(book)
	cc := CoverColor{Bg: "#102030"}
	recordCover(t, c, lib.ID, "a.m4b", coverSource(t, c, lib.ID, "a.m4b"), cc)

	retitled := book
	retitled.Title = "Retitled"
	if v := upsert(retitled); v != v1 {
		t.Fatalf("unchanged art: version %q, want %q", v, v1)
	}
	if got, _ := coverState(t, c, lib.ID, "a.m4b"); got == nil {
		t.Fatal("unchanged art: colour dropped")
	}

	for name, b := range map[string]Book{
		"mtime":   {LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 2, Size: 10, CoverPath: "a.jpg"},
		"size":    {LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 1, Size: 11, CoverPath: "a.jpg"},
		"sidecar": {LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 1, Size: 10, CoverPath: "folder.jpg"},
	} {
		if v := upsert(b); v == v1 || v == "" {
			t.Fatalf("%s changed: version %q, want a new one", name, v)
		}
		if got, _ := coverState(t, c, lib.ID, "a.m4b"); got != nil {
			t.Fatalf("%s changed: colour %+v still exposed", name, got)
		}
		if coverSource(t, c, lib.ID, "a.m4b").Colored {
			t.Fatalf("%s changed: cover source still colored", name)
		}
	}
	// The original art again: its version, and the colour read for it, are back.
	if v := upsert(book); v != v1 {
		t.Fatalf("art restored: version %q, want %q", v, v1)
	}
	if got, _ := coverState(t, c, lib.ID, "a.m4b"); got == nil || *got != cc {
		t.Fatalf("art restored: colour %+v, want %+v", got, cc)
	}
}

// TestCustomCoverVersion: a custom cover upload moves cover_version to the hash
// of its stamp at once, and the file art's colour stops counting; a colour read
// from the art as it was before the upload is not stored over it (compare-and-set
// on the art); removing the cover reverts the version to the file art's, whose
// colour counts again.
func TestCustomCoverVersion(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "A/B", Title: "B", IsFolder: true, MTime: 5}); err != nil {
		t.Fatal(err)
	}
	_, fileVersion := coverState(t, c, lib.ID, "A/B")
	before := coverSource(t, c, lib.ID, "A/B")
	fileColor := CoverColor{Bg: "#102030"}
	recordCover(t, c, lib.ID, "A/B", before, fileColor)

	if err := c.SetCover(ctx, lib.ID, "A/B", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	info, err := c.CoverInfo(ctx, lib.ID, "A/B")
	if err != nil {
		t.Fatal(err)
	}
	want := CoverVersion(CustomArtVersion(info.UpdatedAt))
	if cc, v := coverState(t, c, lib.ID, "A/B"); cc != nil || v != want || v == fileVersion {
		t.Fatalf("after upload: cover = %+v %q, want no colour and %q", cc, v, want)
	}

	// A thumbnail of the file art that finished after the upload is dropped.
	recordCover(t, c, lib.ID, "A/B", before, CoverColor{Bg: "#ffffff"})
	if cc, v := coverState(t, c, lib.ID, "A/B"); cc != nil || v != want {
		t.Fatalf("stale record stored: cover = %+v %q", cc, v)
	}

	if err := c.DeleteCover(ctx, lib.ID, "A/B"); err != nil {
		t.Fatal(err)
	}
	if cc, v := coverState(t, c, lib.ID, "A/B"); cc == nil || *cc != fileColor || v != fileVersion {
		t.Fatalf("after delete: cover = %+v %q, want %+v %q", cc, v, fileColor, fileVersion)
	}
}

// TestDeleteMissingCoverKeepsVersion: deleting a custom cover a book does not
// have leaves its cover identity alone, including one a thumbnail already moved
// to the image's own version, and its colour stays exposed.
func TestDeleteMissingCoverKeepsVersion(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 1, Size: 10, CoverPath: "a.jpg"}); err != nil {
		t.Fatal(err)
	}
	src := coverSource(t, c, lib.ID, "a.m4b")
	cc := CoverColor{Bg: "#102030"}
	if err := c.RecordCoverColors(ctx, []CoverColorRecord{{
		LibraryID: lib.ID, Path: "a.m4b", Art: src.Art, Version: "s123-456", Color: cc}}); err != nil {
		t.Fatal(err)
	}
	gotCC, before := coverState(t, c, lib.ID, "a.m4b")
	if gotCC == nil || before != CoverVersion("s123-456") {
		t.Fatalf("after thumbnail: cover = %+v %q", gotCC, before)
	}
	if err := c.DeleteCover(ctx, lib.ID, "a.m4b"); err != nil {
		t.Fatal(err)
	}
	if gotCC, v := coverState(t, c, lib.ID, "a.m4b"); gotCC == nil || *gotCC != cc || v != before {
		t.Fatalf("after deleting a missing cover: cover = %+v %q, want %+v %q", gotCC, v, cc, before)
	}
}

// TestMovedCustomCoverVersion: a move carrying a custom cover gives the book at
// the new path the custom cover's version.
func TestMovedCustomCoverVersion(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	for _, p := range []string{"old.m4b", "new.m4b"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, Title: p, MTime: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetCover(ctx, lib.ID, "old.m4b", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	_, custom := coverState(t, c, lib.ID, "old.m4b")
	if err := c.MoveDurableState(ctx, lib.ID, "old.m4b", "new.m4b"); err != nil {
		t.Fatal(err)
	}
	if _, v := coverState(t, c, lib.ID, "new.m4b"); v != custom {
		t.Fatalf("moved book version = %q, want the custom cover's %q", v, custom)
	}
}

func TestCoverVersionIsShortAndStable(t *testing.T) {
	a, b := CoverVersion("s100-1"), CoverVersion("s100-2")
	if len(a) != 10 || a == b || a != CoverVersion("s100-1") {
		t.Fatalf("CoverVersion = %q / %q; want 10 stable, distinct characters", a, b)
	}
	if v := CoverVersion(""); v != "" {
		t.Fatalf("CoverVersion(\"\") = %q, want \"\"", v)
	}
}

// TestAdminBookCoverColor: the admin book list and book page carry a book's
// cover colour once one is recorded for its current art, and drop it when the
// art moves on.
func TestAdminBookCoverColor(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "Alpha", MTime: 1}); err != nil {
		t.Fatal(err)
	}
	listed := func() *CoverColor {
		t.Helper()
		page, err := c.ListAdminBooks(ctx, AdminListOptions{})
		if err != nil || len(page.Books) != 1 {
			t.Fatalf("list = %+v, %v", page, err)
		}
		d, err := c.AdminBookDetail(ctx, lib.ID, "a.m4b")
		if err != nil {
			t.Fatal(err)
		}
		if (page.Books[0].CoverColor == nil) != (d.Book.CoverColor == nil) {
			t.Fatalf("list colour %+v, book page colour %+v: want the same", page.Books[0].CoverColor, d.Book.CoverColor)
		}
		return page.Books[0].CoverColor
	}
	if cc := listed(); cc != nil {
		t.Fatalf("uncoloured book lists colour %+v", cc)
	}
	want := CoverColor{Bg: "#203040", Accent: "#e0a010", OnAccent: "#000000"}
	recordCover(t, c, lib.ID, "a.m4b", coverSource(t, c, lib.ID, "a.m4b"), want)
	if cc := listed(); cc == nil || *cc != want {
		t.Fatalf("listed colour = %+v, want %+v", cc, want)
	}
	if err := c.SetCover(ctx, lib.ID, "a.m4b", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	if cc := listed(); cc != nil {
		t.Fatalf("after a new cover the old colour still lists: %+v", cc)
	}
}

// TestCoverColorsDue: the books that may have art (a scan saw some or hasn't
// checked, or a custom cover) and hold no colour for it are due, in pages that
// carry on from next; a coloured book and one a scan found without art are not.
func TestCoverColorsDue(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	yes, no := true, false
	for _, b := range []Book{
		{RelPath: "art.m4b", HasCover: &yes},
		{RelPath: "unchecked.m4b"},
		{RelPath: "none.m4b", HasCover: &no},
		{RelPath: "custom.m4b", HasCover: &no},
		{RelPath: "colored.m4b", HasCover: &yes},
		{RelPath: "artless.m4b", HasCover: &yes},
	} {
		b.LibraryID, b.Title, b.MTime = lib.ID, b.RelPath, 1
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetCover(ctx, lib.ID, "custom.m4b", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	recordCover(t, c, lib.ID, "colored.m4b", coverSource(t, c, lib.ID, "colored.m4b"), CoverColor{Bg: "#101010"})
	// Read and found to have no colour: not due, and no colour on the book.
	recordCover(t, c, lib.ID, "artless.m4b", coverSource(t, c, lib.ID, "artless.m4b"), CoverColor{})
	if cc, _ := coverState(t, c, lib.ID, "artless.m4b"); cc != nil {
		t.Fatalf("artless book colour = %+v, want none", cc)
	}
	// Read, but not coloured: a thumbnail that does decode still records its colour.
	if src := coverSource(t, c, lib.ID, "artless.m4b"); src.Colored || !src.ColorRead {
		t.Fatalf("artless cover source = %+v, want read and not coloured", src)
	}

	var got []string
	var after int64
	pages := 0
	for {
		due, next, err := c.CoverColorsDue(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range due {
			if d.LibraryID != lib.ID || d.Source != coverSource(t, c, lib.ID, d.Path) {
				t.Fatalf("due %+v: want the library and the book's current art", d)
			}
			got = append(got, d.Path)
		}
		pages++
		if next == 0 {
			break
		}
		after = next
	}
	want := []string{"art.m4b", "unchecked.m4b", "custom.m4b"}
	if !slices.Equal(got, want) || pages != 3 {
		t.Fatalf("due = %v over %d pages, want %v over 3", got, pages, want)
	}
}

// TestOnBookChangeListeners: every listener hears a change, a custom cover set
// or removed included; removing a cover that isn't there is no change; a nil
// listener is ignored.
func TestOnBookChangeListeners(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	var a, b atomic.Int32
	c.OnBookChange(func() { a.Add(1) })
	c.OnBookChange(nil)
	c.OnBookChange(func() { b.Add(1) })
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCover(ctx, lib.ID, "a.m4b", pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCover(ctx, lib.ID, "a.m4b"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteCover(ctx, lib.ID, "a.m4b"); err != nil {
		t.Fatal(err)
	}
	if a.Load() != 3 || b.Load() != 3 {
		t.Fatalf("listeners heard %d and %d changes, want 3 each (index, cover set, cover removed)", a.Load(), b.Load())
	}
}
