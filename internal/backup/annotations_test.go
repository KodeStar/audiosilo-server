package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup carries a bookmark's label (migration 0030) with the bookmark.
func TestBackupHoldsBookmarkLabels(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO libraries(name, root, created_at) VALUES('L', '/tmp/l', 'x')`,
		`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, label, created_at)
		 SELECT id, 1, 'A/Book', 12, 'n', 'fell_asleep', 'x' FROM users WHERE username = 'first'`,
	} {
		if _, err := e.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
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
	var label string
	if err := db.QueryRowContext(ctx, `SELECT label FROM bookmarks WHERE rel_path = 'A/Book'`).Scan(&label); err != nil ||
		label != "fell_asleep" {
		t.Fatalf("the backup's bookmark label = %q, %v; want fell_asleep", label, err)
	}
}
