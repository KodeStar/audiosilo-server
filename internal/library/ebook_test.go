package library

import (
	"os"
	"path/filepath"
	"testing"
)

// BookEbook finds what the Bridge's "auto" source reads: the one regular .epub
// directly in a folder book's folder (never a link, a hidden file, one deeper
// down; several is none), or "<stem>.epub" beside a single-file book (a link
// inside the library counts, as the Bridge follows it); never one outside the
// library.
func TestBookEbook(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Author/Book/01.m4b")
	write("Author/Book/Book.EPUB")
	write("Author/Book/Extras/other.epub")
	write("Author/Linked/01.m4b")
	write("Author/Hidden/01.m4b")
	write("Author/Hidden/.book.epub")
	write("Collection/One.m4b")
	write("Collection/One.epub")
	write("Collection/Two.m4b")
	write("Collection/Three.epub")
	write("Loose.mp3")
	write("Loose.epub")
	write("outside/Secret.epub")
	write("Author/Two/01.m4b")
	write("Author/Two/Two.epub")
	write("Author/Two/Two (Annotated).epub")
	write("Collection/Four.m4b")
	write("Collection/Five.m4b")
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Author", "Linked", "Linked.epub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Collection", "Four.epub")); err != nil {
		t.Fatal(err)
	}
	away := filepath.Join(t.TempDir(), "Away.epub")
	if err := os.WriteFile(away, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(away, filepath.Join(root, "Collection", "Five.epub")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		rel    string
		folder bool
		want   string
	}{
		{"Author/Book", true, "Author/Book/Book.EPUB"},
		{"Author/Linked", true, ""}, // a link is never followed
		{"Author/Hidden", true, ""}, // nor a hidden file
		{"Author/Two", true, ""},    // several: the Bridge reads the audio
		{"Collection/One.m4b", false, "Collection/One.epub"},
		{"Collection/Two.m4b", false, ""},                      // another book's epub doesn't count
		{"Collection/Four.m4b", false, "Collection/Four.epub"}, // a link inside the library
		{"Collection/Five.m4b", false, ""},                     // one leading out of it
		{"Loose.mp3", false, "Loose.epub"},
		{"Author/Missing", true, ""},
		{"../outside", true, ""},
	} {
		if got := BookEbook(root, tc.rel, tc.folder); got != tc.want {
			t.Errorf("BookEbook(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}
