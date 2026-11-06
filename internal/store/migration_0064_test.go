package store

import (
	"context"
	"testing"
)

// TestMigration0064AudibleImport: an account that already polled starts its
// policy from its last poll (the library it had is never a policy's), one that
// never did from its creation; no template; no title waits for a backup.
func TestMigration0064AudibleImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0064")
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(1, 'u', 'x', 'admin', 't', 't')`)
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO audible_accounts(account, marketplace, user_id, library_id, created_at, updated_at, last_poll_at)
	      VALUES('acct0001', 'us', 1, 1, '2026-11-01T10:00:00Z', 'u', '2026-11-02T10:00:00Z')`)
	exec(`INSERT INTO audible_accounts(account, marketplace, user_id, library_id, created_at, updated_at)
	      VALUES('acct0002', 'us', 1, 1, '2026-11-01T11:00:00Z', 'u')`)
	exec(`INSERT INTO audible_titles(account, asin, state, first_seen, updated_at) VALUES('acct0001', 'B000000001', 'available', 'f', 'u')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for id, want := range map[string]string{"acct0001": "2026-11-02T10:00:00Z", "acct0002": "2026-11-01T11:00:00Z"} {
		var since, template string
		if err := db.writer.QueryRow(`SELECT policy_since, template FROM audible_accounts WHERE account = ?`, id).
			Scan(&since, &template); err != nil {
			t.Fatal(err)
		}
		if since != want || template != "" {
			t.Errorf("%s: policy_since %q template %q, want %q and none", id, since, template, want)
		}
	}
	var pending int
	if err := db.writer.QueryRow(`SELECT import_pending FROM audible_titles`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("import_pending = %d, %v", pending, err)
	}
}
