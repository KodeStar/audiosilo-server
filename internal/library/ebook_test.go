package library

import (
	"os"
	"path/filepath"
	"testing"
)

// BookEbook finds a regular .epub directly in a folder book's folder, or one
// named like a single-file book's audio beside it; never a link, a hidden file,
// one deeper down, or one outside the library.
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
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Author", "Linked", "Linked.epub")); err != nil {
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
		{"Collection/One.m4b", false, "Collection/One.epub"},
		{"Collection/Two.m4b", false, ""}, // another book's epub doesn't count
		{"Loose.mp3", false, "Loose.epub"},
		{"Author/Missing", true, ""},
		{"../outside", true, ""},
	} {
		if got := BookEbook(root, tc.rel, tc.folder); got != tc.want {
			t.Errorf("BookEbook(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}
