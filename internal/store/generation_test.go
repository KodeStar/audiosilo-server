package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
)

// TestGenerationMovesAfterEveryWrite: every write path moves the generation once
// it has finished (a failed or rolled-back one too), never before its commit;
// reads never do (api's list cache relies on both).
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

	// It moves only once the write has committed: at the COMMIT itself (the
	// driver's commit hook on the one writer connection) it is still the old one.
	var atCommit []uint64
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(dc any) error {
		dc.(sqlite.HookRegisterer).RegisterCommitHook(func() int32 {
			atCommit = append(atCommit, db.Generation())
			return 0
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		name  string
		write func() error
	}{
		{"exec", func() error {
			_, err := db.ExecContext(ctx, `INSERT INTO t VALUES (4)`)
			return err
		}},
		{"commit", func() error {
			return db.WithTx(ctx, "test", func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO t VALUES (5)`)
				return err
			})
		}},
	} {
		before := db.Generation()
		atCommit = nil
		if err := w.write(); err != nil {
			t.Fatal(err)
		}
		if after := db.Generation(); len(atCommit) != 1 || atCommit[0] != before || after == before {
			t.Errorf("%s: generation %d at the COMMIT (%v), %d after; want %d, then moved", w.name, before, atCommit, after, before)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 4 {
		t.Fatalf("rows after the moves = %d, %v; want 4", n, err)
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
