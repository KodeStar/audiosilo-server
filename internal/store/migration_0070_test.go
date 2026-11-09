package store

import (
	"context"
	"testing"
)

// TestMigration0070BookMatchOffered: an existing database's recorded matches
// gain offered_asin and offered_isbn, and its runs' items gain unchanged, empty
// for every row 0069 left (no backfill); a new row may name them.
func TestMigration0070BookMatchOffered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0070")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO book_matches(library_id, path, work_id, recording_id, how, run_id, set_by, set_at)
	      VALUES(1, 'A/Book', 'the-work', 'the-rec', 'accepted', 4, 2, 't')`)
	exec(`INSERT INTO match_runs(id, mode, status, started_at) VALUES(1, 'refresh', 'ready', 't')`)
	exec(`INSERT INTO match_run_items(run_id, library_id, path, outcome) VALUES(1, 1, 'A/Book', 'auto')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var workID, how, asin, isbn string
	if err := db.writer.QueryRow(`SELECT work_id, how, offered_asin, offered_isbn FROM book_matches
	     WHERE library_id = 1 AND path = 'A/Book'`).Scan(&workID, &how, &asin, &isbn); err != nil {
		t.Fatal(err)
	}
	if workID != "the-work" || how != "accepted" || asin != "" || isbn != "" {
		t.Fatalf("after 0070: work %q how %q offered %q/%q; want the row kept, offered ''", workID, how, asin, isbn)
	}
	if _, err := db.writer.Exec(`INSERT INTO book_matches(library_id, path, work_id, how, set_at, offered_asin, offered_isbn)
	     VALUES(1, 'A/Other', 'w', 'auto', 't', 'B000000001', '9780000000002')`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT offered_asin, offered_isbn FROM book_matches WHERE path = 'A/Other'`).
		Scan(&asin, &isbn); err != nil || asin != "B000000001" || isbn != "9780000000002" {
		t.Fatalf("a new row's offered identifiers: %q/%q, %v", asin, isbn, err)
	}

	var outcome, unchanged string
	if err := db.writer.QueryRow(`SELECT outcome, unchanged FROM match_run_items WHERE path = 'A/Book'`).
		Scan(&outcome, &unchanged); err != nil {
		t.Fatal(err)
	}
	if outcome != "auto" || unchanged != "" {
		t.Fatalf("after 0070: item outcome %q unchanged %q; want the row kept, unchanged ''", outcome, unchanged)
	}
	if _, err := db.writer.Exec(`INSERT INTO match_run_items(run_id, library_id, path, outcome, unchanged)
	     VALUES(1, 1, 'A/Other', 'auto', 'fill')`); err != nil {
		t.Fatal(err)
	}
}
