package catalog

import (
	"context"
	"testing"
	"time"
)

// upsertFixture is a library and the books a test writes into it, with a
// helper reading back which of them have rows and how often the content
// generation moved.
type upsertFixture struct {
	c   *Catalog
	ctx context.Context
	lib Library
}

func newUpsertFixture(t *testing.T) upsertFixture {
	t.Helper()
	c, ctx := newTestCatalog(t)
	lib, err := c.CreateLibrary(ctx, Library{Name: "L", Root: "/tmp/l"})
	if err != nil {
		t.Fatal(err)
	}
	return upsertFixture{c, ctx, *lib}
}

// books returns n folder books (two files and a chapter each), titled by path.
func (f upsertFixture) books(paths ...string) []*Book {
	var out []*Book
	for _, p := range paths {
		out = append(out, &Book{LibraryID: f.lib.ID, RelPath: p, IsFolder: true, Title: "Title " + p,
			Author: "Author", AddedAt: "2026-01-01T00:00:00Z",
			Files: []BookFile{{RelPath: p + "/01.mp3", Seq: 0, Duration: 10}, {RelPath: p + "/02.mp3", Seq: 1, Duration: 20}}})
	}
	return out
}

func (f upsertFixture) gen(t *testing.T) uint64 {
	t.Helper()
	g, ok := f.c.ContentGeneration()
	if !ok {
		t.Fatal("no content generation")
	}
	return g
}

// rows reports, for each path, whether it has a books row, how many file rows
// it has, and whether its FTS row finds its title.
func (f upsertFixture) rows(t *testing.T, path string) (book bool, files int, fts bool) {
	t.Helper()
	var id int64
	err := f.c.db.QueryRowContext(f.ctx, `SELECT id FROM books WHERE library_id = ? AND rel_path = ?`, f.lib.ID, path).Scan(&id)
	if err != nil {
		return false, 0, false
	}
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT count(*) FROM book_files WHERE book_id = ?`, id).Scan(&files); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.c.db.QueryRowContext(f.ctx, `SELECT count(*) FROM books_fts WHERE rowid = ? AND books_fts MATCH ?`,
		id, `"Title `+path+`"`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return true, files, n == 1
}

// UpsertBooks writes every book as UpsertBook does (files and the FTS row), in
// one transaction with no budget, or one a book when the budget is spent at once.
func TestUpsertBooksBatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		budget time.Duration
		txs    uint64 // transactions, so content generation moves
	}{
		{"one transaction", 0, 1},
		{"a transaction a book", time.Nanosecond, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUpsertFixture(t)
			var heard int
			f.c.OnBookChange(func() { heard++ })
			books := f.books("A", "B", "C")
			before := f.gen(t)
			for i, err := range f.c.UpsertBooks(f.ctx, books, tc.budget) {
				if err != nil {
					t.Fatalf("book %d: %v", i, err)
				}
			}
			if moved := f.gen(t) - before; moved != tc.txs {
				t.Errorf("content generation moved %d times, want %d (one a transaction)", moved, tc.txs)
			}
			if heard != int(tc.txs) {
				t.Errorf("OnBookChange heard %d times, want %d", heard, tc.txs)
			}
			for _, b := range books {
				if b.ID == 0 {
					t.Errorf("%s: no id set", b.RelPath)
				}
				if ok, files, fts := f.rows(t, b.RelPath); !ok || files != 2 || !fts {
					t.Errorf("%s: row %v, %d files, fts %v; want a row, 2 files, an FTS row", b.RelPath, ok, files, fts)
				}
			}
			// Written again (a re-index), the same rows: the upsert's conflict path.
			for i, err := range f.c.UpsertBooks(f.ctx, f.books("A", "B", "C"), tc.budget) {
				if err != nil {
					t.Fatalf("again, book %d: %v", i, err)
				}
			}
			var n int
			if err := f.c.db.QueryRowContext(f.ctx, `SELECT count(*) FROM books`).Scan(&n); err != nil || n != 3 {
				t.Errorf("books after a re-index = %d (%v), want 3", n, err)
			}
		})
	}
}

// A book that fails part way through its writes is rolled back alone (its
// books row too, written before the failure), and the others in its
// transaction are written; its error is at its index.
func TestUpsertBooksIsolatesAFailingBook(t *testing.T) {
	t.Parallel()
	f := newUpsertFixture(t)
	// The writer pool is one connection, so a temporary trigger on it sees every write.
	if _, err := f.c.db.ExecContext(f.ctx, `CREATE TEMP TRIGGER fail_b AFTER INSERT ON main.book_files
		WHEN NEW.rel_path = 'B/02.mp3' BEGIN SELECT RAISE(ABORT, 'disk says no'); END`); err != nil {
		t.Fatal(err)
	}
	before := f.gen(t)
	errs := f.c.UpsertBooks(f.ctx, f.books("A", "B", "C"), 0)
	if errs[0] != nil || errs[1] == nil || errs[2] != nil {
		t.Fatalf("errors = %v, want only B's", errs)
	}
	if moved := f.gen(t) - before; moved != 1 {
		t.Errorf("content generation moved %d times, want 1: B was rolled back inside the one transaction", moved)
	}
	if ok, _, _ := f.rows(t, "B"); ok {
		t.Error("B has a books row: its savepoint wasn't rolled back")
	}
	for _, p := range []string{"A", "C"} {
		if ok, files, fts := f.rows(t, p); !ok || files != 2 || !fts {
			t.Errorf("%s: row %v, %d files, fts %v; want it written", p, ok, files, fts)
		}
	}
}

// A transaction that fails as a whole (here SQLite rolls it back: RAISE
// ROLLBACK) has the books it reached written again one at a time: the failing
// book fails again on its own, the one before it is written; the books after
// it share the next transaction.
func TestUpsertBooksRetriesAFailedTransactionSingly(t *testing.T) {
	t.Parallel()
	f := newUpsertFixture(t)
	if _, err := f.c.db.ExecContext(f.ctx, `CREATE TEMP TRIGGER fail_b AFTER INSERT ON main.book_files
		WHEN NEW.rel_path = 'B/02.mp3' BEGIN SELECT RAISE(ROLLBACK, 'gone'); END`); err != nil {
		t.Fatal(err)
	}
	before := f.gen(t)
	errs := f.c.UpsertBooks(f.ctx, f.books("A", "B", "C", "D"), 0)
	if errs[0] != nil || errs[1] == nil || errs[2] != nil || errs[3] != nil {
		t.Fatalf("errors = %v, want only B's", errs)
	}
	if moved := f.gen(t) - before; moved != 4 {
		t.Errorf("content generation moved %d times, want 4: the failed transaction (A, B), A and B alone, then C and D", moved)
	}
	for _, p := range []string{"A", "C", "D"} {
		if ok, files, fts := f.rows(t, p); !ok || files != 2 || !fts {
			t.Errorf("%s: row %v, %d files, fts %v; want it written", p, ok, files, fts)
		}
	}
	if ok, _, _ := f.rows(t, "B"); ok {
		t.Error("B has a books row")
	}
}
