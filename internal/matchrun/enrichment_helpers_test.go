package matchrun

import (
	"context"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// storeEnrichment states a book_enrichment row as the retired admin enrichment
// PUT (v2.7.0) left one, ISBN included: such rows still apply, from the next
// time the book is indexed (UpsertBook re-resolves it), which the caller does.
func storeEnrichment(t *testing.T, db *store.DB, libraryID int64, path, asin, isbn string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `INSERT INTO book_enrichment(library_id, path, asin, isbn, updated_at)
		VALUES(?,?,?,?,'2026-01-01T00:00:00Z')`, libraryID, path, asin, isbn); err != nil {
		t.Fatal(err)
	}
}
