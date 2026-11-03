package store

// The SQLite library the server's database is opened with: modernc's (pure Go,
// the default build) or, in a build with -tags sqlite_native (cgo), the native
// C library through mattn/go-sqlite3 (engine_native.go). One is chosen per
// process, the first time it is needed, and every open of the database goes
// through it (Open's pools, VacuumFile, openReadOnly): two copies of SQLite on
// one file in one process don't see each other's POSIX locks, which corrupts it
// (sqlite.org/howtocorrupt.html, 2.2.1). The metadata mirror's own meta.sqlite,
// another file, is modernc's either way (audiosilo-meta's pkg/query).
//
// What the store and the catalogue use of a library beyond database/sql goes
// through the engine: the pre-update hook (the user-state watch), SQL functions
// (RegisterFunction) and the constraint-violation test.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"log/slog"
	"os"
	"sync"
)

// engine is one SQLite library behind database/sql.
type engine struct {
	// name is the engine's own name (the start log's, AUDIOSILO_SQLITE's).
	name string
	// driver is the database/sql driver name the store opens with: the library
	// behind the statement cache.
	driver string
	// isUnique reports whether err is a UNIQUE or PRIMARY KEY violation.
	isUnique func(err error) bool
	// setPreUpdateHook installs fn as the pre-update hook of dc (a conn.Raw
	// driver connection: the statement cache's), or removes it for a nil fn;
	// false when dc takes none.
	setPreUpdateHook func(dc any, fn func(*preUpdate)) bool
	// setCommitHook installs fn as dc's commit hook (it returns true to turn the
	// commit into a rollback), or removes it for a nil fn; false when dc takes
	// none. Tests watch commits with it.
	setCommitHook func(dc any, fn func() bool) bool
}

// activeEngine is the process's engine, chosen once (chooseEngine is the
// build's: engine_default.go, engine_native.go).
var activeEngine = sync.OnceValue(chooseEngine)

// driverName is the database/sql driver name every open of the database uses.
func driverName() string { return activeEngine().driver }

// logEngine logs the engine and its SQLite release once the database answers,
// and an AUDIOSILO_SQLITE this build can't honour.
func logEngine(ctx context.Context, db *sql.DB, log *slog.Logger) {
	e := activeEngine()
	var version string
	_ = db.QueryRowContext(ctx, `SELECT sqlite_version()`).Scan(&version)
	log.Info("database: sqlite", "engine", e.name, "version", version)
	if want := os.Getenv("AUDIOSILO_SQLITE"); want != "" && want != e.name {
		log.Warn("database: AUDIOSILO_SQLITE names an engine this build doesn't have",
			"asked", want, "engine", e.name, "native", "a build with -tags sqlite_native")
	}
}

// preUpdate is one row about to be inserted, updated or deleted, as the
// pre-update hook sees it in either library.
type preUpdate struct {
	op                 int // opInsert, opUpdate, opDelete: SQLite's codes
	table              string
	oldRowID, newRowID int64
	count              func() int
	old, new           func(dest ...any) error
}

// SQLite's action codes, the same in every library.
const (
	opDelete = 9
	opInsert = 18
	opUpdate = 23
)

// sqlFunc is a scalar SQL function every connection has (RegisterFunction).
type sqlFunc struct {
	name string
	nArg int
	fn   func(args []driver.Value) (driver.Value, error)
}

// sqlFuncs are the registered functions; a library that registers them per
// connection (native) reads them as each connection opens.
var sqlFuncs []sqlFunc

// RegisterFunction makes a deterministic scalar SQL function of nArg arguments
// available on every connection of the server's database, whichever library
// opens it. Call it from an init function: a connection opened before it has
// no such function. args hold nil, int64, float64, string or []byte.
func RegisterFunction(name string, nArg int, fn func(args []driver.Value) (driver.Value, error)) {
	sqlFuncs = append(sqlFuncs, sqlFunc{name: name, nArg: nArg, fn: fn})
	registerModerncFunction(name, nArg, fn)
}
