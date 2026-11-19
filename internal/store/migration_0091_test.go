package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0091MovesBooksWithNoMP3ToRevision2: a book with no MP3 at the
// lengths' revision 1, its own, moves to 2 with its indexed_at kept, so the
// scan never looks for its MP3 parts; a book holding an MP3 stays where it is,
// so the next scan reads it once more (server#452), and so does a row an older
// server re-indexed since (its revision not its own). Every row starts with no
// failed tag read (server#318).
func TestMigration0091MovesBooksWithNoMP3ToRevision2(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0091")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, title, format, indexed_at, duration_rev, duration_rev_indexed_at) VALUES
		(1, 1, 'Loose.mp3', 'Loose MP3', 'mp3', 'i1', 1, 'i1'),
		(2, 1, 'Loose.m4b', 'Loose M4B', 'm4b', 'i2', 1, 'i2'),
		(3, 1, 'Mixed', 'Mixed', 'm4b', 'i3', 1, 'i3'),
		(4, 1, 'Parts', 'Parts', 'm4a', 'i4', 1, 'i4'),
		(5, 1, 'Stale.m4b', 'Stale', 'm4b', 'i5', 1, 'older'),
		(6, 1, 'Uncounted.mp3', 'Uncounted', 'mp3', 'i6', 0, 'i6')`)
	exec(`INSERT INTO book_files(book_id, rel_path, seq, format) VALUES
		(3, 'Mixed/01.m4b', 0, 'm4b'), (3, 'Mixed/02.mp3', 1, 'mp3'), (4, 'Parts/01.m4a', 0, 'm4a')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT rel_path || ' ' || duration_rev || ' ' || COALESCE(duration_rev_indexed_at, '-')
		|| ' ' || COALESCE(tag_read_failed_at, '-') FROM books ORDER BY rel_path`)
	want := []string{"Loose.m4b 2 i2 -", "Loose.mp3 1 i1 -", "Mixed 1 i3 -", "Parts 2 i4 -", "Stale.m4b 1 older -", "Uncounted.mp3 0 i6 -"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("revisions:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
