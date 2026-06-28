// Package store opens the SQLite database and applies schema migrations.
//
// AudioSilo uses modernc.org/sqlite (pure Go, no CGO) so binaries cross-compile
// without a C toolchain. The database is a rebuildable index/cache; the audio
// files on disk remain the source of truth for content.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrations apply in lexical filename order, so each must carry the NNNN_
// sequence prefix; a misnamed file would silently run out of sequence.
var migrationName = regexp.MustCompile(`^\d{4}_.*\.sql$`)

// IsUniqueViolation reports whether err is a SQLite UNIQUE (or PRIMARY KEY)
// constraint violation, so the transport layer can map a duplicate-name insert
// to a 409 Conflict instead of an opaque 500. Connections enable extended result
// codes (see the modernc driver), so the error carries the specific
// SQLITE_CONSTRAINT_UNIQUE code — distinct from e.g. a FOREIGN KEY violation,
// which stays a generic internal error.
func IsUniqueViolation(err error) bool {
	var serr *sqlite.Error
	if errors.As(err, &serr) {
		return serr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE ||
			serr.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
	}
	return false
}

// DB is the AudioSilo database handle. It owns two connection pools over the same
// SQLite file: a single-connection WRITER (SQLite serializes writers; one
// connection avoids "database is locked" churn) and a multi-connection read-only
// READER (WAL allows concurrent readers). Routing reads to a separate pool means a
// slow or stuck writer never blocks reads, so the UI stays responsive even while a
// write stalls on a slow volume. For an in-memory DSN the two are the same pool:
// an in-memory database is per-connection, so a second pool would be a distinct,
// empty database.
type DB struct {
	writer *sql.DB
	reader *sql.DB
	log    *slog.Logger

	stopSampler context.CancelFunc // nil unless the pool-stats sampler is running
}

// Option configures Open.
type Option func(*openConfig)

type openConfig struct {
	log *slog.Logger
}

// WithLogger sets the logger used for slow-transaction warnings and pool-stats
// sampling. Defaults to slog.Default() when unset.
func WithLogger(l *slog.Logger) Option {
	return func(c *openConfig) {
		if l != nil {
			c.log = l
		}
	}
}

// slowTxThreshold is how long a single transaction may run before WithTx logs it.
// A write transaction should be milliseconds; seconds means a slow volume or
// contention — the signal that precedes a backend-wide stall.
const slowTxThreshold = 2 * time.Second

// dsnPragmas appends AudioSilo's required pragmas to a DSN. WAL for concurrent
// readers, busy_timeout to ride out brief locks, and foreign_keys for referential
// integrity. foreign_keys defaults OFF per SQLite connection, and the schema
// relies on ~28 ON DELETE CASCADE rules, so these must always be applied — append
// with the right separator rather than skipping them when the DSN already carries
// query params (which would silently disable every cascade delete). extra carries
// per-pool pragmas (e.g. query_only for the reader).
func dsnPragmas(dsn string, extra ...string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	p := "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	for _, e := range extra {
		p += "&_pragma=" + e
	}
	return dsn + sep + p
}

// isMemoryDSN reports whether dsn names an in-memory database, which is
// per-connection and so cannot be backed by a second pool.
func isMemoryDSN(dsn string) bool { return strings.Contains(dsn, ":memory:") }

