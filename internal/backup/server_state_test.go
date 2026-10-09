package backup

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// A backup carries server_state (migration 0040; the support card's answer), so a
// restored server remembers a donation.
func TestBackupHoldsServerState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.db.ExecContext(ctx,
		`INSERT INTO server_state(key, value, updated_at)
		 VALUES('support_card', '{"choice":"donated"}', '2026-10-09T10:00:00.000Z')`); err != nil {
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
	var value string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM server_state WHERE key = 'support_card'`).Scan(&value); err != nil ||
		value != `{"choice":"donated"}` {
		t.Fatalf("the backup's support_card = %q, %v", value, err)
	}
}
