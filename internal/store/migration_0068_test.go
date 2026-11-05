package store

import (
	"context"
	"slices"
	"testing"
)

// TestMigration0068UnknownASINReports: an existing database gains the empty
// report table, keyed by the ASIN alone, with no column naming a book.
func TestMigration0068UnknownASINReports(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0068")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, author, series, narrator, indexed_at, asin)
	      VALUES(7, 1, 'Herbert/Dune', 1, 'Dune', 'Frank Herbert', '', '', 'x', 'B0OWN00001')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM unknown_asin_reports`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("after 0068: %d rows, %v; want an empty table", n, err)
	}
	rows, err := db.writer.Query(`SELECT name FROM pragma_table_info('unknown_asin_reports') ORDER BY cid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"asin", "checked_at", "unknown", "sent_at"}; !slices.Equal(cols, want) {
		t.Fatalf("columns = %v, want %v", cols, want)
	}
	const insert = `INSERT INTO unknown_asin_reports(asin, checked_at) VALUES('B0ABCDEFGH', 't')`
	if _, err := db.writer.Exec(insert); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(insert); err == nil {
		t.Fatal("a second row for one ASIN was accepted")
	}
}
