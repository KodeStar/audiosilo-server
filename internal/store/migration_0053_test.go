package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestMigration0053SetupState: a server upgraded with a library or a second
// account is marked as set up (finished and dismissed), so its admins never see
// the first-run flow; one with a single admin and nothing else, or an empty
// database, gets no row and sees the flow. A demo visitor and a household member
// are not a second account.
func TestMigration0053SetupState(t *testing.T) {
	t.Parallel()
	const done = `{"finished":true,"dismissed":true,"steps":{}}`
	admin := `INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(1, 'admin', 'h', 'admin', 't', 't')`
	for name, tc := range map[string]struct {
		fill []string
		want string // "" = no row
	}{
		"a library": {fill: []string{admin,
			`INSERT INTO libraries(id, name, root, created_at, kind) VALUES(1, 'Pods', '/p', 't', 'podcasts')`}, want: done},
		"a second user": {fill: []string{admin,
			`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(2, 'bob', 'h', 't', 't')`}, want: done},
		"only a demo visitor and a member besides the admin": {fill: []string{admin,
			`INSERT INTO users(id, username, password_hash, is_demo, created_at, updated_at) VALUES(2, 'demo', 'h', 1, 't', 't')`,
			`INSERT INTO users(id, username, password_hash, managed_by, member_kind, member_access, created_at, updated_at)
			 VALUES(3, 'kid', 'h', 1, 'kid', 'all', 't', 't')`}},
		"only the admin": {fill: []string{admin}},
		"empty":          {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dsn, exec, closeRaw := openBefore(t, "0053")
			for _, q := range tc.fill {
				exec(q)
			}
			closeRaw()
			db, err := Open(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			var got string
			err = db.writer.QueryRow(`SELECT value FROM server_state WHERE key = 'setup_state'`).Scan(&got)
			switch {
			case tc.want == "" && errors.Is(err, sql.ErrNoRows):
			case err != nil:
				t.Fatalf("setup_state: %v (want %q)", err, tc.want)
			case got != tc.want:
				t.Fatalf("setup_state = %q, want %q", got, tc.want)
			}
		})
	}
}
