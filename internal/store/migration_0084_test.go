package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestMigration0084AddsPathRekeys (server#210): on a database a v2.6 server
// left, 0084 is pending, which is what makes the start take its before-upgrade
// copy (backup.BeforeUpgrade) before the re-key it brings runs; the log table
// starts empty, leaves every listener's row as it was, and goes with its user
// and its library.
func TestMigration0084AddsPathRekeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0084")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't'), (2, 'M', '/m', 't')`)
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES
		(1, 'ann', 'x', 'user', 't', 't'), (2, 'bob', 'x', 'user', 't', 't')`)
	exec(`INSERT INTO progress(user_id, library_id, rel_path, position, updated_at) VALUES(1, 1, 'A/Book/01.mp3', 42, 't')`)
	closeRaw()

	pending, err := PendingMigrations(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pending, "0084_path_rekeys.sql") {
		t.Fatalf("pending = %v, want 0084 (the before-upgrade copy's trigger)", pending)
	}

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if got := lines(t, db, `SELECT rel_path || ' ' || position FROM progress`); strings.Join(got, ",") != "A/Book/01.mp3 42.0" {
		t.Fatalf("progress after 0084 = %v, want the row untouched", got)
	}
	if got := lines(t, db, `SELECT COUNT(*) FROM path_rekeys`); got[0] != "0" {
		t.Fatalf("path_rekeys starts with %s rows", got[0])
	}
	for _, row := range []string{
		`(1, 1, 'progress')`, `(2, 1, 'progress')`, `(1, 2, 'ratings')`, `(NULL, 2, 'up_next')`,
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO path_rekeys(user_id, library_id, table_name, at, reason, from_path, to_path, row_json)
			SELECT column1, column2, column3, 't', 'file', 'a', 'b', '{}' FROM (VALUES `+row+`)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM libraries WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if got := lines(t, db, `SELECT COALESCE(user_id, 'NULL') || '/' || library_id FROM path_rekeys`); strings.Join(got, ",") != "2/1" {
		t.Fatalf("path_rekeys after a user and a library delete = %v, want only bob's row in library 1", got)
	}
	// A path's rows are an index seek in every table the re-key reads by path.
	for table, index := range map[string]string{"bookmarks": "idx_bookmarks_path", "notes": "idx_notes_path",
		"listening_history": "idx_history_path", "favourites": "idx_favourites_path"} {
		var id, parent, unused int
		var plan string
		if err := db.writer.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT * FROM `+table+`
			WHERE library_id = 1 AND rel_path = 'a'`).Scan(&id, &parent, &unused, &plan); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan, index) {
			t.Errorf("a %s read by path: plan %q, want %s", table, plan, index)
		}
	}
}
