package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0023Backfill: every book already indexed gets its cover art
// identity (a custom cover's stamp, else its mtime, size and sidecar), so the
// books of an upgraded server carry a cover_version before they are re-indexed;
// no book has a colour yet.
func TestMigration0023Backfill(t *testing.T) {
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0023")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	book := func(path, cover string, mtime, size int64) {
		exec(`INSERT INTO books(library_id, rel_path, title, cover_path, mtime, size, indexed_at)
			VALUES(1, ?, 'T', ?, ?, ?, 't')`, path, cover, mtime, size)
	}
	book("A", "A/cover.jpg", 100, 2000)
	book("B", "", 7, 8)
	exec(`INSERT INTO book_covers(library_id, path, mime, data, updated_at) VALUES(1, 'B', 'image/png', x'00', '2026-01-02T03:04:05Z')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT rel_path || '|' || cover_art || '|' || cover_color FROM books ORDER BY rel_path`)
	want := []string{"A|f100 2000 A/cover.jpg|", "B|c2026-01-02T03:04:05Z|"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("books:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
