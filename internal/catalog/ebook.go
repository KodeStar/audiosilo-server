package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
)

// A book's ebook (Bridge Phase 3b, migration 0056): the one .epub beside its
// audio that the reader opens, found by library.BookEbook's rule. It is index
// data like cover_path, derived from the files and never parsed: books.ebook_path
// is the readable file's library-relative path ('' for none) and books.ebook_sig
// the signature of the file the rule names (EbookSig, '' for none), which the scan
// compares to record a new, changed or gone ebook without re-indexing the book;
// books.ebook_size its size (0058, server#228: its own column, no longer read
// back out of the signature).

// Ebook is a book's ebook on the wire (Book.Ebook, omitted when it has none):
// the file's library-relative path and its size in bytes when last scanned. A
// client reads it through GET /libraries/{id}/ebook?path=<the book's path>,
// never by this path.
type Ebook struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// EbookReading is a book's ebook as a scan records it (books.ebook_path,
// ebook_sig): Sig the signature of the file BookEbook's rule names ("" for none),
// Path that file when it is a readable epub ("" when none or unreadable). Unread:
// it couldn't be read right now, so UpsertBook keeps what is stored (a new row
// gets none, which the next scan reads again). Was is for SetEbooks.
type EbookReading struct {
	Path string
	Sig  string
	// Size is the file's size in bytes (books.ebook_size, Ebook.Size), 0 when
	// Path is "".
	Size   int64
	Unread bool
	Was    *EbookState
}

// ebookSize is books.ebook_size for r: its Size, 0 for no readable ebook.
func (r EbookReading) ebookSize() int64 {
	if r.Path == "" {
		return 0
	}
	return r.Size
}

// Ebook is the wire Ebook of r once recorded: nil for no readable ebook.
func (r EbookReading) Ebook() *Ebook { return ebookOf(r.Path, r.ebookSize()) }

// EbookStateAt is the ebook state of the indexed book at bookPath (its
// ebook_sig and indexed_at): what a reading of its ebook taken now is recorded
// over (SetEbooks with Was). ErrNotFound when no book is indexed there.
func (c *Catalog) EbookStateAt(ctx context.Context, libraryID int64, bookPath string) (EbookState, error) {
	var st EbookState
	err := c.db.QueryRowContext(ctx, `SELECT ebook_sig, indexed_at FROM books WHERE library_id = ? AND rel_path = ?`,
		libraryID, bookPath).Scan(&st.Sig, &st.IndexedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, ErrNotFound
	}
	return st, err
}

// EbookState is a book row as a scan read it (Signature.EbookSig and IndexedAt)
// before taking an EbookReading: SetEbooks records the reading only while the row
// still holds it (compare and set), so a reading an index of the book recorded
// since (on demand: the console's Rescan book, the Bridge's IndexMoved), newer
// than the scan's walk, stays.
type EbookState struct {
	Sig       string
	IndexedAt string
}

// EbookSig is the signature of the ebook file name (its base name) with info:
// its size, its modification time and its name, so a renamed file reads as a
// change even when its size and time are kept.
func EbookSig(name string, info fs.FileInfo) string {
	return fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), name)
}

// ebookOf is the wire Ebook of a stored row (ebook_path, ebook_size): nil when
// the book has none.
func ebookOf(path string, size int64) *Ebook {
	if path == "" {
		return nil
	}
	return &Ebook{Path: path, Size: size}
}

// SetEbooks records, by book path, the ebook a scan found for books it left
// unchanged (a new, changed or gone .epub beside unchanged audio), in one
// transaction, with no re-index. A reading with Was is recorded only while the
// row still holds that state (compare and set), else skipped: the book was
// indexed since the scan read it, recording a newer reading.
func (c *Catalog) SetEbooks(ctx context.Context, libraryID int64, readings map[string]EbookReading) error {
	if len(readings) == 0 {
		return nil
	}
	return c.db.WithTx(ctx, "SetEbooks", func(tx *sql.Tx) error {
		for relPath, r := range readings {
			q, args := `UPDATE books SET ebook_path = ?, ebook_sig = ?, ebook_size = ? WHERE library_id = ? AND rel_path = ?`,
				[]any{r.Path, r.Sig, r.ebookSize(), libraryID, relPath}
			if r.Was != nil {
				q += ` AND ebook_sig = ? AND indexed_at = ?`
				args = append(args, r.Was.Sig, r.Was.IndexedAt)
			}
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return err
			}
		}
		return nil
	})
}

// BookEbookAt is the path of the indexed book relPath addresses (BookPathAt's
// rule: the book at it, else the innermost folder book holding it) and whether
// that book has an ebook, from the index alone: the reader's progress routes
// need no more than that, not the whole book. ErrNotFound when no indexed book
// is there (the caller may index it on demand).
func (c *Catalog) BookEbookAt(ctx context.Context, libraryID int64, relPath string) (bookPath string, hasEbook bool, err error) {
	if bookPath, err = c.BookPathAt(ctx, libraryID, CleanRelPath(relPath)); err != nil {
		return "", false, err
	}
	err = c.db.QueryRowContext(ctx, `SELECT ebook_path != '' FROM books WHERE library_id = ? AND rel_path = ?`,
		libraryID, bookPath).Scan(&hasEbook)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	return bookPath, hasEbook, err
}
