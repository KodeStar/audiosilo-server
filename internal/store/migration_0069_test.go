package store

import (
	"context"
	"testing"
)

// TestMigration0069BookMatches: an existing database gains the empty
// book_matches table, one row per book path, how only auto or accepted, and the
// rows go with their library.
func TestMigration0069BookMatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0069")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM book_matches`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after 0069: %d rows, %v; want an empty table", n, err)
	}
	const insert = `INSERT INTO book_matches(library_id, path, work_id, how, set_at) VALUES(1, 'A/Book', 'the-work', ?, 't')`
	if _, err := db.writer.Exec(insert, "auto"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(insert, "accepted"); err == nil {
		t.Fatal("a second row for one path was accepted")
	}
	if _, err := db.writer.Exec(`INSERT INTO book_matches(library_id, path, work_id, how, set_at) VALUES(1, 'A/Other', 'w', 'guessed', 't')`); err == nil {
		t.Fatal("a how outside auto|accepted was accepted")
	}
	if _, err := db.writer.Exec(`DELETE FROM libraries WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM book_matches`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after the library went: %d rows, %v; want none", n, err)
	}
}
