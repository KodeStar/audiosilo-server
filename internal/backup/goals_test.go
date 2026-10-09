package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup carries the listening goals (migration 0028), like every other table.
func TestBackupHoldsListeningGoals(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO listening_goals(user_id, books_per_year, updated_at)
		 SELECT id, 24, '2026-10-04T03:00:00Z' FROM users WHERE username = 'first'`); err != nil {
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
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT g.books_per_year FROM listening_goals g JOIN users u ON u.id = g.user_id WHERE u.username = 'first'`).
		Scan(&n); err != nil || n != 24 {
		t.Fatalf("the backup's goal = %d, %v; want 24", n, err)
	}
}
