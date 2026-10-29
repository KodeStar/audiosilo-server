package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// openUserState opens a migrated database (the real user-state tables) with
// one user, one token and the Bridge's row, and a misfiling recorder in place of
// the test binary's panic.
func openUserState(t *testing.T) (*DB, *[]string) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "us.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var misfiled []string
	db.onMisfiled = func(table string) { misfiled = append(misfiled, table) }
	for _, q := range []string{
		`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(1, 'u', 'x', 'user', 'a', 'a')`,
		`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'lib', '/lib', 'a')`,
		`INSERT INTO tokens(id, user_id, token_hash, kind, created_at) VALUES(1, 1, 'h', 'session', '2026-01-01T00:00:00Z')`,
		`INSERT INTO bridge_status(id, status) VALUES(1, '{}')`,
		`INSERT INTO bridge_jobs(id, kind, dedupe_key, state, created_at, updated_at) VALUES(1, 'bridge.verify', 'k', 'running', 'a', 'a')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	return db, &misfiled
}

// TestUserStateWritesLeaveTheContentGeneration: a user-state exec or
// transaction that changes only what its tables' rules let through leaves the
// content generation where it was; a default write, on the same tables, moves
// it.
func TestUserStateWritesLeaveTheContentGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, misfiled := openUserState(t)
	check := func(name string, wantMoved bool, f func() error) {
		t.Helper()
		before := db.ContentGeneration()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if after := db.ContentGeneration(); (after != before) != wantMoved {
			t.Errorf("%s: content generation %d -> %d, want moved %v", name, before, after, wantMoved)
		}
	}
	still := func(name string, f func() error) { t.Helper(); check(name, false, f) }
	moved := func(name string, f func() error) { t.Helper(); check(name, true, f) }
	exec := func(q string, args ...any) func() error {
		return func() error { _, err := db.ExecUserState(ctx, q, args...); return err }
	}
	// rollback runs q in a user-state transaction that then rolls back.
	boom := errors.New("roll back")
	rollback := func(q string, args ...any) func() error {
		return func() error {
			err := db.WithUserStateTx(ctx, "test", func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, q, args...); err != nil {
					return err
				}
				return boom
			})
			if errors.Is(err, boom) {
				return nil
			}
			return err
		}
	}
	saveProgress := `INSERT INTO progress(user_id, library_id, rel_path, position, duration, finished, playback_speed, version, device_id, updated_at)
		VALUES(1, 1, 'Book', ?, 100, 0, 1, 1, 0, 'now')
		ON CONFLICT(user_id, library_id, rel_path) DO UPDATE SET position = excluded.position`

	// A new progress row is content (Health's duplicates count listeners), an
	// update of its fields is not, and one moving it to another key is.
	moved("progress insert", exec(saveProgress, 1.0))
	still("progress update", exec(saveProgress, 2.0))
	still("progress update in a tx", func() error {
		return db.WithUserStateTx(ctx, "test", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, saveProgress, 3.0)
			return err
		})
	})
	moved("progress re-key", exec(`UPDATE progress SET rel_path = 'Moved' WHERE user_id = 1`))
	moved("progress re-key back", exec(`UPDATE progress SET rel_path = 'Book' WHERE user_id = 1`))
	still("bookmark insert", exec(`INSERT INTO bookmarks(user_id, library_id, rel_path, position, created_at) VALUES(1, 1, 'Book', 5, 'now')`))
	still("bookmark delete", exec(`DELETE FROM bookmarks WHERE user_id = 1`))
	still("token touch", exec(`UPDATE tokens SET last_seen = 'now', last_ip = '10.0.0.1', client_app = 'web' WHERE id = 1`))
	moved("token revoke", exec(`UPDATE tokens SET revoked = 1, last_seen = 'later' WHERE id = 1`))
	moved("token delete", exec(`DELETE FROM tokens WHERE id = 1`))
	still("unchanged heartbeat", exec(`UPDATE bridge_status SET heartbeat_at = 'now', status = '{}' WHERE id = 1`))
	moved("new heartbeat status", exec(`UPDATE bridge_status SET heartbeat_at = 'later', status = '{"version":"2"}' WHERE id = 1`))
	still("lease renewal", exec(`UPDATE bridge_jobs SET lease_until = 'soon', updated_at = 'now', state = 'running', progress = COALESCE(NULL, progress) WHERE id = 1`))
	moved("progress report", exec(`UPDATE bridge_jobs SET lease_until = 'later', progress = '{"step":2}' WHERE id = 1`))
	moved("job finished", exec(`UPDATE bridge_jobs SET state = 'done' WHERE id = 1`))
	// A default write moves it whatever it writes.
	moved("default progress update", func() error {
		_, err := db.ExecContext(ctx, saveProgress, 4.0)
		return err
	})
	moved("default tx", func() error {
		return db.WithTx(ctx, "test", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM bookmarks`)
			return err
		})
	})
	if len(*misfiled) != 0 {
		t.Fatalf("misfiled = %v, want none", *misfiled)
	}

	// Rolled back: a user-state transaction within its rules moves nothing; one
	// that changed content before rolling back still moves it (the check is on
	// what it changed); a default one moves it, as ever.
	still("user-state rollback", rollback(saveProgress, 9.0))
	moved("user-state rollback of content", rollback(`DELETE FROM progress`))
	moved("default rollback", func() error {
		_ = db.WithTx(ctx, "test", func(*sql.Tx) error { return boom })
		return nil
	})
	var pos float64
	if err := db.QueryRowContext(ctx, `SELECT position FROM progress`).Scan(&pos); err != nil || pos != 4 {
		t.Fatalf("progress after the rollbacks = %v, %v; want 4", pos, err)
	}
	// A failed user-state statement changes nothing and moves nothing.
	before := db.ContentGeneration()
	if _, err := db.ExecUserState(ctx, `UPDATE nowhere SET x = 1`); err == nil {
		t.Fatal("a statement on a missing table succeeded")
	}
	if db.ContentGeneration() != before {
		t.Fatal("a failed user-state statement moved the content generation")
	}
}

