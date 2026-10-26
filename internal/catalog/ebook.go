package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// A book's ebook (Bridge Phase 3b, migration 0056): the one .epub beside its
// audio that the reader opens, found by library.BookEbook's rule. It is index
// data like cover_path, derived from the files and never parsed: books.ebook_path
// is the readable file's library-relative path ('' for none) and books.ebook_sig
// the signature of the file the rule names (EbookSig, '' for none), which the scan
// compares to record a new, changed or gone ebook without re-indexing the book.

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
	Path   string
	Sig    string
	Unread bool
	Was    *EbookState
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
// change even when its size and time are kept. The size leads (ebookOf reads it).
func EbookSig(name string, info fs.FileInfo) string {
	return fmt.Sprintf("%d:%d:%s", info.Size(), info.ModTime().UnixNano(), name)
}

// ebookOf is the wire Ebook of a stored row (ebook_path, ebook_sig): nil when the
// book has none, else the path with the size its signature holds.
func ebookOf(path, sig string) *Ebook {
	if path == "" {
		return nil
	}
	head, _, _ := strings.Cut(sig, ":")
	size, _ := strconv.ParseInt(head, 10, 64)
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
			q, args := `UPDATE books SET ebook_path = ?, ebook_sig = ? WHERE library_id = ? AND rel_path = ?`,
				[]any{r.Path, r.Sig, libraryID, relPath}
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
