package store

import (
	"context"
	"testing"
)

// TestMigration0082StartsBooksWithNoMP3AtRevision1: a book with no MP3, its
// own format or a part's, starts at the lengths' revision 1 with its own
// indexed_at, so the scan never looks for its MP3 parts again; a book holding
// an MP3 starts at 0 with none, so the next scan reads it once more (SRV-16).
func TestMigration0082StartsBooksWithNoMP3AtRevision1(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0082")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, title, format, indexed_at) VALUES
		(1, 1, 'Loose.mp3', 'Loose MP3', 'mp3', 'i1'),
		(2, 1, 'Loose.m4b', 'Loose M4B', 'm4b', 'i2'),
		(3, 1, 'Mixed', 'Mixed', 'm4b', 'i3'),
		(4, 1, 'Parts', 'Parts', 'm4a', 'i4')`)
	exec(`INSERT INTO book_files(book_id, rel_path, seq, format) VALUES
		(3, 'Mixed/01.m4b', 0, 'm4b'), (3, 'Mixed/02.mp3', 1, 'mp3'), (4, 'Parts/01.m4a', 0, 'm4a')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	type rev struct {
		rev int
		at  string
	}
	want := map[string]rev{
		"Loose.mp3": {0, ""}, "Mixed": {0, ""},
		"Loose.m4b": {1, "i2"}, "Parts": {1, "i4"},
	}
	rows, err := db.QueryContext(ctx, `SELECT rel_path, duration_rev, COALESCE(duration_rev_indexed_at, '') FROM books`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var p string
		var got rev
		if err := rows.Scan(&p, &got.rev, &got.at); err != nil {
			t.Fatal(err)
		}
		n++
		if got != want[p] {
			t.Errorf("%s: revision %+v, want %+v", p, got, want[p])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(want) {
		t.Errorf("%d books, want %d", n, len(want))
	}
}
