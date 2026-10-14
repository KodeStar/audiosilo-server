package store

import (
	"context"
	"testing"
)

// TestMigration0044HouseholdMembers: the member columns arrive on a database with
// users and tokens in it (NULL / the defaults for every existing row), the CHECKs
// refuse an unknown kind or access, and the SET NULL references let a deleted
// owner or a deleted device leave their member and member token behind.
func TestMigration0044HouseholdMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0044")
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(1, 'adam', 'h', 'user', 't', 't')`)
	exec(`INSERT INTO tokens(id, user_id, token_hash, kind, created_at) VALUES(1, 1, 'x', 'session', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var (
		canAdd          int
		display         string
		managed, kind   any
		access, created any
	)
	if err := db.writer.QueryRow(`SELECT managed_by, member_kind, member_access, can_add_members, display_name FROM users WHERE id = 1`).
		Scan(&managed, &kind, &access, &canAdd, &display); err != nil {
		t.Fatal(err)
	}
	if managed != nil || kind != nil || access != nil || canAdd != 1 || display != "" {
		t.Fatalf("existing user = %v %v %v %d %q", managed, kind, access, canAdd, display)
	}
	if err := db.writer.QueryRow(`SELECT created_on_token_id FROM tokens WHERE id = 1`).Scan(&created); err != nil || created != nil {
		t.Fatalf("existing token = %v, %v", created, err)
	}

	if _, err := db.writer.Exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at, managed_by, member_kind, member_access)
	  VALUES(2, 'evie', '', 'user', 't', 't', 1, 'kid', 'collections')`); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`UPDATE users SET member_kind = 'teen' WHERE id = 2`,
		`UPDATE users SET member_access = 'some' WHERE id = 2`,
	} {
		if _, err := db.writer.Exec(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
	if _, err := db.writer.Exec(`INSERT INTO tokens(id, user_id, token_hash, kind, created_at, created_on_token_id) VALUES(2, 2, 'y', 'session', 't', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(`DELETE FROM tokens WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT created_on_token_id FROM tokens WHERE id = 2`).Scan(&created); err != nil || created != nil {
		t.Fatalf("member token after its device went = %v, %v", created, err)
	}
	if _, err := db.writer.Exec(`DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT managed_by FROM users WHERE id = 2`).Scan(&managed); err != nil || managed != nil {
		t.Fatalf("member after the owner went = %v, %v", managed, err)
	}
}
