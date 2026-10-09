package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0031Timestamps: the bookmarks, notes and listening spans stored
// before 0031 take the fixed-width UTC millisecond form the all-books lists order
// by as text (a trimmed RFC3339Nano, an offset); a value that isn't a date, a bare
// number included, stays as it was.
func TestMigration0031Timestamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0031")
	exec(`INSERT INTO users(id, username, password_hash, created_at, updated_at) VALUES(1, 'u', '', 't', 't')`)
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO bookmarks(user_id, library_id, rel_path, position, note, created_at) VALUES
		(1, 1, 'A', 1, '', '2026-10-07T12:00:00Z'), (1, 1, 'B', 1, '', '2026-10-07T12:00:00.5Z')`)
	exec(`INSERT INTO notes(user_id, library_id, rel_path, position, body, created_at, updated_at) VALUES
		(1, 1, 'A', 0, 'n', '2026-10-07T12:00:00.123456789Z', 'kept')`)
	exec(`INSERT INTO listening_history(user_id, library_id, rel_path, from_pos, to_pos, started_at, ended_at) VALUES
		(1, 1, 'A', 0, 1, '2026-10-07T14:00:00+02:00', '1700000000'), (1, 1, 'B', 0, 1, 'yesterday', '2026-10-07T12:30:00.000Z')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, c := range []struct{ q, want string }{
		{`SELECT created_at FROM bookmarks ORDER BY rel_path`, "2026-10-07T12:00:00.000Z\n2026-10-07T12:00:00.500Z"},
		{`SELECT created_at || '|' || updated_at FROM notes`, "2026-10-07T12:00:00.123Z|kept"},
		{`SELECT started_at || '|' || ended_at FROM listening_history ORDER BY rel_path`,
			"2026-10-07T12:00:00.000Z|1700000000\nyesterday|2026-10-07T12:30:00.000Z"},
	} {
		if got := strings.Join(lines(t, db, c.q), "\n"); got != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.q, got, c.want)
		}
	}
}
