package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0087RedatesBooksAddedBefore1980: a book a scan stamped as added
// before 1980 (a birth time of 0 over an rclone NFS share: 1970) takes its
// mtime as the scan now would, or with no mtime after 1980 when it was last
// indexed; every other book keeps its date (server#360).
func TestMigration0087RedatesBooksAddedBefore1980(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0087")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, title, mtime, indexed_at, added_at) VALUES
		(1, 1, 'Epoch', 'Epoch', 1700000000, '2026-11-01T10:00:00.123456789Z', '1970-01-01T00:00:00Z'),
		(2, 1, 'Early', 'Early', 1700000000, '2026-11-01T10:00:00Z', '1979-12-31T23:59:59Z'),
		(3, 1, 'No mtime', 'No mtime', 0, '2026-11-02T08:30:15.5Z', '1970-01-01T00:00:00Z'),
		(4, 1, 'Fine', 'Fine', 1700000000, '2026-11-01T10:00:00Z', '2024-05-06T07:08:09Z'),
		(5, 1, '1980', '1980', 1700000000, '2026-11-01T10:00:00Z', '1980-01-01T00:00:00Z')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT rel_path || ' ' || added_at FROM books ORDER BY id`)
	want := []string{
		"Epoch 2023-11-14T22:13:20Z",
		"Early 2023-11-14T22:13:20Z",
		"No mtime 2026-11-02T08:30:15Z",
		"Fine 2024-05-06T07:08:09Z",
		"1980 1980-01-01T00:00:00Z",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("added dates:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
