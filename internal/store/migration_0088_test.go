package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0088AddsBookmarkClientID: the bookmarks a v2.6 server left keep
// their rows with no client id; any number of bookmarks may have none, a user
// holds an id once while another user's same id is theirs, and the replay's
// lookup by (user, id) is an index seek.
func TestMigration0088AddsBookmarkClientID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0088")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES
		(1, 'ann', 'x', 'user', 't', 't'), (2, 'bob', 'x', 'user', 't', 't')`)
	exec(`INSERT INTO bookmarks(user_id, library_id, rel_path, position, created_at) VALUES
		(1, 1, 'A/Book', 1, 't'), (1, 1, 'A/Book', 2, 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if got := lines(t, db, `SELECT COALESCE(client_id, 'NULL') FROM bookmarks ORDER BY id`); strings.Join(got, ",") != "NULL,NULL" {
		t.Fatalf("client ids after 0088 = %v, want none", got)
	}
	add := func(user int, clientID any) error {
		_, err := db.ExecContext(ctx, `INSERT INTO bookmarks(user_id, library_id, rel_path, position, created_at, client_id)
			VALUES(?, 1, 'A/Book', 3, 't', ?)`, user, clientID)
		return err
	}
	for _, c := range []struct {
		user     int
		clientID any
		ok       bool
	}{
		{1, nil, true}, {1, "a", true}, {2, "a", true}, {1, "b", true}, {1, "a", false}, {2, "a", false},
	} {
		if err := add(c.user, c.clientID); (err == nil) != c.ok {
			t.Errorf("user %d, client id %v: err %v, want ok=%v", c.user, c.clientID, err, c.ok)
		} else if err != nil && !strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("user %d, client id %v: err %v, want a UNIQUE refusal", c.user, c.clientID, err)
		}
	}
	var id, parent, unused int
	var plan string
	if err := db.writer.QueryRowContext(ctx, `EXPLAIN QUERY PLAN SELECT * FROM bookmarks
		WHERE user_id = 1 AND client_id = 'a'`).Scan(&id, &parent, &unused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_bookmarks_client_id") {
		t.Errorf("a replay's lookup: plan %q, want idx_bookmarks_client_id", plan)
	}
}
