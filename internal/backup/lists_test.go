package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup holds the player's up-next queue and collections (Phase 1b durable
// user state) with their rows.
func TestBackupHoldsQueueAndCollections(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO libraries(name, root, created_at) VALUES('L', '/tmp/l', 'x')`,
		`INSERT INTO up_next(user_id, library_id, rel_path, position, added_at) VALUES(1, 1, 'A/Book', 0, 'x')`,
		`INSERT INTO collections(user_id, name, created_at, updated_at) VALUES(1, 'Bedtime', 'x', 'x')`,
		`INSERT INTO collection_items(collection_id, library_id, rel_path, position, added_at) VALUES(1, 1, 'A/Book', 0, 'x')`,
	} {
		if _, err := e.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	addUser(t, e.db, "second")
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO collection_shares(collection_id, user_id, created_at) VALUES(1, 2, 'x')`); err != nil {
		t.Fatal(err)
	}

	b, err := e.svc.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(e.svc.dir, b.Name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, table := range []string{"up_next", "collections", "collection_items", "collection_shares"} {
		var n int
		// The table names are the fixed list above.
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatalf("%s in the backup: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("%s in the backup = %d rows, want 1", table, n)
		}
	}
}
