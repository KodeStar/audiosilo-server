package store

import (
	"context"
	"testing"
)

// TestMigration0081ClearsStaleGrantMarks: a share marked as a library grant
// that no longer holds its library's whole-library rule (narrowed, left with
// no rule, or marked for a library since deleted) loses the mark, so it can be
// changed like any share. A grant that holds the rule keeps it, extra folders
// or not, and an unmarked share is untouched.
func TestMigration0081ClearsStaleGrantMarks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0081")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Kids', '/kids', 't'), (2, 'Other', '/other', 't')`)
	exec(`INSERT INTO shares(id, name, created_at, whole_library_id) VALUES
		(1, 'whole', 't', 1), (2, 'whole and more', 't', 1), (3, 'narrowed', 't', 1),
		(4, 'empty', 't', 1), (5, 'library gone', 't', 9), (6, 'other root', 't', 1), (7, 'own', 't', NULL)`)
	exec(`INSERT INTO share_paths(share_id, library_id, path) VALUES
		(1, 1, ''), (2, 1, ''), (2, 2, 'Folder'), (3, 1, 'Gruffalo'), (5, 2, 'Folder'), (6, 2, ''), (7, 1, '')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	rows, err := db.QueryContext(ctx, `SELECT name FROM shares WHERE whole_library_id IS NOT NULL ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var marked []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		marked = append(marked, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(marked) != 2 || marked[0] != "whole" || marked[1] != "whole and more" {
		t.Fatalf("still marked = %v; want only the shares holding their library's rule", marked)
	}
}
