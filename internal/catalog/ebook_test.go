package catalog

import (
	"io/fs"
	"testing"
	"time"
)

// fakeInfo is a file's info with only a size and a modification time.
type fakeInfo struct {
	fs.FileInfo
	size int64
	mod  time.Time
}

func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) ModTime() time.Time { return f.mod }

// ebookAt is the wire ebook of the book at rel (nil for none).
func ebookAt(t *testing.T, c *Catalog, libraryID int64, rel string) *Ebook {
	t.Helper()
	b, err := c.GetBookByPath(t.Context(), libraryID, rel)
	if err != nil {
		t.Fatal(err)
	}
	return b.Ebook
}

// The wire ebook is the stored path and size (its own column, server#228),
// none without a path; the signature leads with the size and ends with the name.
func TestEbookOf(t *testing.T) {
	t.Parallel()
	if e := ebookOf("A/My: Book.epub", 1234); e == nil || e.Size != 1234 || e.Path != "A/My: Book.epub" {
		t.Errorf("ebookOf = %+v, want A/My: Book.epub of 1234 bytes", e)
	}
	if e := ebookOf("", 12); e != nil {
		t.Errorf("no path: ebookOf = %+v, want none", e)
	}
	info := fakeInfo{size: 1234, mod: time.Unix(1700000000, 5)}
	if sig := EbookSig("My: Book.epub", info); sig != "1234:1700000000000000005:My: Book.epub" {
		t.Errorf("EbookSig = %q", sig)
	}
}

// The size is recorded with the reading (UpsertBook and SetEbooks), and none
// for a reading with no readable ebook, so the wire never reads it back out of
// the signature.
func TestEbookSizeColumn(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Book", IsFolder: true, Title: "T",
		EbookFile: EbookReading{Path: "Book/b.epub", Sig: "999:1:b.epub", Size: 77}}); err != nil {
		t.Fatal(err)
	}
	if e := ebookAt(t, c, lib.ID, "Book"); e == nil || e.Size != 77 {
		t.Fatalf("upserted ebook = %+v, want the reading's size 77 (not the signature's)", e)
	}
	if err := c.SetEbooks(ctx, lib.ID, map[string]EbookReading{"Book": {Path: "Book/c.epub", Sig: "5:2:c.epub", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	if e := ebookAt(t, c, lib.ID, "Book"); e == nil || e.Size != 5 || e.Path != "Book/c.epub" {
		t.Fatalf("backfilled ebook = %+v, want Book/c.epub of 5 bytes", e)
	}
	if err := c.SetEbooks(ctx, lib.ID, map[string]EbookReading{"Book": {Sig: "5:3:c.epub", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := c.db.QueryRowContext(ctx, `SELECT ebook_size FROM books WHERE rel_path = 'Book'`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	if size != 0 || ebookAt(t, c, lib.ID, "Book") != nil {
		t.Errorf("an unreadable ebook: size %d, want none", size)
	}
}

// An ebook that couldn't be read right now (Unread) keeps what is stored over a
// re-index, and a new row gets none; any other reading is recorded as it is.
func TestUpsertBookKeepsUnreadEbook(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	upsert := func(rel string, r EbookReading) {
		t.Helper()
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: rel, IsFolder: true, Title: "T", EbookFile: r}); err != nil {
			t.Fatal(err)
		}
	}
	upsert("Book", EbookReading{Path: "Book/Book.epub", Sig: "10:1:Book.epub", Size: 10})
	upsert("Book", EbookReading{Unread: true})
	if e := ebookAt(t, c, lib.ID, "Book"); e == nil || e.Path != "Book/Book.epub" || e.Size != 10 {
		t.Fatalf("after an unread re-index: ebook = %+v, want the stored Book/Book.epub", e)
	}
	if sig := signatureOf(t, c, lib.ID, "Book").EbookSig; sig != "10:1:Book.epub" {
		t.Fatalf("after an unread re-index: ebook_sig = %q, want the stored one", sig)
	}
	upsert("Book", EbookReading{})
	if e := ebookAt(t, c, lib.ID, "Book"); e != nil {
		t.Fatalf("a re-index that found none: ebook = %+v, want none", e)
	}
	upsert("New", EbookReading{Unread: true})
	if e := ebookAt(t, c, lib.ID, "New"); e != nil || signatureOf(t, c, lib.ID, "New").EbookSig != "" {
		t.Fatalf("a new row read unread: ebook = %+v, want none and no signature", e)
	}
}

// A scan's reading (Was: the row as the scan read it) is recorded only while the
// row is still as read: an index of the book since (an on-demand rescan, newer
// than the scan's walk) stays, whatever it recorded.
func TestSetEbooksCompareAndSet(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Book", IsFolder: true, Title: "T"}); err != nil {
		t.Fatal(err)
	}
	read := signatureOf(t, c, lib.ID, "Book") // as a scan reads it before its walk
	was := &EbookState{Sig: read.EbookSig, IndexedAt: read.IndexedAt}

	// Indexed since (the epub removed and the book rescanned on demand: none
	// again, so only indexed_at tells): the scan's older reading is skipped.
	time.Sleep(time.Millisecond)
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Book", IsFolder: true, Title: "T"}); err != nil {
		t.Fatal(err)
	}
	stale := EbookReading{Path: "Book/Book.epub", Sig: "10:1:Book.epub", Was: was}
	if err := c.SetEbooks(ctx, lib.ID, map[string]EbookReading{"Book": stale}); err != nil {
		t.Fatal(err)
	}
	if e := ebookAt(t, c, lib.ID, "Book"); e != nil {
		t.Fatalf("a reading older than the index since was recorded: %+v", e)
	}

	// Taken against the row as it now is: recorded.
	now := signatureOf(t, c, lib.ID, "Book")
	fresh := EbookReading{Path: "Book/Book.epub", Sig: "10:1:Book.epub", Was: &EbookState{Sig: now.EbookSig, IndexedAt: now.IndexedAt}}
	if err := c.SetEbooks(ctx, lib.ID, map[string]EbookReading{"Book": fresh}); err != nil {
		t.Fatal(err)
	}
	if e := ebookAt(t, c, lib.ID, "Book"); e == nil || e.Path != "Book/Book.epub" {
		t.Fatalf("a current reading: ebook = %+v, want Book/Book.epub", e)
	}
}

// signatureOf is the stored Signature of the book at rel.
func signatureOf(t *testing.T, c *Catalog, libraryID int64, rel string) Signature {
	t.Helper()
	sigs, err := c.Signatures(t.Context(), libraryID)
	if err != nil {
		t.Fatal(err)
	}
	sig, ok := sigs[rel]
	if !ok {
		t.Fatalf("no book at %q", rel)
	}
	return sig
}
