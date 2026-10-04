package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigration0016Backfill: on a database from before 0016, the migration stamps
// each row's scanned snapshot with the row's indexed_at, re-applies an enrichment a
// stopped scan had left off its row (the scanner's old end-of-scan pass is gone),
// and turns an infinite series position, which no JSON reply can carry, into none.
func TestMigration0016Backfill(t *testing.T) {
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0016")
	exec(`INSERT INTO libraries(id, name, root, created_at) VALUES(1, 'L', '/l', 't')`)
	exec(`INSERT INTO books(library_id, rel_path, title, asin, indexed_at) VALUES(1, 'A/Enriched', 'Enriched', '', '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO book_enrichment(library_id, path, asin, isbn, updated_at) VALUES(1, 'A/Enriched', 'B000000001', '', 't')`)
	exec(`INSERT INTO books(library_id, rel_path, title, series_index, indexed_at) VALUES(1, 'A/Infinite', 'Infinite', 9e999, '2026-01-02T00:00:00Z')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var asin, stamp, indexedAt string
	if err := db.writer.QueryRow(`SELECT asin, json_extract(scanned, '$."@indexed_at"'), indexed_at
		FROM books WHERE rel_path = 'A/Enriched'`).Scan(&asin, &stamp, &indexedAt); err != nil {
		t.Fatal(err)
	}
	if asin != "B000000001" {
		t.Errorf("asin = %q, want the attached enrichment re-applied", asin)
	}
	if stamp != indexedAt {
		t.Errorf("scanned stamp = %q, want the row's indexed_at %q", stamp, indexedAt)
	}
	var idx float64
	if err := db.writer.QueryRow(`SELECT series_index FROM books WHERE rel_path = 'A/Infinite'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Errorf("series_index = %v, want 0 (no position)", idx)
	}
}

// openBefore makes a database at dsn with every migration before `before` applied
// (as an older server left it), for a test to fill before Open applies the rest.
// exec runs a statement on it; call closeRaw before Open.
func openBefore(t *testing.T, before string) (dsn string, exec func(q string, args ...any), closeRaw func()) {
	t.Helper()
	ctx := context.Background()
	dsn = filepath.Join(t.TempDir(), "pre-"+before+".db")
	raw, err := sql.Open("sqlite", dsnPragmas(dsn))
	if err != nil {
		t.Fatal(err)
	}
	exec = func(q string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", strings.SplitN(q, "\n", 2)[0], err)
		}
	}
	exec(`CREATE TABLE schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`)
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries { // sorted by name
		if e.Name() >= before {
			break
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		exec(string(body))
		exec(`INSERT INTO schema_migrations(name, applied_at) VALUES(?, 't')`, e.Name())
	}
	return dsn, exec, func() {
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
