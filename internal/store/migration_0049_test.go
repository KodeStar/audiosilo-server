package store

import (
	"context"
	"testing"
)

// TestMigration0049BookLabels: the books gain the new columns empty (abridged
// unknown), every book is marked for its genre tag's read and every ABS file
// for a read again, and the full-text index made again with the label columns
// keeps every row it had, still searchable.
func TestMigration0049BookLabels(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0049")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, author, series, narrator, indexed_at, abs_indexed_at, released_checked)
	      VALUES(7, 1, 'Pratchett/Mort', 1, 'Mort', 'Terry Pratchett', 'Discworld', 'Nigel Planer', 'x', 'x', 1)`)
	exec(`INSERT INTO books_fts(rowid, title, author, series, narrator) VALUES(7, 'Mort', 'Terry Pratchett', 'Discworld
Death', 'Nigel Planer')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var (
		subtitle, publisher, language, genres, tags string
		abridged                                    *int
		checked                                     int
		absAt                                       *string
	)
	if err := db.writer.QueryRow(`SELECT subtitle, publisher, language, abridged, genres, tags, genres_checked, abs_indexed_at
		FROM books WHERE id = 7`).Scan(&subtitle, &publisher, &language, &abridged, &genres, &tags, &checked, &absAt); err != nil {
		t.Fatal(err)
	}
	if subtitle != "" || publisher != "" || language != "" || abridged != nil || genres != "" || tags != "" {
		t.Errorf("new columns = %q %q %q %v %q %q, want empty and unknown", subtitle, publisher, language, abridged, genres, tags)
	}
	if checked != 0 || absAt != nil {
		t.Errorf("genres_checked = %d, abs_indexed_at = %v; want 0 and NULL (read again)", checked, absAt)
	}
	var rowid int64
	var series, labelCols string
	if err := db.writer.QueryRow(`SELECT rowid, series, genres || tags FROM books_fts WHERE books_fts MATCH 'death'`).
		Scan(&rowid, &series, &labelCols); err != nil {
		t.Fatal(err)
	}
	if rowid != 7 || series != "Discworld\nDeath" || labelCols != "" {
		t.Errorf("fts row = %d %q %q, want the copied row", rowid, series, labelCols)
	}
	if _, err := db.writer.Exec(`INSERT INTO books_fts(rowid, title, author, series, narrator, genres, tags)
		VALUES(8, 'Guards', '', '', '', 'Fantasy', 'Owned')`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM books_fts WHERE books_fts MATCH 'tags: owned'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("a tag match = %d, %v; want 1", n, err)
	}
}
