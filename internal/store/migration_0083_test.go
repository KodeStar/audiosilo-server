package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigration0083AddsPathKeys (server#210): on a database a v2.6 server left,
// books with NFC, NFD and ASCII names keep their paths byte for byte and start
// with no key (NULL: catalog.KeyPaths keys them at the start).
func TestMigration0083AddsPathKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0083")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	// Explicit code points: NFC "É" precomposed, NFD "E" + U+0301.
	exec(`INSERT INTO books(id, library_id, rel_path, title, indexed_at) VALUES
		(1, 1, ?, 'NFC', 'i'), (2, 1, ?, 'NFD', 'i'), (3, 1, 'Plain/Book', 'ASCII', 'i')`,
		"Émile/Germinal", "Émile/Germinal")
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	got := lines(t, db, `SELECT hex(rel_path) || ' ' || COALESCE(path_key, 'NULL') FROM books ORDER BY id`)
	want := []string{"C3896D696C652F4765726D696E616C NULL", "45CC816D696C652F4765726D696E616C NULL",
		"506C61696E2F426F6F6B NULL"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("books after 0083:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
