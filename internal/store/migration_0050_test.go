package store

import (
	"context"
	"testing"
)

// TestMigration0050AttachmentsDescription: a book's stored attachments gain an
// empty description and keep what they had.
func TestMigration0050AttachmentsDescription(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0050")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, indexed_at, attachments_sig)
	      VALUES(7, 1, 'Pratchett/Mort', 1, 'Mort', 'x', 'characters.json:1:1')`)
	exec(`INSERT INTO book_attachments(book_id, characters, updated_at) VALUES(7, '{"characters":[]}', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var characters, description string
	if err := db.writer.QueryRow(`SELECT characters, description FROM book_attachments WHERE book_id = 7`).
		Scan(&characters, &description); err != nil {
		t.Fatal(err)
	}
	if characters != `{"characters":[]}` || description != "" {
		t.Errorf("row = %q %q, want the characters kept and no description", characters, description)
	}
}
