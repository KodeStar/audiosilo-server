//go:build sqlite_native && cgo && sqlite_fts5 && sqlite_preupdate_hook

package store

import (
	"context"
	"database/sql"
	"errors"
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
