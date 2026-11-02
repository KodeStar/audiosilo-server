package store

// The prepared-statement cache. database/sql hands the driver SQL text on every
// ExecContext/QueryContext, and modernc prepares it (SQLite parses and plans it)
// each time, runs it once and finalizes it. A first scan of 1,000 books spent
// 58% of the writer's time in that parse, against 27% running the statements.
// Each connection here keeps its single-statement SQL prepared, by its text, and
// runs the prepared statement again; modernc resets and clears its bindings
// after each run, so a run sees nothing of the one before.
//
// Not cached: SQL holding more than one statement (a migration; modernc runs it
// statement by statement), a statement whose rows are still open (a query run
// again inside its own rows loop gets a statement of its own), and nothing past
// maxCachedStmts per connection, where the least recently used statement not in
// use is closed to make room. A statement whose schema changes is re-prepared by
// SQLite itself (sqlite3_prepare_v2), as an uncached one would be.
//
// A join whose parameters are bare ? gains little: SQLite 3.54 reads their
// bound values to estimate the join's rows, so each new binding expires the
// statement and SQLite re-parses it on its next run. Joins take +? instead
// (see SQLite in CONVENTIONS.md).
//
// database/sql never uses one driver connection from two goroutines at once
// (it holds the connection's lock around every driver call, a Rows.Close from
// a cancelled context's watcher included), so a connection's cache needs no
// lock of its own.

import (
	"container/list"
	"context"
	"database/sql"
	"database/sql/driver"
	"reflect"
	"strings"

	"modernc.org/sqlite"
)

// driverName is the name store.Open opens its pools by: modernc's driver behind
// the statement cache.
const driverName = "sqlite-stmtcache"

// maxCachedStmts bounds each connection's cache. The server has well under a
// thousand distinct statements, few of them hot; a var so a test can shrink it.
var maxCachedStmts = 256

func init() {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	sql.Register(driverName, cacheDriver{db.Driver()})
	_ = db.Close()
}

type cacheDriver struct{ d driver.Driver }

func (c cacheDriver) Open(name string) (driver.Conn, error) {
	cn, err := c.d.Open(name)
	if err != nil {
		return nil, err
	}
	return &cacheConn{Conn: cn, stmts: map[string]*list.Element{}, lru: list.New()}, nil
}

// cachedStmt is one prepared statement and whether its rows are open.
type cachedStmt struct {
	query string
	s     driver.Stmt
	busy  bool
}

// cacheConn is modernc's connection with its statements kept prepared. It
// forwards the optional interfaces modernc's connection has that database/sql
// or this package use; a conn.Raw that needs any other gets modernc's
// connection from Unwrap.
type cacheConn struct {
	driver.Conn
	stmts map[string]*list.Element // query -> element of lru, Value *cachedStmt
	lru   *list.List               // front: most recently used
	// prepared and hits count the cache's outcomes (tests).
	prepared, hits int
}

// Unwrap is modernc's own connection.
func (c *cacheConn) Unwrap() driver.Conn { return c.Conn }

func (c *cacheConn) Close() error {
	for e := c.lru.Front(); e != nil; e = e.Next() {
		_ = e.Value.(*cachedStmt).s.Close()
	}
	c.stmts, c.lru = nil, list.New()
	return c.Conn.Close()
}

func (c *cacheConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, o)
}

func (c *cacheConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
}

func (c *cacheConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

func (c *cacheConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *cacheConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

func (c *cacheConn) RegisterPreUpdateHook(f sqlite.PreUpdateHookFn) {
	c.Conn.(sqlite.HookRegisterer).RegisterPreUpdateHook(f)
}

func (c *cacheConn) RegisterCommitHook(f sqlite.CommitHookFn) {
	c.Conn.(sqlite.HookRegisterer).RegisterCommitHook(f)
}

func (c *cacheConn) RegisterRollbackHook(f sqlite.RollbackHookFn) {
	c.Conn.(sqlite.HookRegisterer).RegisterRollbackHook(f)
}

// multiStatement reports whether q may hold more than one statement: a ';'
// anywhere but at its end. A ';' inside a string literal says yes too, which
// only costs that statement the cache.
func multiStatement(q string) bool {
	return strings.Contains(strings.TrimRight(q, "; \t\r\n"), ";")
}

// get is q's prepared statement, nil when q takes the uncached path.
func (c *cacheConn) get(ctx context.Context, q string) (*cachedStmt, error) {
	if multiStatement(q) {
		return nil, nil
	}
	if e, ok := c.stmts[q]; ok {
		cs := e.Value.(*cachedStmt)
		if cs.busy {
			return nil, nil
		}
		c.lru.MoveToFront(e)
		c.hits++
		return cs, nil
	}
	if len(c.stmts) >= maxCachedStmts && !c.evict() {
		return nil, nil // every cached statement is in use
	}
	s, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	c.prepared++
	cs := &cachedStmt{query: q, s: s}
	c.stmts[q] = c.lru.PushFront(cs)
	return cs, nil
}

// evict closes the least recently used statement not in use, false when every
// one is.
func (c *cacheConn) evict() bool {
	for e := c.lru.Back(); e != nil; e = e.Prev() {
		cs := e.Value.(*cachedStmt)
		if cs.busy {
			continue
		}
		c.lru.Remove(e)
		delete(c.stmts, cs.query)
		_ = cs.s.Close()
		return true
	}
	return false
}

func (c *cacheConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	cs, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	}
	return cs.s.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (c *cacheConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	cs, err := c.get(ctx, q)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
	}
	r, err := cs.s.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		return nil, err
	}
	cs.busy = true
	return &cacheRows{rows: r, cs: cs}, nil
}

// cacheRows is a cached statement's rows: closing them frees the statement for
// its next run. The column-type interfaces modernc's rows have are forwarded,
// so sql.Rows.ColumnTypes reads the same.
type cacheRows struct {
	rows driver.Rows
	cs   *cachedStmt
}

func (r *cacheRows) Columns() []string              { return r.rows.Columns() }
func (r *cacheRows) Next(dest []driver.Value) error { return r.rows.Next(dest) }

func (r *cacheRows) Close() error {
	err := r.rows.Close()
	r.cs.busy = false
	return err
}

func (r *cacheRows) ColumnTypeDatabaseTypeName(i int) string {
	if t, ok := r.rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return t.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *cacheRows) ColumnTypeLength(i int) (int64, bool) {
	if t, ok := r.rows.(driver.RowsColumnTypeLength); ok {
		return t.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *cacheRows) ColumnTypeNullable(i int) (bool, bool) {
	if t, ok := r.rows.(driver.RowsColumnTypeNullable); ok {
		return t.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *cacheRows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	if t, ok := r.rows.(driver.RowsColumnTypePrecisionScale); ok {
		return t.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *cacheRows) ColumnTypeScanType(i int) reflect.Type {
	if t, ok := r.rows.(driver.RowsColumnTypeScanType); ok {
		return t.ColumnTypeScanType(i)
	}
	return reflect.TypeFor[any]()
}
