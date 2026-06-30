package catalog

import (
	"context"
	"strings"
)

// SetEnrichment attaches durable, path-keyed metadata (ASIN/ISBN) to a book and
// applies it to the current index row immediately. The book_enrichment table is not
// FK'd to the rebuildable books index, so the enrichment survives a re-scan — see
// ApplyEnrichments, which the scanner calls to re-apply it onto freshly-indexed
// books. A blank field leaves any existing enrichment for that field untouched.
func (c *Catalog) SetEnrichment(ctx context.Context, libraryID int64, path, asin, isbn string) error {
	asin, isbn = strings.TrimSpace(asin), strings.TrimSpace(isbn)
	if asin == "" && isbn == "" {
		return nil
	}
	// CASE WHEN keeps an existing stored value when the incoming one is blank.
	if _, err := c.db.ExecContext(ctx,
		`INSERT INTO book_enrichment(library_id, path, asin, isbn, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     asin = CASE WHEN excluded.asin <> '' THEN excluded.asin ELSE book_enrichment.asin END,
		     isbn = CASE WHEN excluded.isbn <> '' THEN excluded.isbn ELSE book_enrichment.isbn END,
		     updated_at = excluded.updated_at`,
		libraryID, path, asin, isbn, c.ts()); err != nil {
		return err
	}
	// Apply to the live index row now, so the change shows without waiting for a scan.
	_, err := c.db.ExecContext(ctx,
		`UPDATE books SET
		     asin = CASE WHEN ? <> '' THEN ? ELSE asin END,
		     isbn = CASE WHEN ? <> '' THEN ? ELSE isbn END
		 WHERE library_id = ? AND rel_path = ?`,
		asin, asin, isbn, isbn, libraryID, path)
	return err
}

// ApplyEnrichments re-applies stored enrichment onto a library's books, filling in
// only the non-empty enrichment fields. The scanner calls it after a scan so an
// ASIN/ISBN attached earlier survives the index being rebuilt from the filesystem.
func (c *Catalog) ApplyEnrichments(ctx context.Context, libraryID int64) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE books SET
		     asin = COALESCE((SELECT NULLIF(e.asin, '') FROM book_enrichment e
		                      WHERE e.library_id = books.library_id AND e.path = books.rel_path), asin),
		     isbn = COALESCE((SELECT NULLIF(e.isbn, '') FROM book_enrichment e
		                      WHERE e.library_id = books.library_id AND e.path = books.rel_path), isbn)
		 WHERE library_id = ? AND rel_path IN (SELECT path FROM book_enrichment WHERE library_id = ?)`,
		libraryID, libraryID)
	return err
}
