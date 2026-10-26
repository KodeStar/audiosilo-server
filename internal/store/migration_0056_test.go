package store

import (
	"context"
	"testing"
)

// TestMigration0056BookEbook: every book indexed before it holds no ebook and
// no signature (the next scan fills them in) and keeps what it had.
func TestMigration0056BookEbook(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0056")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, indexed_at)
	      VALUES(7, 1, 'Austen/Persuasion', 1, 'Persuasion', 'x')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var title, path, sig string
	if err := db.writer.QueryRow(`SELECT title, ebook_path, ebook_sig FROM books WHERE id = 7`).
		Scan(&title, &path, &sig); err != nil {
		t.Fatal(err)
	}
	if title != "Persuasion" || path != "" || sig != "" {
		t.Errorf("row = %q %q %q, want the title kept and no ebook", title, path, sig)
	}
}
