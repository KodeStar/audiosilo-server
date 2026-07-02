package catalog

import (
	"context"
	"database/sql"
	"strings"
)

// applyEnrichmentToPathSQL fills a single book row's non-empty enrichment fields
// from its stored book_enrichment row. NULLIF/COALESCE means a blank stored field
// leaves the indexed value untouched. Shared by SetEnrichment (live apply) and
// ApplyEnrichment (per-path restore) so the merge rule lives in one place.
const applyEnrichmentToPathSQL = `UPDATE books SET
	     asin = COALESCE((SELECT NULLIF(e.asin, '') FROM book_enrichment e
	                      WHERE e.library_id = books.library_id AND e.path = books.rel_path), asin),
	     isbn = COALESCE((SELECT NULLIF(e.isbn, '') FROM book_enrichment e
	                      WHERE e.library_id = books.library_id AND e.path = books.rel_path), isbn)
	 WHERE library_id = ? AND rel_path = ?`

// SetEnrichment attaches durable, path-keyed metadata (ASIN/ISBN) to a book and
// applies it to the current index row immediately. The book_enrichment table is not
// FK'd to the rebuildable books index, so the enrichment survives a re-scan - see
// ApplyEnrichments, which the scanner calls to re-apply it onto freshly-indexed
// books. A blank field leaves any existing enrichment for that field untouched.
func (c *Catalog) SetEnrichment(ctx context.Context, libraryID int64, path, asin, isbn string) error {
	// Canonicalize the key on write (see CleanRelPath) so it matches the
	// scanner's rel_path keys - a non-canonical key would never re-apply.
	path = CleanRelPath(path)
	asin, isbn = strings.TrimSpace(asin), strings.TrimSpace(isbn)
	if asin == "" && isbn == "" {
		return nil
	}
	// One transaction so the durable row and the live index row never diverge: a
	// failure after the upsert would otherwise store the enrichment but leave the
	// books row stale until the next scan.
	return c.db.WithTx(ctx, "SetEnrichment", func(tx *sql.Tx) error {
		// CASE WHEN keeps an existing stored value when the incoming one is blank.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO book_enrichment(library_id, path, asin, isbn, updated_at) VALUES(?,?,?,?,?)
			 ON CONFLICT(library_id, path) DO UPDATE SET
			     asin = CASE WHEN excluded.asin <> '' THEN excluded.asin ELSE book_enrichment.asin END,
			     isbn = CASE WHEN excluded.isbn <> '' THEN excluded.isbn ELSE book_enrichment.isbn END,
			     updated_at = excluded.updated_at`,
			libraryID, path, asin, isbn, c.ts()); err != nil {
			return err
		}
		// Apply the now-stored (merged) enrichment to the live index row, so the
		// change shows without waiting for a scan.
		_, err := tx.ExecContext(ctx, applyEnrichmentToPathSQL, libraryID, path)
		return err
	})
}

// ApplyEnrichment restores stored enrichment onto a single just-indexed book,
// filling in only the non-empty enrichment fields. The scanner calls it from
// IndexPath so an on-demand index of one path doesn't re-sweep the whole library
// (cf. ApplyEnrichments, the bulk form used after a full scan).
func (c *Catalog) ApplyEnrichment(ctx context.Context, libraryID int64, path string) error {
	_, err := c.db.ExecContext(ctx, applyEnrichmentToPathSQL, libraryID, path)
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
