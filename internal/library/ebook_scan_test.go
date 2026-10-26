package library

import (
	"os"
	"path/filepath"
	"runtime"
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

	// Made unreadable (a corrupt replacement): none.
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

// A single-file book's "<stem>.epub" is what the disk opens by that name, as the
// Bridge's os.Stat finds it: "Six.EPUB" beside "Six.m4b" is its ebook on a
// case-insensitive disk (recorded as "Six.epub") and none on a case-sensitive
// one, the scan, an on-demand index and BookEbook agreeing either way.
func TestEbookCaseVariantBesideSingleFile(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	copyFixtureM4B(t, filepath.Join(root, "Six.m4b"))
	copyFixtureM4B(t, filepath.Join(root, "Seven.m4b"))
	writeFile(t, root, "Six.EPUB", epub)
	writeFile(t, root, "seven.epub", epub)
	want := map[string]string{"Six.m4b": "", "Seven.m4b": ""}
	if _, err := os.Stat(filepath.Join(root, "Six.epub")); err == nil { // a case-insensitive disk
		want = map[string]string{"Six.m4b": "Six.epub", "Seven.m4b": "Seven.epub"}
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "E", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	path := func(e *catalog.Ebook) string {
		if e == nil {
			return ""
		}
		return e.Path
	}
	for book, w := range want {
		if got := BookEbook(root, book, false); got != w {
			t.Errorf("BookEbook(%q) = %q, want %q", book, got, w)
		}
		b, err := cat.GetBookByPath(ctx, lib.ID, book)
		if err != nil {
			t.Fatal(err)
		}
		if got := path(b.Ebook); got != w {
			t.Errorf("scan: %s ebook = %q, want %q", book, got, w)
		}
		ib, err := s.IndexPath(ctx, *lib, book)
		if err != nil {
			t.Fatal(err)
		}
		if got := path(ib.Ebook); got != w {
			t.Errorf("on demand: %s ebook = %q, want %q", book, got, w)
		}
	}
}

// A single-file book's ebook that is a link inside the library whose target goes
// is none from the next scan on (recorded by the link's own signature), not the
// recorded path the reader would 404 on; its target back, the scan after records
// it again.
func TestScanEbookLinkToNothing(t *testing.T) {
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	epub := epubBytes(t, minimalEpub...)
	copyFixtureM4B(t, filepath.Join(root, "One.m4b"))
	writeFile(t, root, "Ebooks/real.epub", epub)
	if err := os.Symlink(filepath.Join(root, "Ebooks", "real.epub"), filepath.Join(root, "One.epub")); err != nil {
		t.Fatal(err)
	}
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "E", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFor := func(when, want string) {
		t.Helper()
		if _, err := s.Scan(ctx, *lib); err != nil {
			t.Fatal(err)
		}
		b, err := cat.GetBookByPath(ctx, lib.ID, "One.m4b")
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case want == "" && b.Ebook != nil:
			t.Fatalf("%s: ebook = %+v, want none", when, b.Ebook)
		case want != "" && (b.Ebook == nil || b.Ebook.Path != want):
			t.Fatalf("%s: ebook = %+v, want %s", when, b.Ebook, want)
		}
		if got := BookEbook(root, "One.m4b", false); got != want {
			t.Fatalf("%s: BookEbook = %q, want %q", when, got, want)
		}
	}
	scanFor("the link's target there", "One.epub")
	if err := os.Remove(filepath.Join(root, "Ebooks", "real.epub")); err != nil {
		t.Fatal(err)
	}
	scanFor("the target gone", "")
	scanFor("the target still gone", "")
	writeFile(t, root, "Ebooks/real.epub", epub)
	scanFor("the target back", "One.epub")
}

// An ebook that can't be read when its book is indexed again (a permission, a
// flaky mount) keeps what is stored, on demand and in a scan that re-indexes the
// book, rather than taking the reader's ebook away until the next scan.
func TestIndexKeepsEbookItCantRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions that bind (not root)")
	}
	t.Parallel()
	cat, s, ctx := newScanEnv(t)
	root := t.TempDir()
	copyFixtureM4B(t, filepath.Join(root, "Author", "Book", "01.m4b"))
	writeFile(t, root, "Author/Book/Book.epub", epubBytes(t, minimalEpub...))
	lib, err := cat.CreateLibrary(ctx, catalog.Library{Name: "E", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	kept := func(when string) {
		t.Helper()
		b, err := cat.GetBookByPath(ctx, lib.ID, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		if b.Ebook == nil || b.Ebook.Path != "Author/Book/Book.epub" {
			t.Fatalf("%s: ebook = %+v, want the stored Author/Book/Book.epub", when, b.Ebook)
		}
	}
	kept("first scan")
	epubPath := filepath.Join(root, "Author", "Book", "Book.epub")
	if err := os.Chmod(epubPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(epubPath, 0o644) })
	if _, err := s.IndexPath(ctx, *lib, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	kept("IndexPath")
	// The audio changed: the scan re-indexes the book.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(root, "Author", "Book", "01.m4b"), later, later); err != nil {
		t.Fatal(err)
	}
	res, err := s.Scan(ctx, *lib)
	if err != nil {
		t.Fatal(err)
	}
	if res.Updated != 1 {
		t.Fatalf("the book wasn't re-indexed: %+v", res.ScanCounts)
	}
	kept("a scan's re-index")
}