// TestUserStateMisfiled: a user-state write that changes a table its list
// doesn't name moves the content generation and is reported (a panic in a test
// binary without a recorder), in an exec and in a transaction.
func TestUserStateMisfiled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, misfiled := openUserState(t)
	before := db.ContentGeneration()
	if _, err := db.ExecUserState(ctx, `UPDATE users SET role = 'admin' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() == before || len(*misfiled) != 1 || (*misfiled)[0] != "users" {
		t.Fatalf("exec on users: generation %d -> %d, misfiled %v", before, db.ContentGeneration(), *misfiled)
	}
	before = db.ContentGeneration()
	err := db.WithUserStateTx(ctx, "test", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, created_at) VALUES(1, 1, 'B', 1, 'now')`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO collections(user_id, name, created_at, updated_at) VALUES(1, 'c', 'now', 'now')`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() == before || len(*misfiled) != 2 || (*misfiled)[1] != "collections" {
		t.Fatalf("tx on collections: generation %d -> %d, misfiled %v", before, db.ContentGeneration(), *misfiled)
	}
	// A foreign key's action is seen too: with a user's own row let through,
	// deleting one is content and a misfiling only through what it cascades to
	// (the collection made above, the token).
	db.userState["users"] = tableRule{rows: true}
	before = db.ContentGeneration()
	*misfiled = nil
	if _, err := db.ExecUserState(ctx, `DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() == before || !slices.Equal(*misfiled, []string{"collections"}) {
		t.Fatalf("cascading delete: generation %d -> %d, misfiled %v", before, db.ContentGeneration(), *misfiled)
	}

	// Without a recorder, a test binary panics once the write has finished.
	db.onMisfiled = nil
	func() {
		defer func() {
			r := recover()
			if s, _ := r.(string); !strings.Contains(s, "libraries") {
				t.Fatalf("recovered %v, want a panic naming libraries", r)
			}
		}()
		_, _ = db.ExecUserState(ctx, `INSERT INTO libraries(name, root, created_at) VALUES('l', '/x', 'a')`)
	}()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM libraries WHERE name = 'l'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("libraries after the panic = %d, %v; want the write kept", n, err)
	}
	// The writer is usable afterwards.
	before = db.ContentGeneration()
	if _, err := db.ExecContext(ctx, `INSERT INTO libraries(name, root, created_at) VALUES('m', '/y', 'a')`); err != nil || db.ContentGeneration() == before {
		t.Fatalf("a write after the panic = %v (generation %d -> %d)", err, before, db.ContentGeneration())
	}
}

// TestUserStateWatchUnset: the hook sees the connection's writes while set
// (allowed) and none once unset (denied), so a later write on the writer's
// connection never reaches a finished watch.
func TestUserStateWatchUnset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := openUserState(t)
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	w := &userStateWatch{rules: db.userState}
	if !w.set(conn) {
		t.Fatal("the hook could not be set")
	}
	write := func(name string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `INSERT INTO libraries(name, root, created_at) VALUES(?, '/w', 'a')`, name); err != nil {
			t.Fatal(err)
		}
	}
	write("seen")
	if !w.content || w.misfiled != "libraries" {
		t.Fatalf("while set: content %v, misfiled %q", w.content, w.misfiled)
	}
	*w = userStateWatch{rules: db.userState}
	w.unset(conn)
	write("unseen")
	if w.content || w.misfiled != "" {
		t.Fatalf("after unset: content %v, misfiled %q", w.content, w.misfiled)
	}
}

// TestUserStateRulesMatchTheSchema: every rule names a table the migrations
// created and columns it has (Open takes a schema without them, a newer
// server's, as more content), and a column rule leaves some column watched.
func TestUserStateRulesMatchTheSchema(t *testing.T) {
	t.Parallel()
	db, _ := openUserState(t)
	for table, rule := range userStateTables {
		cols, err := tableColumns(context.Background(), db.writer, table)
		if err != nil || len(cols) == 0 {
			t.Errorf("%s: columns %v, %v", table, cols, err)
			continue
		}
		for _, c := range rule.free {
			if !slices.Contains(cols, c) {
				t.Errorf("%s: no column %s", table, c)
			}
		}
		if got := db.userState[table]; rule.free != nil && len(got.watched) == 0 {
			t.Errorf("%s: no watched columns", table)
		}
	}
}

// TestUserStateRulesOnANewerSchema: a database a newer server changed (a
// rollback) still opens, and what a rule no longer names is content: a freed
// column renamed is watched, a column rule's dropped table has no rule.
func TestUserStateRulesOnANewerSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE bridge_jobs RENAME COLUMN lease_until TO lease_ends`,
		`DROP TABLE bridge_status`,
		`INSERT INTO bridge_jobs(id, kind, dedupe_key, state, created_at, updated_at) VALUES(1, 'bridge.verify', 'k', 'running', 'a', 'a')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = db.Close()
	if db, err = Open(ctx, path); err != nil {
		t.Fatalf("reopening a newer schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, ok := db.userState["bridge_status"]; ok {
		t.Error("a dropped table kept its rule")
	}
	before := db.ContentGeneration()
	if _, err := db.ExecUserState(ctx, `UPDATE bridge_jobs SET lease_ends = 'soon' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() == before {
		t.Fatal("a renamed freed column's change stayed user state")
	}
}
