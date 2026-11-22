package store

import (
	"context"
	"slices"
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
	got := lines(t, db, `SELECT reason || ' ' || import_job_id || ' ' || updated_at || ' [' || fold_refusal_reason || '] ' ||
		    COALESCE(fold_refused_at, 'null') FROM audible_titles WHERE account = 'acct0001' AND asin = 'B000000001'`)
	if want := []string{"contested 42 u [] null"}; !slices.Equal(got, want) {
		t.Fatalf("legacy title after migration = %q, want %q", got, want)
	}
}
