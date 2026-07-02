package store

// Internal (package store) test: migrate() is unexported, so we live in the
// package to exercise it and the transactional migration runner directly.

import (
	"context"
	"path/filepath"
	"testing"
)

// countMigrations returns the number of recorded migrations.
func countMigrations(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	return n
}

// TestOpenAppliesMigrations opens a file-backed DB and asserts every embedded
// migration lands in schema_migrations.
func TestOpenAppliesMigrations(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if n := countMigrations(t, db); n <= 0 {
		t.Fatalf("schema_migrations rows = %d, want > 0 (migrations should have applied)", n)
	}
}

// TestMigrationsIdempotent re-opens the same file and re-runs migrate(),
// asserting no error and no change to the recorded migration count.
func TestMigrationsIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db")

	db1, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	first := countMigrations(t, db1)
	db1.Close()

	// Re-open the SAME file: migrate() runs again and must be a no-op.
	db2, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer db2.Close()

	second := countMigrations(t, db2)
	if second != first {
		t.Fatalf("schema_migrations count changed across reopen: first=%d second=%d (migrate must be idempotent)", first, second)
	}

	// An explicit extra migrate() call must also be a no-op (no error, same count).
	if err := db2.migrate(ctx); err != nil {
		t.Fatalf("re-running migrate(): %v", err)
	}
	if third := countMigrations(t, db2); third != first {
		t.Fatalf("schema_migrations count changed after explicit migrate(): want %d, got %d", first, third)
	}
}

// TestForeignKeysEnabled asserts the foreign_keys pragma is ON after Open - the
// schema relies on ON DELETE CASCADE rules that silently no-op when it is off.
func TestForeignKeysEnabled(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Check the writer connection: cascade deletes run on it, so its
	// foreign_keys pragma is the one that matters.
	var on int
	if err := db.writer.QueryRow(`PRAGMA foreign_keys`).Scan(&on); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if on != 1 {
		t.Fatalf("PRAGMA foreign_keys = %d, want 1 (cascade deletes depend on it)", on)
	}
}

// TestMigrationRunnerRollsBackOnBadBody exercises the lower-level transactional
// guarantee the migration runner relies on: a migration body that fails partway
// must leave no partial schema behind. The embedded migrations can't be
// corrupted from a test, so this drives the same begin/exec/rollback pattern
// migrate() uses (apply a body in a tx; on error, Rollback) with a body whose
// second statement is invalid.
func TestMigrationRunnerRollsBackOnBadBody(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "test.db")

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// A two-statement "migration": the first creates a table, the second is
	// invalid SQL. The runner wraps the whole body in one tx and rolls back on
	// the exec error, so the table from the first statement must not survive.
	badBody := `CREATE TABLE rollback_probe(id INTEGER PRIMARY KEY);
		THIS IS NOT VALID SQL;`

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, err := tx.ExecContext(ctx, badBody); err == nil {
		tx.Rollback()
		t.Fatal("bad migration body executed without error; expected a syntax error")
	} else {
		// Mirror migrate(): on error, roll the whole transaction back.
		if rbErr := tx.Rollback(); rbErr != nil {
			t.Fatalf("Rollback: %v", rbErr)
		}
	}

	// The partial work (the rollback_probe table) must have been discarded.
	var name string
	err = db.writer.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='rollback_probe'`,
	).Scan(&name)
	if err == nil {
		t.Fatalf("rollback_probe table survived a failed migration; transactional rollback did not protect the schema")
	}
}
