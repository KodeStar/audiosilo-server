package store

import (
	"context"
	"testing"
)

// TestMigration0067TagHints: the books gain empty hint columns and are marked
// unchecked, so the next scan reads their tags once; their identifiers are
// untouched.
func TestMigration0067TagHints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0067")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, author, series, narrator, indexed_at, asin)
	      VALUES(7, 1, 'Herbert/Dune', 1, 'Dune', 'Frank Herbert', '', '', 'x', 'B0OWN00001')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var asin, tagASIN, tagISBN, tagLanguage string
	var checked int
	if err := db.writer.QueryRow(`SELECT asin, tag_asin, tag_isbn, tag_language, hints_checked FROM books WHERE id = 7`).
		Scan(&asin, &tagASIN, &tagISBN, &tagLanguage, &checked); err != nil {
		t.Fatal(err)
	}
	if asin != "B0OWN00001" || tagASIN != "" || tagISBN != "" || tagLanguage != "" || checked != 0 {
		t.Errorf("after 0067: asin %q, hints %q %q %q, checked %d; want the ASIN kept, empty hints, unchecked",
			asin, tagASIN, tagISBN, tagLanguage, checked)
	}
}
