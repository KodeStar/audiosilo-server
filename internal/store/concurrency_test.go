package store

// Internal (package store) tests for the reader/writer pool split and the WithTx
// helper. These use a file-backed DB (t.TempDir) on purpose: an in-memory DB is
// per-connection, so the reader pool only exists for a file-backed database.

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestReaderServesReadsDuringWriteTx is the core guarantee of the split: a read
// must not block while a write transaction holds the single writer connection.
// Under the old single-pool model (MaxOpenConns(1)) this read would queue behind
// the open transaction and time out.
func TestReaderServesReadsDuringWriteTx(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Hold the single writer connection open in a transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations(name, applied_at) VALUES('zzz_probe', 'now')`); err != nil {
		t.Fatalf("write in tx: %v", err)
	}

	// A read on the reader pool must complete promptly despite the held writer.
	done := make(chan error, 1)
	go func() {
		var n int
		done <- db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read during write tx: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("read blocked while a write transaction held the writer — reader pool is not isolating reads")
	}
}

// TestWithTxCommitsAndRollsBack verifies WithTx commits on success and rolls back
// on a returned error (leaving no partial writes).
func TestWithTxCommitsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `CREATE TABLE probe (x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Commit path.
	if err := db.WithTx(ctx, "insert-ok", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO probe(x) VALUES(1)`)
		return e
	}); err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}

	// Rollback path: a returned error must discard the write and propagate.
	sentinel := errors.New("boom")
	if err := db.WithTx(ctx, "insert-fail", func(tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT INTO probe(x) VALUES(2)`); e != nil {
			return e
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error path: want sentinel, got %v", err)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM probe`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("probe rows = %d, want 1 (the rolled-back insert must not survive)", n)
	}
}

// TestPing verifies the reader-pool health probe used by /healthz.
func TestPing(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	db.Close()
	if err := db.Ping(ctx); err == nil {
		t.Fatal("Ping after Close should fail")
	}
}

// TestReaderSeesCommittedWritesAndRejectsWrites guards the two invariants the
// reader/writer split rests on, neither of which the :memory: API suite exercises
// (there reader == writer, a single pool):
//   - the reader pool observes a write committed on the writer pool (WAL
//     cross-connection visibility) — the read-your-writes guarantee that
//     on-demand indexing relies on (UpsertBook on the writer, then GetBook on
//     the reader);
//   - the reader pool is genuinely read-only (query_only), so a write
//     accidentally routed through QueryContext/QueryRowContext fails loudly here
//     instead of only in production against the file-backed DB.
func TestReaderSeesCommittedWritesAndRejectsWrites(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if db.reader == db.writer {
		t.Fatal("expected a separate reader pool for a file-backed DB")
	}

	if _, err := db.ExecContext(ctx, `CREATE TABLE probe (x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.WithTx(ctx, "insert", func(tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `INSERT INTO probe(x) VALUES(42)`)
		return e
	}); err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	// The reader pool (a distinct connection) must see the just-committed row.
	var x int
	if err := db.QueryRowContext(ctx, `SELECT x FROM probe`).Scan(&x); err != nil {
		t.Fatalf("read via reader pool: %v", err)
	}
	if x != 42 {
		t.Fatalf("reader saw x=%d, want 42 (cross-pool read-your-writes broken)", x)
	}

	// A write on the reader pool must be refused — query_only(ON) is what stops a
	// stray read-executor write from silently corrupting via the wrong pool.
	if _, err := db.reader.ExecContext(ctx, `INSERT INTO probe(x) VALUES(1)`); err == nil {
		t.Fatal("reader pool accepted a write; query_only(ON) is not in effect")
	}
}
