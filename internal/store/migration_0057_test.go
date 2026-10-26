package store

import (
	"context"
	"testing"
)

// TestMigration0057ReadingProgress: a reader's place is one row per user,
// library and book path, and goes with its user or its library.
func TestMigration0057ReadingProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0057")
	exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(1, 'ann', '', 't', 't')`)
	exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(2, 'bob', '', 't', 't')`)
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(2, 'More', '/m', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	insert := func(user, lib int, path string) error {
		_, err := db.writer.Exec(`INSERT INTO reading_progress(user_id, library_id, rel_path, cfi, progression, version, updated_at)
		                          VALUES(?, ?, ?, 'epubcfi(/6/2!/4/2/1:0)', 0.5, 1, 't')`, user, lib, path)
		return err
	}
	for _, r := range []struct {
		user, lib int
		path      string
	}{{1, 1, "A/Book"}, {2, 1, "A/Book"}, {1, 2, "A/Book"}, {1, 1, "A/Other"}} {
		if err := insert(r.user, r.lib, r.path); err != nil {
			t.Fatalf("insert %v: %v", r, err)
		}
	}
	// Denied: a second row for the same user, library and path.
	if err := insert(1, 1, "A/Book"); err == nil {
		t.Fatal("a second place for the same book was stored")
	}
	var name string
	if err := db.writer.QueryRow(`SELECT device_name FROM reading_progress WHERE user_id = 2`).Scan(&name); err != nil || name != "" {
		t.Fatalf("device_name = %q, %v, want '' by default", name, err)
	}
	if _, err := db.writer.Exec(`DELETE FROM users WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(`DELETE FROM libraries WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM reading_progress`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows left = %d, %v, want 2 (user 1's in library 1)", n, err)
	}
}
