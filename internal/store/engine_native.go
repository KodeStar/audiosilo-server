//go:build sqlite_native && cgo && sqlite_fts5 && sqlite_preupdate_hook

package store

// The native C SQLite (mattn/go-sqlite3, its bundled amalgamation; with mattn's
// libsqlite3 tag, the system's library) for a build with
//
//	CGO_ENABLED=1 go build -tags "sqlite_native sqlite_fts5 sqlite_preupdate_hook" ./cmd/audiosilo
//
// sqlite_fts5 gives it FTS5 (books_fts) and sqlite_preupdate_hook the
// pre-update hook the user-state watch needs; without either the build stops
// (engine_native_guard.go) rather than run without them. AUDIOSILO_SQLITE=modernc
// runs such a binary on modernc's library instead (for comparing the two on one
// machine); the choice holds for the process.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"

	mattn "github.com/mattn/go-sqlite3"
)

// nativeDriver is the database/sql name of mattn's driver behind the statement
// cache.
const nativeDriver = "sqlite-native-stmtcache"

func init() {
	d := &mattn.SQLiteDriver{ConnectHook: func(c *mattn.SQLiteConn) error {
		for _, f := range sqlFuncs {
			if err := c.RegisterFunc(f.name, mattnFunc(f), true); err != nil {
				return err
			}
		}
		return nil
	}}
	sql.Register(nativeDriver, cacheDriver{d: pragmaDriver{d}, execCacheable: plainDML, query: nativeQuery,
		afterExec: nativeAfterExec})
}

var (
	anyType   = reflect.TypeFor[any]()
	errorType = reflect.TypeFor[error]()
)

// mattnFunc is f as mattn registers a function: a Go function of f.nArg any
// parameters (mattn takes the arity from the signature, so a call with another
// number of arguments fails to prepare, as on modernc, rather than reaching fn),
// handing fn its arguments as modernc does (mattn's NULL, a nil []byte, as nil).
func mattnFunc(f sqlFunc) any {
	call := func(args []reflect.Value) []reflect.Value {
		vals := make([]driver.Value, len(args))
		for i, a := range args {
			v := a.Interface()
			if b, ok := v.([]byte); ok && b == nil {
				v = nil
			}
			vals[i] = v
		}
		out, err := f.fn(vals)
		res, errV := reflect.New(anyType).Elem(), reflect.New(errorType).Elem()
		if out != nil {
			res.Set(reflect.ValueOf(out))
		}
		if err != nil {
			errV.Set(reflect.ValueOf(err))
		}
		return []reflect.Value{res, errV}
	}
	in, variadic := make([]reflect.Type, max(f.nArg, 1)), f.nArg < 0
	for i := range in {
		in[i] = anyType
	}
	if variadic {
		in[0] = reflect.TypeFor[[]any]()
		inner := call
		call = func(args []reflect.Value) []reflect.Value { // the one []any: spread it
			rest := args[0]
			spread := make([]reflect.Value, rest.Len())
			for i := range spread {
				spread[i] = rest.Index(i)
			}
			return inner(spread)
		}
	}
	return reflect.MakeFunc(reflect.FuncOf(in, []reflect.Type{anyType, errorType}, variadic), call).Interface()
}

func chooseEngine() *engine {
	if os.Getenv("AUDIOSILO_SQLITE") == "modernc" {
		return moderncEngine
	}
	return nativeEngine
}

var nativeEngine = &engine{
	name:   "native",
	driver: nativeDriver,
	isUnique: func(err error) bool {
		var serr mattn.Error
		if errors.As(err, &serr) {
			return serr.ExtendedCode == mattn.ErrConstraintUnique || serr.ExtendedCode == mattn.ErrConstraintPrimaryKey
		}
		var nerr *nativeError // the fast query path's (engine_native_rows.go)
		if errors.As(err, &nerr) {
			return nerr.extended == int(mattn.ErrConstraintUnique) || nerr.extended == int(mattn.ErrConstraintPrimaryKey)
		}
		return false
	},
	setPreUpdateHook: func(dc any, fn func(*preUpdate)) bool {
		c, ok := unwrapConn(dc).(*mattn.SQLiteConn)
		if !ok {
			return false
		}
		if fn == nil {
			c.RegisterPreUpdateHook(nil)
			return true
		}
		c.RegisterPreUpdateHook(func(d mattn.SQLitePreUpdateData) {
			fn(&preUpdate{op: d.Op, table: d.TableName, oldRowID: d.OldRowID, newRowID: d.NewRowID,
				count: d.Count, old: d.Old, new: d.New})
		})
		return true
	},
	setCommitHook: func(dc any, fn func() bool) bool {
		c, ok := unwrapConn(dc).(*mattn.SQLiteConn)
		if !ok {
			return false
		}
		if fn == nil {
			c.RegisterCommitHook(nil)
			return true
		}
		c.RegisterCommitHook(func() int {
			if fn() {
				return 1
			}
			return 0
		})
		return true
	},
}

// plainDML reports whether q is an INSERT, UPDATE, DELETE or REPLACE without
// RETURNING: the statements the statement cache may run again for Exec on
// mattn. mattn's Exec steps a statement once, so one that returns rows (a
// SELECT) would stop after its first; plain DML is done in that step, after
// which the cache resets it (nativeAfterExec).
func plainDML(q string) bool {
	if containsFold(q, "RETURNING") {
		return false
	}
	head := strings.TrimSpace(q)
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
		if hasPrefixFold(head, kw) {
			return true
		}
	}
	return false
}

// pragmaDriver opens mattn connections from the store's DSNs, whose PRAGMAs
// are modernc's _pragma parameters (dsnPragmas): it takes them out of the DSN
// and runs each, in the DSN's order, as the connection opens. (modernc runs
// busy_timeout first and the rest sorted by name; mattn has set a 5 s
// busy_timeout before any of them runs, so the order changes nothing here.)
type pragmaDriver struct{ d *mattn.SQLiteDriver }

func (p pragmaDriver) Open(name string) (driver.Conn, error) {
	dsn, pragmas := splitPragmas(name)
	c, err := p.d.Open(dsn)
	if err != nil {
		return nil, err
	}
	for _, pr := range pragmas {
		if _, err := c.(driver.ExecerContext).ExecContext(context.Background(), "PRAGMA "+pr, nil); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// splitPragmas takes the _pragma parameters out of a DSN: the DSN without them,
// and their values ("journal_mode(WAL)").
func splitPragmas(name string) (string, []string) {
	path, query, ok := strings.Cut(name, "?")
	if !ok {
		return name, nil
	}
	var pragmas []string
	var rest []string
	for part := range strings.SplitSeq(query, "&") {
		k, v, _ := strings.Cut(part, "=")
		if k == "_pragma" {
			if u, err := url.QueryUnescape(v); err == nil {
				v = u
			}
			pragmas = append(pragmas, v)
			continue
		}
		if part != "" {
			rest = append(rest, part)
		}
	}
	if len(rest) == 0 {
		return path, pragmas
	}
	return path + "?" + strings.Join(rest, "&"), pragmas
}
