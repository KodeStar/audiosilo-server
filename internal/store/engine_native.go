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
	"strings"

	mattn "github.com/mattn/go-sqlite3"
)

// nativeDriver is the database/sql name of mattn's driver behind the statement
// cache.
const nativeDriver = "sqlite-native-stmtcache"

func init() {
	d := &mattn.SQLiteDriver{ConnectHook: func(c *mattn.SQLiteConn) error {
		for _, f := range sqlFuncs {
			fn := f.fn
			if err := c.RegisterFunc(f.name, func(args ...any) (any, error) {
				vals := make([]driver.Value, len(args))
				for i, a := range args {
					vals[i] = a
				}
				return fn(vals)
			}, true); err != nil {
				return err
			}
		}
		return nil
	}}
	sql.Register(nativeDriver, cacheDriver{d: pragmaDriver{d}, execCacheable: plainDML, query: nativeQuery})
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
// mattn. mattn's Exec steps a statement once and resets it only at its next
// run, so a statement that returns a row (a SELECT, RETURNING) would stay
// active, holding its transaction open; plain DML is done in one step.
func plainDML(q string) bool {
	head := strings.ToUpper(strings.TrimSpace(q))
	if strings.Contains(head, "RETURNING") {
		return false
	}
	for _, kw := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
		if strings.HasPrefix(head, kw) {
			return true
		}
	}
	return false
}

// pragmaDriver opens mattn connections from the store's DSNs, whose PRAGMAs
// are modernc's _pragma parameters (dsnPragmas): it takes them out of the DSN
// and runs each, in order, as the connection opens, as modernc does.
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
