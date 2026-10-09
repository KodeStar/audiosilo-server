package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup carries the household choices (migration 0041) and the cast tokens
// (0042, hashes only, like the tokens table), like every other table.
func TestBackupHoldsHouseholdAndCastTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO household_sharing(user_id, share_listening, updated_at)
		 SELECT id, 1, '2026-10-09T10:00:00.000Z' FROM users WHERE username = 'first'`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO cast_tokens(token_hash, user_id, library_id, rel_path, created_at, expires_at)
		 SELECT 'abc', id, 1, 'Author/Book', '2026-10-09T10:00:00.000Z', '2026-10-10T10:00:00.000Z' FROM users WHERE username = 'first'`); err != nil {
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
	var on bool
	if err := db.QueryRowContext(ctx,
		`SELECT h.share_listening FROM household_sharing h JOIN users u ON u.id = h.user_id WHERE u.username = 'first'`).
		Scan(&on); err != nil || !on {
		t.Fatalf("the backup's household choice = %v, %v; want on", on, err)
	}
	var path string
	if err := db.QueryRowContext(ctx, `SELECT rel_path FROM cast_tokens WHERE token_hash = 'abc'`).Scan(&path); err != nil || path != "Author/Book" {
		t.Fatalf("the backup's cast token = %q, %v", path, err)
	}
}
