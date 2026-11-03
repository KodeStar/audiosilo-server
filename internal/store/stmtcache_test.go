package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// openCached opens a one-connection pool on the engine's driver (the statement
// cache in front of its library) over a fresh file, so every call runs on the
// same cacheConn.
func openCached(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName(), filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// cacheStats is the one connection's cache: how many statements it prepared,
// how many runs reused one, and how many it holds.
func cacheStats(t *testing.T, db *sql.DB) (prepared, hits, held int) {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.Raw(func(dc any) error {
		c, ok := dc.(*cacheConn)
		if !ok {
			return fmt.Errorf("driver connection is %T, not the cache's", dc)
		}
		prepared, hits, held = c.prepared, c.hits, len(c.stmts)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return prepared, hits, held
}

func mustExec(t *testing.T, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// A statement run again is the one prepared before, with the new arguments and
// nothing of the last run's.
func TestStmtCacheReusesStatements(t *testing.T) {
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (k INTEGER PRIMARY KEY, v TEXT)`)
	for i := range 100 {
		mustExec(t, db, `INSERT INTO t (k, v) VALUES (?, ?)`, i, fmt.Sprint("v", i))
	}
	for i := range 100 {
		var v string
		if err := db.QueryRowContext(context.Background(), `SELECT v FROM t WHERE k = ?`, i).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprint("v", i); v != want {
			t.Fatalf("k=%d: got %q, want %q", i, v, want)
		}
	}
	// A run with fewer arguments bound than the last: nothing carries over.
	var n sql.NullString
	if err := db.QueryRowContext(context.Background(), `SELECT ?`, nil).Scan(&n); err != nil || n.Valid {
		t.Fatalf("SELECT NULL: %v, %v", n, err)
	}
	// The CREATE is cached only where any Exec may be (modernc; not mattn).
	want := 3 // insert, select, select ?
	if activeEngine().driver == moderncDriver {
		want = 4
	}
	prepared, hits, _ := cacheStats(t, db)
	if prepared != want || hits != 198 {
		t.Errorf("prepared %d, hits %d; want %d prepared and 198 hits", prepared, hits, want)
	}
}

// A query run again inside its own rows loop gets a statement of its own; the
// outer rows read on undisturbed.
func TestStmtCacheNestedRuns(t *testing.T) {
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (k INTEGER)`)
	for i := range 5 {
		mustExec(t, db, `INSERT INTO t VALUES (?)`, i)
	}
	ctx := context.Background()
	err := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		const q = `SELECT k FROM t WHERE k >= ? ORDER BY k`
		outer, err := tx.QueryContext(ctx, q, 0)
		if err != nil {
			return err
		}
		defer outer.Close()
		var seen []int
		for outer.Next() {
			var k int
			if err := outer.Scan(&k); err != nil {
				return err
			}
			seen = append(seen, k)
			n, err := func() (int, error) {
				inner, err := tx.QueryContext(ctx, q, k)
				if err != nil {
					return 0, err
				}
				defer inner.Close()
				n := 0
				for inner.Next() {
					n++
				}
				return n, inner.Err()
			}()
			if err != nil {
				return err
			}
			if n != 5-k {
				return fmt.Errorf("inner run from %d read %d rows, want %d", k, n, 5-k)
			}
		}
		if err := outer.Err(); err != nil {
			return err
		}
		if fmt.Sprint(seen) != "[0 1 2 3 4]" {
			return fmt.Errorf("outer rows read %v", seen)
		}
		return nil
	}()
	if err != nil {
		t.Fatal(err)
	}
}

// Past the bound, the least recently used statement not in use is closed; one
// whose rows are open is kept.
func TestStmtCacheEvicts(t *testing.T) {
	old := maxCachedStmts
	maxCachedStmts = 4
	defer func() { maxCachedStmts = old }()
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (k INTEGER)`)
	mustExec(t, db, `INSERT INTO t VALUES (1), (2)`)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open, err := conn.QueryContext(ctx, `SELECT k FROM t ORDER BY k`)
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	for i := range 20 {
		var got int
		if err := conn.QueryRowContext(ctx, fmt.Sprintf(`SELECT %d + k FROM t WHERE k = 1`, i)).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != i+1 {
			t.Fatalf("statement %d: got %d", i, got)
		}
	}
	var ks []int
	for open.Next() {
		var k int
		if err := open.Scan(&k); err != nil {
			t.Fatal(err)
		}
		ks = append(ks, k)
	}
	if err := open.Err(); err != nil || fmt.Sprint(ks) != "[1 2]" {
		t.Fatalf("rows kept open across evictions read %v, %v", ks, err)
	}
	_ = open.Close() // frees the statement, and the connection for cacheStats
	_ = conn.Close()
	if _, _, held := cacheStats(t, db); held > 4 {
		t.Errorf("cache holds %d statements, bound 4", held)
	}
	// An evicted statement is prepared again.
	if n := count(t, db, `SELECT 0 + k FROM t WHERE k = 1`); n != 1 {
		t.Errorf("evicted statement run again: %d", n)
	}
}

// SQL holding several statements runs every one (modernc's own path) and is
// not cached.
func TestStmtCacheMultiStatement(t *testing.T) {
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE a (x INTEGER); CREATE TABLE b (y INTEGER); INSERT INTO b VALUES (7);`)
	if n := count(t, db, `SELECT y FROM b`); n != 7 {
		t.Fatalf("second statement's insert: %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM sqlite_schema WHERE name IN ('a', 'b')`); n != 2 {
		t.Fatalf("tables made: %d", n)
	}
	if prepared, _, _ := cacheStats(t, db); prepared != 2 {
		t.Errorf("prepared %d, want 2 (the two queries)", prepared)
	}
	for _, q := range []string{`SELECT 1;`, "SELECT 1 ;\n", `SELECT 1`} {
		if multiStatement(q) {
			t.Errorf("multiStatement(%q) = true", q)
		}
	}
}

// schemaChange reads the SQL text in any ASCII case without copying it.
func TestSchemaChange(t *testing.T) {
	for q, want := range map[string]bool{
		"-- 0001\nCREATE TABLE a (x INTEGER)": true,
		"create index i on a(x)":              true,
		"Alter Table a ADD y":                 true,
		"drop TABLE a":                        true,
		"INSERT INTO a VALUES (1)":            false,
		"SELECT created_at FROM a":            false,
		"UPDATE a SET dropped = 1":            false,
		"":                                    false,
	} {
		if got := schemaChange(q); got != want {
			t.Errorf("schemaChange(%q) = %v, want %v", q, got, want)
		}
	}
}

// A statement that fails (a constraint) answers its error and runs again
// cleanly; SQL that doesn't prepare is not cached.
func TestStmtCacheErrors(t *testing.T) {
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (k INTEGER PRIMARY KEY)`)
	mustExec(t, db, `INSERT INTO t VALUES (?)`, 1)
	if _, err := db.Exec(`INSERT INTO t VALUES (?)`, 1); err == nil {
		t.Fatal("duplicate key inserted")
	}
	mustExec(t, db, `INSERT INTO t VALUES (?)`, 2)
	if n := count(t, db, `SELECT count(*) FROM t`); n != 2 {
		t.Fatalf("rows: %d", n)
	}
	before, _, held := cacheStats(t, db)
	if _, err := db.Exec(`INSERT INTO nowhere VALUES (1)`); err == nil {
		t.Fatal("insert into a missing table succeeded")
	}
	if prepared, _, now := cacheStats(t, db); prepared != before || now != held {
		t.Errorf("a statement that failed to prepare was cached (%d -> %d prepared, %d -> %d held)", before, prepared, held, now)
	}
}

// A cached statement over a table whose schema changed sees the new schema.
func TestStmtCacheSchemaChange(t *testing.T) {
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER)`)
	mustExec(t, db, `INSERT INTO t VALUES (1)`)
	cols := func() int {
		rows, err := db.Query(`SELECT * FROM t`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		c, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return len(c)
	}
	if n := cols(); n != 1 {
		t.Fatalf("columns before: %d", n)
	}
	mustExec(t, db, `ALTER TABLE t ADD COLUMN b TEXT`)
	if n := cols(); n != 2 {
		t.Fatalf("columns after ALTER: %d, want 2", n)
	}
}

// A run its context ends is interrupted, and the statement runs again
// afterwards.
func TestStmtCacheCancelledRun(t *testing.T) {
	db := openCached(t)
	const q = `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM c WHERE n < ?) SELECT count(*) FROM c`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var n int
	err := db.QueryRowContext(ctx, q, 1_000_000_000).Scan(&n)
	if err == nil {
		t.Fatal("a billion-row count finished inside 20 ms")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("interrupted with %v", err)
	}
	if got := count(t, db, q, 10); got != 10 {
		t.Fatalf("run after the interrupted one: %d", got)
	}
}

// The column types read the same through the cache, and the raw connection
// still takes the engine's pre-update hook (the user-state watch installs one).
func TestStmtCacheForwards(t *testing.T) {
	db := openCached(t)
	mustExec(t, db, `CREATE TABLE t (a INTEGER NOT NULL, b TEXT)`)
	for range 2 { // the second run is the cached statement's rows
		func() {
			rows, err := db.Query(`SELECT a, b FROM t`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			types, err := rows.ColumnTypes()
			if err != nil {
				t.Fatal(err)
			}
			if got := types[0].DatabaseTypeName() + "," + types[1].DatabaseTypeName(); got != "INTEGER,TEXT" {
				t.Errorf("column types %s", got)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
		}()
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(dc any) error {
		if !activeEngine().setPreUpdateHook(dc, func(*preUpdate) {}) || !activeEngine().setPreUpdateHook(dc, nil) {
			return errors.New("connection takes no pre-update hook")
		}
		if u, ok := dc.(interface{ Unwrap() driver.Conn }); !ok || u.Unwrap() == nil {
			return errors.New("connection doesn't unwrap")
		}
		return nil
	})
	_ = conn.Close() // the pool's one connection: cacheStats takes it next
	if err != nil {
		t.Fatal(err)
	}
	if _, _, held := cacheStats(t, db); held == 0 {
		t.Error("nothing cached")
	}
}
