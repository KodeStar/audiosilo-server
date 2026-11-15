package store

import (
	"context"
	"testing"
)

// TestMigration0078ChapterEdits (server#240): an existing database gains the
// empty chapter_edits table, one row per book path; a row outlives its editor's
// account (updated_by NULL) and goes with its library.
func TestMigration0078ChapterEdits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0078")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(7, 'admin', 'x', 'admin', 't', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM chapter_edits`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after 0078: %d rows, %v; want an empty table", n, err)
	}
	const insert = `INSERT INTO chapter_edits(library_id, path, list, updated_by, updated_at) VALUES(1, 'A/Book', '{}', 7, 't')`
	if _, err := db.writer.Exec(insert); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(insert); err == nil {
		t.Fatal("a second edit for one path was accepted")
	}
	if _, err := db.writer.Exec(`DELETE FROM users WHERE id = 7`); err != nil {
		t.Fatal(err)
	}
	var by *int64
	if err := db.writer.QueryRow(`SELECT updated_by FROM chapter_edits`).Scan(&by); err != nil || by != nil {
		t.Fatalf("updated_by = %v, %v; want NULL once the account is gone", by, err)
	}
	if _, err := db.writer.Exec(`DELETE FROM libraries WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM chapter_edits`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after the library went: %d rows, %v; want none", n, err)
	}
}
