package library

import (
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// A cover image put beside a book that had none is the book's cover from the
// next scan, though its audio is unchanged: a folder book takes any image in
// its folder, a loose file in the library's root a conventionally named one beside it. A book whose
// cover is already known is not read again for it.
func TestScanFindsCoverAddedToUnchangedBook(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "01.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Single.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Author", "Covered", "01.m4b"))
	writeFile(t, root, "Author/Covered/cover.jpg", []byte("img"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "C", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scan := func() *ScanResult {
		t.Helper()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	coverOf := func(rel string) string {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, rel)
		if err != nil {
			t.Fatal(err)
		}
		return b.CoverPath
	}
	scan()
	if got := coverOf("Author/Book"); got != "" {
		t.Fatalf("before: cover %q, want none", got)
	}

	writeFile(t, root, "Author/Book/Some Book - Front.png", []byte("img"))
	writeFile(t, root, "folder.jpg", []byte("img"))
	res := scan()
	if got := coverOf("Author/Book"); got != "Author/Book/Some Book - Front.png" {
		t.Fatalf("folder book: cover %q, want the image added", got)
	}
	if got := coverOf("Single.m4b"); got != "folder.jpg" {
		t.Fatalf("loose file: cover %q, want the folder image added", got)
	}
	if res.Updated != 2 {
		t.Fatalf("updated %d, want the two books whose cover appeared", res.Updated)
	}

	// Nothing new: no book is read again.
	if res := scan(); res.Updated != 0 {
		t.Fatalf("rescan updated %d, want 0", res.Updated)
	}
	if got := coverOf("Author/Covered"); got != "Author/Covered/cover.jpg" {
		t.Fatalf("covered book: %q", got)
	}
}
