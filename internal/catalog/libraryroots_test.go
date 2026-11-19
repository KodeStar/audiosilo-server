package catalog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The overlap rule by platform: one folder, or one inside the other past a
// separator, never a sibling whose name only starts the same; a volume's root
// holds every folder on it; Windows and macOS fold case, Linux doesn't.
func TestRootsOverlap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		sep  byte
		fold bool
		want bool
	}{
		{"/srv/books", "/srv/books", '/', false, true},
		{"/srv/books/pods", "/srv/books", '/', false, true},
		{"/srv/books", "/srv/books/pods", '/', false, true},
		{"/srv/books2", "/srv/books", '/', false, false},
		{"/srv/pods", "/srv/books", '/', false, false},
		{"/", "/srv/books", '/', false, true},
		{"/srv/Books/pods", "/srv/books", '/', false, false},
		{"/srv/Books/pods", "/srv/books", '/', true, true},
		{`C:\`, `C:\Audio\Pods`, '\\', true, true},
		{`c:\audio`, `C:\Audio\Pods`, '\\', true, true},
		{`C:\Audio2`, `C:\Audio\Pods`, '\\', true, false},
		{`D:\`, `C:\Audio`, '\\', true, false},
		{`\\nas\media\`, `\\NAS\Media\Pods`, '\\', true, true},
		{`\\nas\media2\pods`, `\\nas\media\`, '\\', true, false},
	} {
		if got := rootsOverlap(tc.a, tc.b, tc.sep, tc.fold); got != tc.want {
			t.Errorf("rootsOverlap(%q, %q, fold %v) = %v, want %v", tc.a, tc.b, tc.fold, got, tc.want)
		}
	}
}

// A podcast library whose root is reached through a link into another
// library's folder (or holds a link-made path to one) is refused on create
// and on an edit (denied), its folder not there yet included; a link that
// leads elsewhere is no overlap (allowed). A pair stored before a link made
// them overlap is listed by RootOverlaps, the launcher's start warning.
func TestPodcastRootOverlapThroughLinks(t *testing.T) {
	c, ctx := newTestCatalog(t)
	base := t.TempDir()
	booksDir := filepath.Join(base, "books")
	elsewhere := filepath.Join(base, "elsewhere")
	for _, d := range []string{booksDir, elsewhere} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := func(name, to string) string {
		t.Helper()
		p := filepath.Join(base, name)
		if err := os.Symlink(to, p); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
		return p
	}
	toBooks, toElsewhere := link("to-books", booksDir), link("to-elsewhere", elsewhere)
	books, err := c.CreateLibrary(ctx, Library{Name: "Books", Root: booksDir})
	if err != nil {
		t.Fatal(err)
	}

	for _, root := range []string{toBooks, filepath.Join(toBooks, "Podcasts"), filepath.Join(toBooks, "not", "there", "yet")} {
		_, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: root, Kind: KindPodcasts})
		if !errors.Is(err, ErrRootOverlaps) || !strings.Contains(err.Error(), `"Books"`) {
			t.Errorf("podcast root %s through a link into the books: %v", root, err)
		}
	}
	pods, err := c.CreateLibrary(ctx, Library{Name: "Pods", Root: toElsewhere, Kind: KindPodcasts})
	if err != nil {
		t.Fatalf("podcast root through a link elsewhere: %v", err)
	}
	if _, _, err := c.UpdateLibrary(ctx, pods.ID, LibraryPatch{Root: filepath.Join(toBooks, "Shows")}); !errors.Is(err, ErrRootOverlaps) {
		t.Errorf("edit onto a link into the books: %v", err)
	}
	// A books library through a link into the podcasts' folder.
	if _, err := c.CreateLibrary(ctx, Library{Name: "More", Root: filepath.Join(link("to-pods", elsewhere), "More")}); !errors.Is(err, ErrRootOverlaps) {
		t.Errorf("books root through a link into the podcasts: %v", err)
	}
	if got, err := c.RootOverlaps(ctx); err != nil || len(got) != 0 {
		t.Fatalf("overlaps with none stored: %+v %v", got, err)
	}

	// The podcasts' link moved onto the books after both were stored.
	if err := os.Remove(toElsewhere); err != nil {
		t.Fatal(err)
	}
	link("to-elsewhere", filepath.Join(booksDir, "Shows"))
	if err := os.Mkdir(filepath.Join(booksDir, "Shows"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := c.RootOverlaps(ctx)
	if err != nil || len(got) != 1 || got[0].Library.ID != books.ID || got[0].Other.ID != pods.ID {
		t.Fatalf("overlaps once a link made one: %+v %v", got, err)
	}
}
