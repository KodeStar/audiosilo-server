//go:build sqlite_native && cgo && sqlite_fts5 && sqlite_preupdate_hook

package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// The native engine's query path: mattn's statement field is there (or every
// query silently goes back to mattn's own rows), a cached query's rows are
// read by it, and what it reads, binds and reports is what modernc's would.
func TestNativeFastRows(t *testing.T) {
	if activeEngine() != nativeEngine {
		t.Skip("AUDIOSILO_SQLITE picked modernc")
	}
	if stmtField < 0 {
		t.Fatal("mattn's SQLiteStmt has no sqlite3_stmt field s: the fast query path is off")
	}
	ctx := context.Background()
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (k INTEGER PRIMARY KEY, i INTEGER, r REAL, s TEXT, b BLOB, n TEXT, e TEXT)`)
	mustExec(t, db, `INSERT INTO t VALUES (1, 42, 1.5, 'héllo', X'0102', NULL, '')`)
	mustExec(t, db, `INSERT INTO t (k, b) VALUES (2, X'')`)

	// The rows are the fast path's.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second run is the cached statement's
		func() {
			rows, err := conn.QueryContext(ctx, `SELECT i FROM t WHERE k = ?`, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			for rows.Next() {
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		}()
	}
	var fast bool
	_ = conn.Raw(func(dc any) error {
		c := dc.(*cacheConn)
		e := c.stmts[`SELECT i FROM t WHERE k = ?`]
		if e == nil {
			return nil
		}
		r, ok, err := c.drv.query(ctx, e.Value.(*cachedStmt).s, nil)
		if ok && err == nil {
			_, fast = r.(*nativeRows)
			_ = r.Close()
		}
		return nil
	})
	_ = conn.Close()
	if !fast {
		t.Fatal("a cached query's rows are not the fast path's")
	}

	// Values as modernc reads them, through a column-typed scan into any.
	var i, r, s, b, n, e any
	if err := db.QueryRowContext(ctx, `SELECT i, r, s, b, n, e FROM t WHERE k = ?`, 1).Scan(&i, &r, &s, &b, &n, &e); err != nil {
		t.Fatal(err)
	}
	if i != int64(42) || r != 1.5 || s != "héllo" || string(b.([]byte)) != "\x01\x02" || n != nil || e != "" {
		t.Errorf("row = %#v %#v %#v %#v %#v %#v", i, r, s, b, n, e)
	}
	var empty []byte
	if err := db.QueryRowContext(ctx, `SELECT b FROM t WHERE k = ?`, 2).Scan(&empty); err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("empty blob = %#v, %v", empty, err)
	}

	// Bindings: named, bool, []byte, nil, time; and a run binds nothing of the last.
	when := time.Date(2026, 11, 3, 1, 2, 3, 0, time.UTC)
	var got string
	if err := db.QueryRowContext(ctx, `SELECT :a || '|' || @b || '|' || $c || '|' || ? || '|' || hex(?) || '|' || coalesce(?, 'null')`,
		sql.Named("a", "x"), sql.Named("b", true), sql.Named("c", 2.5), when, []byte{0xab}, nil).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "x|1|2.5|2026-11-03 01:02:03+00:00|AB|null"; got != want {
		t.Errorf("bound %q, want %q", got, want)
	}

	// A context ended mid-query interrupts it, and the statement runs after.
	const long = `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < ?) SELECT count(*) FROM c`
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	var cnt int
	if err := db.QueryRowContext(cctx, long, 1_000_000_000).Scan(&cnt); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("interrupted query: %v, want the context's deadline", err)
	}
	if err := db.QueryRowContext(ctx, long, 10).Scan(&cnt); err != nil || cnt != 10 {
		t.Errorf("after the interrupt: %d, %v", cnt, err)
	}

	// A constraint failure read through a query is still a unique violation.
	for range 2 {
		var k int
		err := db.QueryRowContext(ctx, `INSERT INTO t (k) VALUES (?) RETURNING k`, 1).Scan(&k)
		if !IsUniqueViolation(err) {
			t.Errorf("duplicate key through RETURNING: %v, not a unique violation", err)
		}
	}
}

// A cached query on one connection, after another connection changed its
// table, reads the new columns on its first run (SQLite re-prepares it at the
// query's first step, before its columns are read).
func TestNativeRowsSeeAnotherConnectionsSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "x.db")
	open := func() *sql.DB {
		db, err := sql.Open(driverName(), dsnPragmas(path))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	reader, writer := open(), open()
	mustExec(t, writer, `CREATE TABLE t (a INTEGER)`)
	mustExec(t, writer, `INSERT INTO t VALUES (1)`)
	cols := func() int {
		rows, err := reader.QueryContext(ctx, `SELECT * FROM t`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		c, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return len(c)
	}
	for range 2 { // the second run is the cached statement's
		if n := cols(); n != 1 {
			t.Fatalf("columns before: %d", n)
		}
	}
	mustExec(t, writer, `ALTER TABLE t ADD COLUMN b TEXT`)
	if n := cols(); n != 2 {
		t.Fatalf("columns after another connection's ALTER: %d, want 2", n)
	}
}

// Rows closed after their context ended leave no interrupt behind: the
// connection's next statement runs.
func TestNativeRowsLeaveNoInterrupt(t *testing.T) {
	if activeEngine() != nativeEngine {
		t.Skip("AUDIOSILO_SQLITE picked modernc")
	}
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (k INTEGER)`)
	mustExec(t, db, `INSERT INTO t VALUES (1), (2)`)
	const long = `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < ?) SELECT count(*) FROM c`
	const q = `SELECT k FROM t ORDER BY k`
	for i := range 200 {
		func() {
			ctx, cancel := context.WithCancel(context.Background())
			rows, err := db.QueryContext(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close() // right after the cancel: the interrupt races it
			cancel()
			_ = rows.Err()
		}()
		var n int
		if err := db.QueryRowContext(context.Background(), long, 20_000).Scan(&n); err != nil || n != 20_000 {
			t.Fatalf("run %d: the next statement after cancelled rows: %d, %v", i, n, err)
		}
	}
	// A context already ended fails the query at once, as on modernc.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = conn.Raw(func(dc any) error {
		e := dc.(*cacheConn).stmts[q]
		if e == nil {
			return errors.New("the query isn't cached")
		}
		_, _, err := nativeQuery(cctx, e.Value.(*cachedStmt).s, nil)
		return err
	})
	_ = conn.Close()
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a query on an ended context: %v, want context.Canceled", err)
	}
}

// RegisterFunction's functions take their arity on the native engine too, and
// see NULL as nil.
func TestNativeFunctionArity(t *testing.T) {
	if activeEngine() != nativeEngine {
		t.Skip("AUDIOSILO_SQLITE picked modernc")
	}
	f := sqlFunc{name: "as_test_kind", nArg: 1, fn: func(args []driver.Value) (driver.Value, error) {
		if args[0] == nil {
			return "nil", nil
		}
		return fmt.Sprintf("%T", args[0]), nil
	}}
	old := sqlFuncs
	sqlFuncs = append(slices.Clone(sqlFuncs), f)
	defer func() { sqlFuncs = old }()
	db := openCached(t)
	var got string
	if err := db.QueryRow(`SELECT as_test_kind(NULL) || ',' || as_test_kind(X'') || ',' || as_test_kind('a') || ',' || as_test_kind(1)`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "nil,[]uint8,string,int64" {
		t.Errorf("argument types %q", got)
	}
	if _, err := db.Exec(`SELECT as_test_kind(1, 2)`); err == nil {
		t.Error("a call with two arguments to a one-argument function prepared")
	}
}
