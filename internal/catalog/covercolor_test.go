package catalog

import (
	"testing"
)

// coverState reads a book's stored cover colour and version through GetBookByPath
// (bookCols), failing the test on an error.
func coverState(t *testing.T, c *Catalog, libID int64, path string) (*CoverColor, string) {
	t.Helper()
	b, err := c.GetBookByPath(t.Context(), libID, path)
	if err != nil {
		t.Fatal(err)
	}
	return b.CoverColor, b.CoverVersion
}

func recordCover(t *testing.T, c *Catalog, libID int64, path, version string, cc CoverColor) {
	t.Helper()
	srcs, err := c.CoverSources(t.Context(), libID, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RecordCoverColors(t.Context(), []CoverColorRecord{{
		LibraryID: libID, Path: path, Source: srcs[path], Version: version, Color: cc}}); err != nil {
		t.Fatal(err)
	}
}

// TestRecordCoverColors: a thumbnail's colours and version land on the book and
// read back on every book shape (item, list, search, recent), with or without an
// accent; CoverSources reports what is stored.
func TestRecordCoverColors(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "Alpha", MTime: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "b.m4b", Title: "Beta", MTime: 1}); err != nil {
		t.Fatal(err)
	}
	if cc, v := coverState(t, c, lib.ID, "a.m4b"); cc != nil || v != "" {
		t.Fatalf("new book cover = %+v %q, want none", cc, v)
	}
	full := CoverColor{Bg: "#141e50", Accent: "#f08c14", OnAccent: "#000000"}
	recordCover(t, c, lib.ID, "a.m4b", "v1", full)
	recordCover(t, c, lib.ID, "b.m4b", "v2", CoverColor{Bg: "#757575"})

	if cc, v := coverState(t, c, lib.ID, "a.m4b"); cc == nil || *cc != full || v != "v1" {
		t.Fatalf("item cover = %+v %q, want %+v v1", cc, v, full)
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
	srcs, err := c.CoverSources(ctx, lib.ID, []string{"a.m4b"})
	if err != nil {
		t.Fatal(err)
	}
	if s := srcs["a.m4b"]; s.Version != "v1" || !s.Colored {
		t.Fatalf("cover source = %+v, want version v1, colored", s)
	}
}

// TestUpsertBookClearsCoverOnArtChange: a re-index that changes the book's mtime
// or sidecar clears its colours and version (the art may be new); one that changes
// neither keeps them.
func TestUpsertBookClearsCoverOnArtChange(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	book := Book{LibraryID: lib.ID, RelPath: "a.m4b", Title: "A", MTime: 1, CoverPath: "a.jpg"}
	upsert := func(b Book) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &b); err != nil {
			t.Fatal(err)
		}
	}
	upsert(book)
	cc := CoverColor{Bg: "#102030"}

	recordCover(t, c, lib.ID, "a.m4b", "v1", cc)
	retitled := book
	retitled.Title = "Retitled"
	upsert(retitled)
	if got, v := coverState(t, c, lib.ID, "a.m4b"); got == nil || v != "v1" {
		t.Fatalf("unchanged art: cover = %+v %q, want kept", got, v)
	}

	touched := book
	touched.MTime = 2
	upsert(touched)
	if got, v := coverState(t, c, lib.ID, "a.m4b"); got != nil || v != "" {
		t.Fatalf("mtime changed: cover = %+v %q, want cleared", got, v)
	}

	recordCover(t, c, lib.ID, "a.m4b", "v2", cc)
	resleeved := touched
	resleeved.CoverPath = "folder.jpg"
	upsert(resleeved)
	if got, v := coverState(t, c, lib.ID, "a.m4b"); got != nil || v != "" {
		t.Fatalf("sidecar changed: cover = %+v %q, want cleared", got, v)
	}
}

// TestCustomCoverResetsCoverVersion: a custom cover upload moves cover_version to
// the new art at once (the hash of its stamp, as a thumbnail of it would record)
// and clears the old art's colours; removing it clears both. A record made from
// the art as it was before the upload is not stored over it.
func TestCustomCoverResetsCoverVersion(t *testing.T) {
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp"})
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "A/B", Title: "B", IsFolder: true}); err != nil {
		t.Fatal(err)
	}
	before, err := c.CoverSources(ctx, lib.ID, []string{"A/B"})
	if err != nil {
		t.Fatal(err)
	}
	recordCover(t, c, lib.ID, "A/B", "sidecar", CoverColor{Bg: "#102030"})

	if err := c.SetCover(ctx, lib.ID, "A/B", pngBytes, 0); err != nil {
		t.Fatal(err)
	}
	info, err := c.CoverInfo(ctx, lib.ID, "A/B")
	if err != nil {
		t.Fatal(err)
	}
	want := CoverVersion(CustomArtVersion(info.UpdatedAt))
	if cc, v := coverState(t, c, lib.ID, "A/B"); cc != nil || v != want {
		t.Fatalf("after upload: cover = %+v %q, want no colour and %q", cc, v, want)
	}

	// A thumbnail of the sidecar that finished after the upload is dropped.
	if err := c.RecordCoverColors(ctx, []CoverColorRecord{{LibraryID: lib.ID, Path: "A/B",
		Source: before["A/B"], Version: "stale", Color: CoverColor{Bg: "#ffffff"}}}); err != nil {
		t.Fatal(err)
	}
	if cc, v := coverState(t, c, lib.ID, "A/B"); cc != nil || v != want {
		t.Fatalf("stale record stored: cover = %+v %q", cc, v)
	}

	if err := c.DeleteCover(ctx, lib.ID, "A/B"); err != nil {
		t.Fatal(err)
	}
	if cc, v := coverState(t, c, lib.ID, "A/B"); cc != nil || v != "" {
		t.Fatalf("after delete: cover = %+v %q, want cleared", cc, v)
	}
}

func TestCoverVersionIsShortAndStable(t *testing.T) {
	a, b := CoverVersion("s100-1"), CoverVersion("s100-2")
	if len(a) != 10 || a == b || a != CoverVersion("s100-1") {
		t.Fatalf("CoverVersion = %q / %q; want 10 stable, distinct characters", a, b)
	}
}
