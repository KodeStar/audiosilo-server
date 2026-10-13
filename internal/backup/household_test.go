package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup carries the household choices (migration 0042) and the cast tokens'
// grants (0043; the tokens themselves are tokens rows, hashes only), like every
// other table.
func TestBackupHoldsHouseholdAndCastTokens(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO household_sharing(user_id, share_listening, updated_at)
		 SELECT id, 1, '2026-10-09T10:00:00.000Z' FROM users WHERE username = 'first'`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO tokens(id, user_id, token_hash, kind, created_at)
		 SELECT 100, id, 'phone', 'session', '2026-10-09T10:00:00Z' FROM users WHERE username = 'first'`,
		`INSERT INTO tokens(id, user_id, token_hash, kind, created_at, expires_at)
		 SELECT 101, id, 'abc', 'cast', '2026-10-09T10:00:00Z', '2026-10-10T10:00:00Z' FROM users WHERE username = 'first'`,
		`INSERT INTO cast_grants(token_id, library_id, rel_path, parent_token_id) VALUES(101, 1, 'Author/Book', 100)`,
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
	var on bool
	if err := db.QueryRowContext(ctx,
		`SELECT h.share_listening FROM household_sharing h JOIN users u ON u.id = h.user_id WHERE u.username = 'first'`).
		Scan(&on); err != nil || !on {
		t.Fatalf("the backup's household choice = %v, %v; want on", on, err)
	}
	var path string
	if err := db.QueryRowContext(ctx,
		`SELECT g.rel_path FROM cast_grants g JOIN tokens t ON t.id = g.token_id WHERE t.token_hash = 'abc'`).
		Scan(&path); err != nil || path != "Author/Book" {
		t.Fatalf("the backup's cast token = %q, %v", path, err)
	}
}
