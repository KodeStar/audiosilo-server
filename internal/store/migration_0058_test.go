package store

import (
	"context"
	"testing"
)

// TestMigration0058Readalong: a recorded ebook's size moves into its own
// column (read from the signature, whose size leads), a book with none or an
// unreadable one keeps 0, nothing is hashed yet, and every attachments row
// starts with no read-along map.
func TestMigration0058Readalong(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0058")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'Books', '/l', 't')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, indexed_at, ebook_path, ebook_sig)
	      VALUES(7, 1, 'Austen/Persuasion', 1, 'Persuasion', 'x', 'Austen/Persuasion/p.epub', '1061234:1700000000:p.epub')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, indexed_at, ebook_path, ebook_sig)
	      VALUES(8, 1, 'Austen/Emma', 1, 'Emma', 'x', '', '99:1:broken.epub')`)
	exec(`INSERT INTO books(id, library_id, rel_path, is_folder, title, indexed_at)
	      VALUES(9, 1, 'Austen/Mansfield', 1, 'Mansfield', 'x')`)
	exec(`INSERT INTO book_attachments(book_id, characters, recaps, provenance, updated_at)
	      VALUES(7, '{"x":1}', '', '', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for id, want := range map[int]int64{7: 1061234, 8: 0, 9: 0} {
		var size int64
		var sha, hashed string
		if err := db.writer.QueryRow(`SELECT ebook_size, ebook_sha256, ebook_hashed_sig FROM books WHERE id = ?`, id).
			Scan(&size, &sha, &hashed); err != nil {
			t.Fatal(err)
		}
		if size != want || sha != "" || hashed != "" {
			t.Errorf("book %d = %d %q %q, want size %d and no digest", id, size, sha, hashed, want)
		}
	}
	var chars, readalong string
	if err := db.writer.QueryRow(`SELECT characters, readalong FROM book_attachments WHERE book_id = 7`).
		Scan(&chars, &readalong); err != nil {
		t.Fatal(err)
	}
	if chars != `{"x":1}` || readalong != "" {
		t.Errorf("attachments = %q %q, want the characters kept and no map", chars, readalong)
	}
}
