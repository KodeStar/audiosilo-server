package catalog

import (
	"testing"
	"time"
)

// TestRowCoverVersions: a favourite and a /me/progress row carry their book's
// cover_version (the Book's, so a cover URL busts the same cache), follow it when
// the cover changes, and omit it for a path that is no indexed book. A progress
// row read any other way (GetProgress, a save's echo) never carries one, even when
// a PUT body echoed one back.
func TestRowCoverVersions(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	uid := seedUser(t, c, ctx)
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "A/Book", IsFolder: true,
		Title: "Book", MTime: 1, Size: 10}); err != nil {
		t.Fatal(err)
	}
	book, nav := Ref{LibraryID: lib.ID, Path: "A/Book"}, Ref{LibraryID: lib.ID, Path: "A"}
	all := []Scope{{LibraryID: lib.ID, AllowAll: true}}
	for _, ref := range []Ref{book, nav} {
		if err := c.AddFavourite(ctx, uid, ref); err != nil {
			t.Fatal(err)
		}
		if _, err := c.SaveProgress(ctx, uid, Progress{Ref: ref, Position: 5, Duration: 10,
			UpdatedAt: time.Now().UTC().Format(time.RFC3339), CoverVersion: "echoed"}); err != nil {
			t.Fatal(err)
		}
	}
	versions := func() (favs, progress map[string]string) {
		t.Helper()
		favs, progress = map[string]string{}, map[string]string{}
		fl, err := c.ListAllFavourites(ctx, uid, all)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range fl {
			favs[f.Path] = f.CoverVersion
		}
		pl, err := c.ListProgress(ctx, uid, all)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pl {
			progress[p.Path] = p.CoverVersion
		}
		return favs, progress
	}
	check := func(stage string) string {
		t.Helper()
		_, want := coverState(t, c, lib.ID, book.Path)
		favs, progress := versions()
		if want == "" || favs[book.Path] != want || progress[book.Path] != want {
			t.Fatalf("%s: favourite %q, progress %q, want the book's %q", stage, favs[book.Path], progress[book.Path], want)
		}
		if v, ok := favs[nav.Path]; !ok || v != "" {
			t.Fatalf("%s: navigation folder favourite version %q (listed %v), want none", stage, v, ok)
		}
		if v, ok := progress[nav.Path]; !ok || v != "" {
			t.Fatalf("%s: unindexed progress version %q (listed %v), want none", stage, v, ok)
		}
		return want
	}
	file := check("file art")
	if err := c.SetCover(ctx, lib.ID, book.Path, pngBytes, 0, SourceEdited); err != nil {
		t.Fatal(err)
	}
	if custom := check("custom cover"); custom == file {
		t.Fatalf("custom cover kept the file art's version %q", file)
	}

	// Only the list carries it: the single read and the save's echo don't.
	if p, err := c.GetProgress(ctx, uid, book); err != nil || p == nil || p.CoverVersion != "" {
		t.Fatalf("GetProgress = %+v (%v), want no cover_version", p, err)
	}
	saved, err := c.SaveProgress(ctx, uid, Progress{Ref: book, Position: 6, Duration: 10,
		UpdatedAt: time.Now().Add(time.Second).UTC().Format(time.RFC3339), CoverVersion: "echoed"})
	if err != nil || saved.CoverVersion != "" {
		t.Fatalf("save echo = %+v (%v), want no cover_version", saved, err)
	}
}
