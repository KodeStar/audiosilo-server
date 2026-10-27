package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestGenerationMovesAfterEveryWrite: every write path moves the generation once
// it has finished (a failed or rolled-back one too); reads never do, and the
// generation a write leaves is already visible to a reader (api's list cache
// relies on both).
func TestGenerationMovesAfterEveryWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "gen.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (v INTEGER)`); err != nil {
		t.Fatal(err)
	}
	moved := func(name string, f func()) {
		t.Helper()
		before := db.Generation()
		f()
		if after := db.Generation(); after <= before {
			t.Errorf("%s: generation %d -> %d, want it moved", name, before, after)
		}
	}
	moved("exec", func() {
		if _, err := db.ExecContext(ctx, `INSERT INTO t VALUES (1)`); err != nil {
			t.Fatal(err)
		}
	})
	moved("failed exec", func() { _, _ = db.ExecContext(ctx, `INSERT INTO missing VALUES (1)`) })
	moved("commit", func() {
		err := db.WithTx(ctx, "test", func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO t VALUES (2)`)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	moved("rollback", func() {
		_ = db.WithTx(ctx, "test", func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO t VALUES (3)`); err != nil {
				return err
			}
			return errors.New("roll back")
		})
	})

	// A commit is visible on the reader pool by the time its generation is.
	var gotGen uint64
	var n int
	err = db.WithTx(ctx, "test", func(tx *sql.Tx) error {
		gotGen = db.Generation()
		_, err := tx.ExecContext(ctx, `INSERT INTO t VALUES (4)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.Generation() == gotGen {
		t.Fatal("generation did not move past the commit")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("rows after the moves = %d, %v; want 3", n, err)
	}

	// Reads leave it where it is.
	g := db.Generation()
	for range 3 {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if db.Generation() != g {
		t.Fatalf("reads moved the generation: %d -> %d", g, db.Generation())
	}
}
