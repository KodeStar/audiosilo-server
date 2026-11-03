package store

// modernc's SQLite (pure Go): the default build's engine, and the native build's
// with AUDIOSILO_SQLITE=modernc. Always compiled: audiosilo-meta's pkg/query
// links it into every build anyway.

import (
	"database/sql"
	"database/sql/driver"
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// moderncDriver is the database/sql name of modernc's driver behind the
// statement cache.
const moderncDriver = "sqlite-stmtcache"

func init() {
	db, err := sql.Open("sqlite", "")
	if err != nil {
		panic(err)
	}
	// modernc resets a statement after every run, so any statement may be
	// cached for Exec.
	sql.Register(moderncDriver, cacheDriver{d: db.Driver(), execCacheable: func(string) bool { return true }})
	_ = db.Close()
}

var moderncEngine = &engine{
	name:   "modernc",
	driver: moderncDriver,
	// Connections enable extended result codes, so a UNIQUE violation carries
	// SQLITE_CONSTRAINT_UNIQUE, distinct from e.g. a FOREIGN KEY violation.
	isUnique: func(err error) bool {
		var serr *sqlite.Error
		if errors.As(err, &serr) {
			return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
				serr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
		}
		return false
	},
	setPreUpdateHook: func(dc any, fn func(*preUpdate)) bool {
		h, ok := unwrapConn(dc).(sqlite.HookRegisterer)
		if !ok {
			return false
		}
		if fn == nil {
			h.RegisterPreUpdateHook(nil)
			return true
		}
		h.RegisterPreUpdateHook(func(d sqlite.SQLitePreUpdateData) {
			fn(&preUpdate{op: int(d.Op), table: d.TableName, oldRowID: d.OldRowID, newRowID: d.NewRowID,
				count: d.Count, old: d.Old, new: d.New})
		})
		return true
	},
	setCommitHook: func(dc any, fn func() bool) bool {
		h, ok := unwrapConn(dc).(sqlite.HookRegisterer)
		if !ok {
			return false
		}
		if fn == nil {
			h.RegisterCommitHook(nil)
			return true
		}
		h.RegisterCommitHook(func() int32 {
			if fn() {
				return 1
			}
			return 0
		})
		return true
	},
}

// registerModerncFunction registers fn with modernc, which keeps one registry
// for every connection it opens.
func registerModerncFunction(name string, nArg int, fn func(args []driver.Value) (driver.Value, error)) {
	sqlite.MustRegisterDeterministicScalarFunction(name, int32(nArg),
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) { return fn(args) })
}

// unwrapConn is the library's own connection under the statement cache's.
func unwrapConn(dc any) any {
	if c, ok := dc.(interface{ Unwrap() driver.Conn }); ok {
		return c.Unwrap()
	}
	return dc
}