// Open opens (creating if needed) the SQLite database at dsn and applies any
// pending migrations. Pass ":memory:" for tests.
func Open(ctx context.Context, dsn string, opts ...Option) (*DB, error) {
	cfg := openConfig{log: slog.Default()}
	for _, o := range opts {
		o(&cfg)
	}

	writer, err := sql.Open("sqlite", dsnPragmas(dsn))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	writer.SetMaxOpenConns(1)
	if err := writer.PingContext(ctx); err != nil {
		writer.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	// reader == writer until (and unless) a separate read pool is opened, so
	// migrate() and any read during bootstrap go through the writer connection.
	db := &DB{writer: writer, reader: writer, log: cfg.log}
	if err := db.migrate(ctx); err != nil {
		writer.Close()
		return nil, err
	}

	// A file-backed DB gets a concurrent read-only pool over the same file so a
	// stuck writer can't block reads. An in-memory DB stays single-pool (a second
	// pool would be a different, empty database).
	if !isMemoryDSN(dsn) {
		reader, err := sql.Open("sqlite", dsnPragmas(dsn, "query_only(ON)"))
		if err != nil {
			writer.Close()
			return nil, fmt.Errorf("open sqlite reader: %w", err)
		}
		n := runtime.NumCPU()
		if n < 4 {
			n = 4
		}
		reader.SetMaxOpenConns(n)
		reader.SetMaxIdleConns(n)
		if err := reader.PingContext(ctx); err != nil {
			reader.Close()
			writer.Close()
			return nil, fmt.Errorf("ping sqlite reader: %w", err)
		}
		db.reader = reader
		db.startStatsSampler()
	}
	return db, nil
}

// QueryContext runs a read query on the reader pool.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.reader.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row read query on the reader pool.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.reader.QueryRowContext(ctx, query, args...)
}

// ExecContext runs a write on the writer pool. (This also satisfies the auth
// package's sqlExecer, so standalone auth-code writes route to the writer.)
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.writer.ExecContext(ctx, query, args...)
}

// BeginTx starts a transaction on the writer pool. Prefer WithTx, which also
// guarantees rollback and logs slow transactions.
func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	return db.writer.BeginTx(ctx, opts)
}

// Ping verifies the database is reachable for reads. It runs a trivial query on
// the reader pool (a stronger check than a bare connection ping) and backs the
// GET /healthz endpoint, so a wedged writer doesn't fail the health check while
// reads are still serving.
func (db *DB) Ping(ctx context.Context) error {
	var x int
	return db.reader.QueryRowContext(ctx, `SELECT 1`).Scan(&x)
}

// WithTx runs fn inside a writer transaction, committing on success and rolling
// back on error (or panic). name labels the operation in the slow-transaction log.
func (db *DB) WithTx(ctx context.Context, name string, fn func(*sql.Tx) error) error {
	start := time.Now()
	tx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if el := time.Since(start); el >= slowTxThreshold {
		db.log.Warn("slow db transaction", "op", name, "elapsed", el.String())
	}
	return nil
}

// startStatsSampler periodically logs writer-pool contention. A growing WaitCount
// means callers are queueing for the single writer connection — the direct signal
// that a slow hold is starving the backend (which previously surfaced only as an
// unexplained freeze). Stopped by Close.
func (db *DB) startStatsSampler() {
	ctx, cancel := context.WithCancel(context.Background())
	db.stopSampler = cancel
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		var lastWaitCount int64
		var lastWaitDur time.Duration
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s := db.writer.Stats()
				if s.WaitCount > lastWaitCount {
					db.log.Warn("db writer pool contention",
						"waits", s.WaitCount-lastWaitCount,
						"wait_time", (s.WaitDuration - lastWaitDur).String(),
						"in_use", s.InUse)
				}
				lastWaitCount = s.WaitCount
				lastWaitDur = s.WaitDuration
			}
		}
	}()
}

// Close stops the stats sampler and closes both pools.
func (db *DB) Close() error {
	if db.stopSampler != nil {
		db.stopSampler()
	}
	var rerr error
	if db.reader != nil && db.reader != db.writer {
		rerr = db.reader.Close()
	}
	if werr := db.writer.Close(); werr != nil {
		return werr
	}
	return rerr
}

// migrate applies embedded migrations that have not yet been recorded. It runs
// entirely on the writer connection (setup-time, single-threaded).
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if !migrationName.MatchString(e.Name()) {
			return fmt.Errorf("invalid migration filename %q: must match NNNN_*.sql", e.Name())
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		var exists int
		if err := db.writer.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE name = ?`, name).Scan(&exists); err == nil {
			continue // already applied
		} else if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := db.writer.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(name, applied_at) VALUES(?, ?)`,
			name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
