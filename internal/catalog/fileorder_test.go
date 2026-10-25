package catalog

import (
	"maps"
	"slices"
	"testing"
)

// TestSignaturesStoredFileOrder (#177, 0055): a folder book's file order key is
// stored on the row, and Signatures reads it from there; a row indexed before
// the column existed (NULL) falls back to its book_files and says so, until
// SetFileOrders records it. A single-file book has none.
func TestSignaturesStoredFileOrder(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	files := []BookFile{{RelPath: "A/Book/1.mp3", Seq: 0}, {RelPath: "A/Book/2.mp3", Seq: 1}, {RelPath: "A/Book/10.mp3", Seq: 2}}
	key := FileOrderKey(files)
	if key == 0 {
		t.Fatal("no key for three files")
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "A/Book", IsFolder: true, Title: "Book", Files: files, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: "Loose.m4b", Title: "Loose",
		Files: []BookFile{{RelPath: "Loose.m4b"}}, AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	var stored *int64
	if err := c.db.QueryRowContext(ctx, `SELECT file_order FROM books WHERE rel_path = 'A/Book'`).Scan(&stored); err != nil || stored == nil || uint64(*stored) != key {
		t.Fatalf("stored file_order = %v, %v; want %d", stored, err, key)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT file_order FROM books WHERE rel_path = 'Loose.m4b'`).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("a single-file book's file_order = %v, %v; want NULL", stored, err)
	}

	sig := func(p string) Signature {
		t.Helper()
		sigs, err := c.Signatures(ctx, lib.ID)
		if err != nil {
			t.Fatal(err)
		}
		return sigs[p]
	}
	// The stored key is what Signatures reads: book_files are not looked at.
	if _, err := c.db.ExecContext(ctx, `UPDATE book_files SET seq = 2 - seq WHERE rel_path LIKE 'A/Book/%'`); err != nil {
		t.Fatal(err)
	}
	if s := sig("A/Book"); s.FileOrder != key || s.FileOrderUnset {
		t.Fatalf("stored: FileOrder %d unset %v; want %d from the row", s.FileOrder, s.FileOrderUnset, key)
	}
	if s := sig("Loose.m4b"); s.FileOrder != 0 || s.FileOrderUnset {
		t.Fatalf("single-file book: %+v", s)
	}

	// A row from before 0055: the key read from its book_files (in the order they
	// now hold, reversed above), flagged unset.
	if _, err := c.db.ExecContext(ctx, `UPDATE books SET file_order = NULL`); err != nil {
		t.Fatal(err)
	}
	reversed := slices.Clone(files)
	slices.Reverse(reversed)
	if s := sig("A/Book"); s.FileOrder != FileOrderKey(reversed) || !s.FileOrderUnset {
		t.Fatalf("NULL key: FileOrder %d unset %v; want %d from book_files", s.FileOrder, s.FileOrderUnset, FileOrderKey(reversed))
	}
	if s := sig("Loose.m4b"); s.FileOrder != 0 || s.FileOrderUnset {
		t.Fatalf("single-file book with a NULL key: %+v, want nothing to record", s)
	}

	// SetFileOrders records it (a key with the high bit set round-trips), and
	// never on a single-file book.
	const high = uint64(1)<<63 | 12345
	if err := c.SetFileOrders(ctx, lib.ID, map[string]uint64{"A/Book": high, "Loose.m4b": 7}); err != nil {
		t.Fatal(err)
	}
	if s := sig("A/Book"); s.FileOrder != high || s.FileOrderUnset {
		t.Fatalf("after SetFileOrders: %d unset %v; want %d", s.FileOrder, s.FileOrderUnset, high)
	}
	if err := c.db.QueryRowContext(ctx, `SELECT file_order FROM books WHERE rel_path = 'Loose.m4b'`).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("SetFileOrders wrote a single-file book: %v, %v", stored, err)
	}
	if err := c.SetFileOrders(ctx, lib.ID, nil); err != nil {
		t.Fatal(err)
	}
}

// TestFoldersHoldingBooks: a folder holds a book when the book is at it or below
// it, by whole segments ("A/B" holds nothing of "A/B2"), with LIKE's wildcards
// plain characters in a name, and only in the library asked about.
func TestFoldersHoldingBooks(t *testing.T) {
	t.Parallel()
	c, ctx := newTestCatalog(t)
	lib, _ := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	other, _ := c.CreateLibrary(ctx, Library{Name: "M", Root: "/tmp/m"})
	for _, p := range []string{"A/B2/Book", "Author/Series/Book", "Pct%/Book", "Under_score/Book", "Flat.m4b", "X/Joined"} {
		if _, err := c.UpsertBook(ctx, &Book{LibraryID: lib.ID, RelPath: p, IsFolder: true, Title: p, AddedAt: "2020-01-01"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.UpsertBook(ctx, &Book{LibraryID: other.ID, RelPath: "Elsewhere/Book", IsFolder: true, Title: "x", AddedAt: "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.FoldersHoldingBooks(ctx, lib.ID, []string{
		"A/B", "A/B2", "A", "Author", "Author/Series", "Author/Series/Book", "Author/Ser",
		"Pct%", "Pct", "P%", "Under_score", "UnderXscore", "Under", "X/Joined", "Elsewhere", "Nothing",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"A", "A/B2", "Author", "Author/Series", "Author/Series/Book", "Pct%", "Under_score", "X/Joined"}
	if keys := slices.Sorted(maps.Keys(got)); !slices.Equal(keys, want) {
		t.Fatalf("FoldersHoldingBooks = %q, want %q", keys, want)
	}
	if got, err := c.FoldersHoldingBooks(ctx, lib.ID, nil); err != nil || len(got) != 0 {
		t.Fatalf("no folders: %v, %v", got, err)
	}
}
