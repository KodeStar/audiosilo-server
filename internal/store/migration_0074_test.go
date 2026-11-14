package store

import (
	"context"
	"testing"
)

// TestMigration0074BookHints (server#366): an existing database gains the empty
// book_hints table, one row per book path and source, and the rows go with their
// library.
func TestMigration0074BookHints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0074")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM book_hints`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after 0074: %d rows, %v; want an empty table", n, err)
	}
	const insert = `INSERT INTO book_hints(library_id, path, source, asin, import_id, set_at) VALUES(1, 'A/Book', ?, 'B0HINT0001', 3, 't')`
	if _, err := db.writer.Exec(insert, "abs"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(insert, "abs"); err == nil {
		t.Fatal("a second abs row for one path was accepted")
	}
	if _, err := db.writer.Exec(insert, "other"); err != nil {
		t.Fatalf("another source's row for the path: %v", err)
	}
	var isbn string
	if err := db.writer.QueryRow(`SELECT isbn FROM book_hints WHERE source = 'abs'`).Scan(&isbn); err != nil || isbn != "" {
		t.Fatalf("isbn = %q, %v; want '' by default", isbn, err)
	}
	if _, err := db.writer.Exec(`INSERT INTO book_hints(library_id, path, source, asin, import_id, set_at)
		VALUES(1, 'B/Book', 'abs', 'B0HINT0002', NULL, 't')`); err == nil {
		t.Fatal("a NULL import_id was accepted: 0 is none")
	}
	if _, err := db.writer.Exec(`DELETE FROM libraries WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM book_hints`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after the library went: %d rows, %v; want none", n, err)
	}
}
