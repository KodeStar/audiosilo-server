package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup carries the player's ratings (durable user state, player redesign
// Phase 1b): the table and its rows.
func TestBackupHoldsRatings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO libraries(name, root, created_at) VALUES('L', '/tmp/l', 'x')`,
		`INSERT INTO ratings(user_id, library_id, rel_path, rating, note, created_at, updated_at)
		 VALUES((SELECT id FROM users WHERE username = 'first'), (SELECT id FROM libraries), 'A/Book', 4, 'kept',
		        '2026-10-01T09:00:00.000Z', '2026-10-01T09:00:00.000Z')`,
	} {
		if _, err := e.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	m, err := e.svc.Create(ctx, KindManual)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, filepath.Join(e.svc.dir, m.Name))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var stars int
	var note string
	if err := db.QueryRowContext(ctx, `SELECT rating, note FROM ratings WHERE rel_path = 'A/Book'`).Scan(&stars, &note); err != nil {
		t.Fatalf("the backup has no rating: %v", err)
	}
	if stars != 4 || note != "kept" {
		t.Fatalf("backed-up rating = %d %q", stars, note)
	}
}
