package store

import (
	"context"
	"testing"
)

func TestMigration0093KeepsExistingAudibleTitle(t *testing.T) {
	t.Parallel()
	dsn, exec, closeRaw := openBefore(t, "0093")
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(1, 'u', 'x', 'admin', 't', 't')`)
	exec(`INSERT INTO audible_accounts(account, marketplace, user_id, created_at, updated_at) VALUES('acct0001', 'us', 1, 't', 't')`)
	exec(`INSERT INTO audible_titles(account, asin, state, reason, import_job_id, first_seen, updated_at)
		  VALUES('acct0001', 'B000000001', 'available', 'contested', 42, 'f', 'u')`)
	closeRaw()
	ctx := context.Background()
	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var oldTitle bool
	err = db.QueryRowContext(ctx, `SELECT reason = 'contested' AND import_job_id = 42 AND updated_at = 'u'
		    AND fold_refusal_reason = '' AND fold_refused_at IS NULL
		  FROM audible_titles WHERE account = 'acct0001' AND asin = 'B000000001'`).Scan(&oldTitle)
	if err != nil || !oldTitle {
		t.Fatalf("legacy title after migration: %t, %v", oldTitle, err)
	}
}
