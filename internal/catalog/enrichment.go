package catalog

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// SetEnrichment attaches durable, path-keyed metadata (ASIN/ISBN) to a book and
// applies it to the current index row immediately. The book_enrichment table is not
// FK'd to the rebuildable books index, so the enrichment survives a re-scan:
// UpsertBook re-applies it (refreshEffective) whenever the book is re-indexed. A
// blank field leaves any existing enrichment for that field untouched, and an
// admin's override of asin/isbn still wins over it. No route writes through it
// since the admin enrichment PUT was retired (v2.7.0; the Bridge's backfill is
// BackfillASIN): rows already stored still apply, and tests state one with it.
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
	err := c.db.WithTx(ctx, "SetEnrichment", func(tx *sql.Tx) error {
		return setEnrichmentTx(ctx, tx, libraryID, path, asin, isbn, c.ts())
	})
	if err == nil {
		c.changed()
	}
	return err
}

// setEnrichmentTx is SetEnrichment inside tx: the durable row and the live
// index row in one transaction, so they never diverge (a failure after the
// upsert would otherwise store the enrichment but leave the books row stale
// until the next scan). path is clean; a blank field keeps what is stored.
func setEnrichmentTx(ctx context.Context, tx *sql.Tx, libraryID int64, path, asin, isbn, now string) error {
	// CASE WHEN keeps an existing stored value when the incoming one is blank.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO book_enrichment(library_id, path, asin, isbn, updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		     asin = CASE WHEN excluded.asin <> '' THEN excluded.asin ELSE book_enrichment.asin END,
		     isbn = CASE WHEN excluded.isbn <> '' THEN excluded.isbn ELSE book_enrichment.isbn END,
		     updated_at = excluded.updated_at`,
		libraryID, path, asin, isbn, now); err != nil {
		return err
	}
	// Apply the now-stored (merged) enrichment to the live index row, so the
	// change shows without waiting for a scan. Enrichment may precede the book
	// being indexed; the upsert that indexes it applies it then.
	bookID, err := bookIDByPath(ctx, tx, libraryID, path)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return refreshEffective(ctx, tx, bookID)
}
