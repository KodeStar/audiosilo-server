package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// ebookLibrary lays out books with and without an ebook beside them under a new
// root: what TestBookEbook covers, as books a scan finds (real audio; the
// single-file books sit at the root, where each file is a book).
func ebookLibrary(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	for _, audio := range []string{"Author/Book/01.m4b", "Author/Linked/01.m4b", "Author/Hidden/01.m4b",
		"Author/Two/01.m4b", "Author/Bad/01.m4b", "One.m4b", "Two.m4b",
		"Four.m4b", "Five.m4b", "Author/None/01.m4b"} {
		copyFixtureM4B(t, filepath.Join(root, filepath.FromSlash(audio)))
	}
	writeFile(t, root, "Author/Book/Book.EPUB", epub)
	writeFile(t, root, "Author/Book/Extras/other.epub", epub) // deeper down: not the book's
	writeFile(t, root, "Author/Hidden/.book.epub", epub)
	writeFile(t, root, "Author/Two/Two.epub", epub)
	writeFile(t, root, "Author/Two/Two (Annotated).epub", epub)
	writeFile(t, root, "Author/Bad/Bad.epub", []byte("PK not a zip"))
	writeFile(t, root, "One.epub", epub)
	writeFile(t, root, "Three.epub", epub) // no book of that name
	writeFile(t, root, "outside/Secret.epub", epub)
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Author", "Linked", "Linked.epub")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside", "Secret.epub"), filepath.Join(root, "Four.epub")); err != nil {
		t.Fatal(err)
	}
	away := filepath.Join(t.TempDir(), "Away.epub")
	if err := os.WriteFile(away, epub, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(away, filepath.Join(root, "Five.epub")); err != nil {
		t.Fatal(err)
	}
	return root
}

// ebookBooks are ebookLibrary's books and the ebook each holds ("" for none).
var ebookBooks = map[string]string{
	"Author/Book":   "Author/Book/Book.EPUB",
	"Author/Linked": "", // a folder book's link is never followed
	"Author/Hidden": "", // nor a hidden file
	"Author/Two":    "", // several: none
	"Author/Bad":    "", // not a readable epub
	"Author/None":   "",
	"One.m4b":       "One.epub", // single-file books (at the root)
	"Two.m4b":       "",
	"Four.m4b":      "Four.epub", // a link inside the library
	"Five.m4b":      "",          // one leading out of it
}

// The scan records each book's ebook by BookEbook's rule, read from its walk:
// what it stores is exactly what BookEbook (the estimate's) answers, on the wire
// as {path, size}, and an on-demand index (IndexPath, no walk) agrees.
func TestScanRecordsEbookByBookEbookRule(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := ebookLibrary(t)
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "E", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	for book, want := range ebookBooks {
		b, err := cat.GetBookByPath(ctx, lib.ID, book)
		if err != nil {
			t.Fatalf("%s: %v", book, err)
		}
		if got := BookEbook(root, book, b.IsFolder); got != want {
			t.Errorf("BookEbook(%q) = %q, want %q", book, got, want)
		}
		switch {
		case want == "" && b.Ebook != nil:
			t.Errorf("%s: ebook = %+v, want none", book, b.Ebook)
		case want != "" && (b.Ebook == nil || b.Ebook.Path != want || b.Ebook.Size != sizeOf(t, root, want)):
			t.Errorf("%s: ebook = %+v, want %s", book, b.Ebook, want)
		}
	}

	// On demand: a book the scan hasn't reached gets the same answer.
	copyFixtureM4B(t, filepath.Join(root, "Late", "Book", "01.m4b"))
	if err := os.WriteFile(filepath.Join(root, "Late", "Book", "Late.epub"), epubBytes(t, minimalEpub...), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := s.IndexPath(ctx, *lib, "Late/Book")
	if err != nil {
		t.Fatal(err)
	}
	if b.Ebook == nil || b.Ebook.Path != "Late/Book/Late.epub" {
		t.Fatalf("on-demand ebook = %+v, want Late/Book/Late.epub", b.Ebook)
	}
}

// sizeOf is the size of the file at library-relative rel (through a link).
func sizeOf(t *testing.T, root, rel string) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// An ebook added, changed, made unreadable, removed or made one of several
// beside unchanged audio is recorded by the next scan without re-indexing the
// book (an upgraded library gains its ebooks the same way: ebook_sig starts
// empty); a book whose epub is unchanged reads nothing.
func TestScanBackfillsEbookWithoutReindex(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "01.m4b"))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "E", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ebook := func() *catalog.Ebook {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		return b.Ebook
	}
	scan := func() {
		t.Helper()
		res, err := s.Scan(ctx, *lib)
		if err != nil {
			t.Fatal(err)
		}
		// The first scan adds the book; every later one leaves it unchanged.
		if res.Updated != 0 {
			t.Fatalf("the book was re-indexed: %+v", res.ScanCounts)
		}
	}
	scan()
	if e := ebook(); e != nil {
		t.Fatalf("no epub yet: ebook = %+v", e)
	}

	epubPath := filepath.Join(root, "Author", "Book", "Book.epub")
	if err := os.WriteFile(epubPath, epubBytes(t, minimalEpub...), 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e == nil || e.Path != "Author/Book/Book.epub" {
		t.Fatalf("added: ebook = %+v", e)
	}

	// Changed (bigger, later), still readable: the new size.
	bigger := epubBytes(t, append(minimalEpub, "OEBPS/ch1.xhtml", "OEBPS/ch2.xhtml")...)
	if err := os.WriteFile(epubPath, bigger, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(epubPath, later, later); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e == nil || e.Size != int64(len(bigger)) {
		t.Fatalf("changed: ebook = %+v, want size %d", e, len(bigger))
	}

	// Made unreadable (a DRM'd or corrupt replacement): none.
	if err := os.WriteFile(epubPath, []byte(strings.Repeat("x", 64)), 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e != nil {
		t.Fatalf("unreadable: ebook = %+v, want none", e)
	}

	// Readable again, then a second epub: several is none.
	if err := os.WriteFile(epubPath, epubBytes(t, minimalEpub...), 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e == nil {
		t.Fatal("readable again: no ebook")
	}
	second := filepath.Join(root, "Author", "Book", "Other.epub")
	if err := os.WriteFile(second, epubBytes(t, minimalEpub...), 0o644); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e != nil {
		t.Fatalf("two epubs: ebook = %+v, want none", e)
	}

	// Removed (both): none; renamed back in under another name: that name.
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(root, "Author", "Book", "Renamed.epub")
	if err := os.Rename(epubPath, renamed); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e == nil || e.Path != "Author/Book/Renamed.epub" {
		t.Fatalf("renamed (size and time kept): ebook = %+v", e)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	scan()
	if e := ebook(); e != nil {
		t.Fatalf("removed: ebook = %+v, want none", e)
	}
}
