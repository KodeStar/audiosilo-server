package store

import (
	"slices"
	"testing"
)

func TestMigration0095Matched(t *testing.T) {
	t.Parallel()
	dsn, exec, closeRaw := openBefore(t, "0095")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'A', '/a', 't'), (2, 'B', '/b', 't')`)
	exec(`INSERT INTO books(library_id, rel_path, asin, isbn, indexed_at) VALUES
		(1, 'Alone', '', '', 't'), (1, 'ASIN', 'x', '', 't'), (1, 'ISBN', '', 'y', 't'),
		(1, 'Both', 'x', '', 't'), (1, 'Recorded', '', '', 't'), (2, 'Recorded', '', '', 't')`)
	exec(`INSERT INTO book_matches(library_id, path, work_id, how, set_at) VALUES
		(1, 'Both', 'w', 'auto', 't'), (1, 'Recorded', 'w', 'accepted', 't'), (2, 'Gone', 'v', 'auto', 't')`)
	closeRaw()
	ctx := t.Context()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	check := func(want ...string) {
		t.Helper()
		got := lines(t, db, `SELECT library_id || '/' || rel_path FROM books WHERE matched ORDER BY 1`)
		if !slices.Equal(got, want) {
			t.Fatalf("matched books = %v, want %v", got, want)
		}
	}
	check("1/ASIN", "1/Both", "1/ISBN", "1/Recorded")

	// Writes as an older build or a later migration would issue them. A move
	// can change either half of the key on either table, in either order.
	for _, q := range []string{
		`DELETE FROM book_matches WHERE path = 'Both'`,
		`UPDATE book_matches SET library_id = 2 WHERE path = 'Recorded'`,
		`UPDATE books SET library_id = 2, rel_path = 'Gone' WHERE rel_path = 'Alone'`,
		`UPDATE books SET rel_path = 'New' WHERE library_id = 2 AND rel_path = 'Recorded'`,
		`INSERT INTO books(library_id, rel_path, indexed_at) VALUES(2, 'Recorded', 't')`,
		`INSERT INTO books(library_id, rel_path, isbn, indexed_at) VALUES(2, 'New ISBN', 'y', 't')`,
		`UPDATE books SET isbn = '' WHERE rel_path = 'ISBN'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
		if stale := lines(t, db, `SELECT b.rel_path FROM books b WHERE b.matched <> (
			b.asin <> '' OR b.isbn <> '' OR EXISTS (
				SELECT 1 FROM book_matches m WHERE m.library_id = b.library_id AND m.path = b.rel_path))`); len(stale) > 0 {
			t.Fatalf("stale flags after %s: %v", q, stale)
		}
	}
	check("1/ASIN", "1/Both", "2/Gone", "2/New ISBN", "2/Recorded")
}
