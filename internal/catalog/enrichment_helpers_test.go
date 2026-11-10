package catalog

import (
	"context"
	"database/sql"
	"testing"
)

// setEnrichment states a book_enrichment row as the retired admin
// enrichment PUT (v2.7.0) or the Bridge's backfill leaves one, applied
// to the live row (setEnrichmentTx, a blank field keeping what is stored).
func setEnrichment(t *testing.T, c *Catalog, libraryID int64, path, asin, isbn string) {
	t.Helper()
	ctx := context.Background()
	if err := c.db.WithTx(ctx, "test.setEnrichment", func(tx *sql.Tx) error {
		return setEnrichmentTx(ctx, tx, libraryID, CleanRelPath(path), asin, isbn, c.ts())
	}); err != nil {
		t.Fatal(err)
	}
}
