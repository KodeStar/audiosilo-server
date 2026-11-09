package store

import (
	"context"
	"testing"
)

// TestMigration0071MatchItemUnchanged: an existing run's items gain unchanged,
// empty for every row 0070 left (no backfill), and a new item may name scopes.
func TestMigration0071MatchItemUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0071")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO match_runs(id, mode, status, started_at) VALUES(1, 'refresh', 'ready', 't')`)
	exec(`INSERT INTO match_run_items(run_id, library_id, path, outcome) VALUES(1, 1, 'A/Book', 'auto')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var outcome, unchanged string
	if err := db.writer.QueryRow(`SELECT outcome, unchanged FROM match_run_items WHERE path = 'A/Book'`).
		Scan(&outcome, &unchanged); err != nil {
		t.Fatal(err)
	}
	if outcome != "auto" || unchanged != "" {
		t.Fatalf("after 0071: outcome %q unchanged %q; want the row kept, unchanged ''", outcome, unchanged)
	}
	if _, err := db.writer.Exec(`INSERT INTO match_run_items(run_id, library_id, path, outcome, unchanged)
	     VALUES(1, 1, 'A/Other', 'auto', 'fill')`); err != nil {
		t.Fatal(err)
	}
}
