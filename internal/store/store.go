// Package store opens the SQLite database and applies schema migrations.
//
// AudioSilo uses modernc.org/sqlite (pure Go, no CGO) so binaries cross-compile
// without a C toolchain. The database is a rebuildable index/cache; the audio
// files on disk remain the source of truth for content.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
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
// SQLITE_CONSTRAINT_UNIQUE code - distinct from e.g. a FOREIGN KEY violation,
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
	dsn    string // as given to Open; VacuumInto opens its own connection on it

	// content is the content generation (ContentGeneration): moved after every
	// write on the writer pool has finished, except a user-state one that
	// changed only user state (userstate.go).
	content atomic.Uint64
	// userState is each user-state table's rule as this database's schema
	// spells it (userStateRules), read once migrations have run.
	userState map[string]tableRule
	// onMisfiled, when set (tests), hears of a user-state write that changed a
	// table outside userStateTables instead of a test binary's panic.
	onMisfiled func(table string)
	// durable counts WithDurableTx's commits (DurableCommits).
	durable atomic.Uint64

	stopSampler context.CancelFunc // nil unless the pool-stats sampler is running
}

// Option configures Open.
type Option func(*openConfig)

type openConfig struct {
	log    *slog.Logger
	mmap   string                 // the reader pool's mmap mode (WithReaderMmap)
	onFUSE func(path string) bool // the auto mode's filesystem check (mmap_linux.go)
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
// contention - the signal that precedes a backend-wide stall.
const slowTxThreshold = 2 * time.Second

// dsnPragmas appends AudioSilo's required pragmas to a DSN: WAL for concurrent
// readers, synchronous=NORMAL for commits without an fsync, busy_timeout to ride
// out brief locks, and foreign_keys for referential integrity.
//
// synchronous=NORMAL is the WAL trade-off SQLite documents: a commit no longer
// fsyncs the WAL (only a checkpoint does), so a power cut or OS crash can roll
// back the last few commits, but never corrupts the database or tears a
// transaction (an application crash loses nothing). Any commit can be in that
// window, not only listening progress (which clients re-send). FULL cost an
// fsync per commit (two per progress save), 1 to 5 ms on an SSD and far more on
// an HDD array or a FUSE share, all of it serialized on the one writer.
//
// So the few writes whose loss would bring back what a person ended go through
// WithDurableTx / ExecDurable, which commit under FULL: ending a credential
// (sign-out, a device or API key revoked, a password set or cleared, an account
// disabled, demoted or deleted, an auth code revoked, rotated, replaced or
// used, a pairing token used, a Bridge pairing code replaced, a Bridge paired
// or unpaired) and a Bridge claim (its attempt number must never be
// handed out twice). Their fsync also flushes the NORMAL commits before them.
// Everything else stays NORMAL; listening progress and heartbeats never take it.
//
// foreign_keys defaults OFF per SQLite connection, and the schema relies on ~28
// ON DELETE CASCADE rules, so the pragmas must always be applied - append with
// the right separator rather than skipping them when the DSN already carries
// query params (which would silently disable every cascade delete). extra
// carries per-pool pragmas (e.g. query_only for the reader).
func dsnPragmas(dsn string, extra ...string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	p := "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	for _, e := range extra {
		p += "&_pragma=" + e
	}
	return dsn + sep + p
}

// isMemoryDSN reports whether dsn names an in-memory database, which is
// per-connection and so cannot be backed by a second pool. Both spellings count:
// the bare ":memory:" and the URI form "mode=memory" - missing the latter would
// open a second reader pool over a distinct, empty in-memory database.
func isMemoryDSN(dsn string) bool {
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

// Open opens (creating if needed) the SQLite database at dsn and applies any
// pending migrations. Pass ":memory:" for tests.
func Open(ctx context.Context, dsn string, opts ...Option) (*DB, error) {
	cfg := openConfig{log: slog.Default(), onFUSE: onFUSE}
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
	db := &DB{writer: writer, reader: writer, log: cfg.log, dsn: dsn}
	if err := db.migrate(ctx); err != nil {
		writer.Close()
		return nil, err
	}
	if db.userState, err = userStateRules(ctx, writer); err != nil {
		writer.Close()
		return nil, err
	}

	// A file-backed DB gets a concurrent read-only pool over the same file so a
	// stuck writer can't block reads. An in-memory DB stays single-pool (a second
	// pool would be a different, empty database).
	if !isMemoryDSN(dsn) {
		// Asked once the writer has created the file, so statfs sees it.
		mmap := readerMmap(cfg.mmap, func() bool { return cfg.onFUSE(dsnFile(dsn)) })
		reader, err := sql.Open("sqlite", dsnPragmas(dsn, "query_only(ON)", fmt.Sprintf("mmap_size(%d)", mmap)))
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
		db.logReaderMmap(ctx, cfg.mmap, mmap)
		db.startStatsSampler()
	}
	return db, nil
}

// logReaderMmap says at start whether the readers map the database (mode as
// configured, asked what readerMmap chose, got what SQLite reports: 0 where the
// build or platform can't map).
func (db *DB) logReaderMmap(ctx context.Context, mode string, asked int64) {
	if mode == "" {
		mode = MmapAuto
	}
	var got int64
	if err := db.reader.QueryRowContext(ctx, "PRAGMA mmap_size").Scan(&got); err != nil {
		db.log.Warn("database readers: mmap_size unreadable", "mode", mode, "err", err)
		return
	}
	db.log.Info("database readers: mmap", "on", got > 0, "mode", mode, "mmap_size", got, "asked", asked)
}

// QueryContext runs a read query on the reader pool.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.reader.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row read query on the reader pool.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.reader.QueryRowContext(ctx, query, args...)
}

// ExecDurable is ExecContext committed durably (WithDurableTx): for a lone
// statement that ends a credential. name labels it in the slow-transaction log.
func (db *DB) ExecDurable(ctx context.Context, name, query string, args ...any) (sql.Result, error) {
	var res sql.Result
	err := db.WithDurableTx(ctx, name, func(tx *sql.Tx) error {
		var err error
		res, err = tx.ExecContext(ctx, query, args...)
		return err
	})
	return res, err
}

// ExecContext runs a write on the writer pool. (This also satisfies the auth
// package's sqlExecer, so standalone auth-code writes route to the writer.)
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	defer db.content.Add(1) // after the statement (autocommit) has committed
	return db.writer.ExecContext(ctx, query, args...)
}

// ContentGeneration is the database's content generation: a number that moves
// after every write through this handle has finished (ExecContext returned, a
// WithTx committed or rolled back), except a user-state write (ExecUserState,
// WithUserStateTx) that changed only what userStateTables lists as user state
// (a listener's progress, sessions, history and annotations, a token's
// last-seen touch, the Bridge's heartbeats and job progress), and
// never otherwise. Every write goes through those (the reader pool is
// query_only), so an answer computed from reads that began after
// ContentGeneration returned g, and that reads no user state, is current for as
// long as ContentGeneration still returns g: a write that committed after them
// moved it on (or changed nothing the answer reads), and one that committed
// before them is in what they read. A cache keyed on it is exact (api's list
// cache, the library cache, and the token cache, which drops a touched token's
// row itself; CONVENTIONS.md, Content generation, has each one's audit). It
// moves for every other write, a no-op or failed one included, so it says only
// "no content was written since", never what changed.
func (db *DB) ContentGeneration() uint64 { return db.content.Load() }

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
	return db.withTx(ctx, name, time.Now(), db.writer, true, fn)
}

// WithDurableTx is WithTx for a write that must survive a power cut or OS crash
// (see dsnPragmas for which): it commits with synchronous=FULL, so the commit
// returns only once the WAL is on disk. It holds the writer's one connection
// (sql.DB.Conn) from setting FULL until NORMAL is back, so no other write runs
// in between and none is left at FULL; a connection that can't be put back is
// dropped (the pool opens a fresh one, NORMAL from the DSN). Setting the pragma
// on the connection is what works under modernc and the single-writer pool: a
// PASSIVE wal_checkpoint after the commit would skip its WAL sync whenever a
// reader's snapshot leaves it nothing to copy, and SQLite has no per-commit
// sync of its own.
func (db *DB) WithDurableTx(ctx context.Context, name string, fn func(*sql.Tx) error) error {
	start := time.Now()
	conn, err := db.writer.Conn(ctx)
	if err != nil {
		db.logSlow(name, start, err) // a wait for the writer that timed out, as WithTx logs it
		return err
	}
	defer func() { _ = conn.Close() }()
	// Deferred before FULL is set, so a FULL that took effect but answered an
	// error (a cancel landing on it) is put back too.
	defer func() {
		// Whatever happened, on a context the caller's leaving doesn't end.
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA synchronous = NORMAL`); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA synchronous = FULL`); err != nil {
		db.logSlow(name, start, err)
		return err
	}
	if err := db.withTx(ctx, name, start, conn, true, fn); err != nil {
		return err
	}
	db.durable.Add(1)
	return nil
}

// DurableCommits is how many WithDurableTx commits succeeded (tests check that a
// path takes it).
func (db *DB) DurableCommits() uint64 { return db.durable.Load() }

// logSlow warns of a transaction that took slowTxThreshold or more since start,
// whatever its outcome (err).
func (db *DB) logSlow(name string, start time.Time, err error) {
	if el := time.Since(start); el >= slowTxThreshold {
		db.log.Warn("slow db transaction", "op", name, "elapsed", el.String(), "err", err)
	}
}

// txBeginner is the writer pool or its one connection held (WithDurableTx).
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// withTx is WithTx on a writer pool or connection, start being when the caller
// began waiting for it. With content it moves the content generation once the
// transaction has begun, after its commit or rollback; a user-state
// transaction's caller decides instead (userStateWrite).
func (db *DB) withTx(ctx context.Context, name string, start time.Time, on txBeginner, content bool, fn func(*sql.Tx) error) (err error) {
	// Log a slow transaction regardless of outcome: a tx that stalls and then
	// fails (e.g. the request timeout cancels its context after ~25s) is the
	// backend-lockup precursor this warning exists to surface, so the check must
	// not sit only on the success path. Deferred so it fires on every return.
	defer func() { db.logSlow(name, start, err) }()
	tx, err := on.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if content {
		defer db.content.Add(1) // deferred first, so it runs last: after the commit, or the rollback
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// A sample's writer-pool waiting is worth a warning when the average waiter queued
// contentionWarnMean (a slow hold) or all of them together contentionWarnTotal (one
// long stall among many short waits). A few milliseconds a waiter is the single
// writer doing its job: spike 1's Bridge queued a caller in nearly every sample of
// its seven-hour run, never averaging 6 ms a waiter or totalling 25 ms. Both read
// sums over a sample, which has no per-wait maximum: enough traffic of the same
// kind reaches the total (250 waits of 4 ms), and many short waits can average one
// slow hold below the mean.
const (
	contentionWarnMean  = 25 * time.Millisecond
	contentionWarnTotal = time.Second
)

// contentionLevel is the log level of one sample: waits callers started to queue,
// wait is the time of the waits that ended, and inUse whether the writer is held
// now. database/sql counts a wait when it starts and times it when it ends, so a
// caller still queued at one tick is timed at a later one, and time that arrives
// with no new waits is at least one caller's. New callers with none of them timed
// while the writer is held is how a stall looks at its first tick (callers with no
// deadline behind a writer that never frees are never timed at all); in ordinary
// traffic some wait of the sample has ended by the tick.
func contentionLevel(waits int64, wait time.Duration, inUse int) slog.Level {
	stalled := waits > 0 && wait == 0 && inUse > 0
	if stalled || wait >= contentionWarnTotal || wait/time.Duration(max(waits, 1)) >= contentionWarnMean {
		return slog.LevelWarn
	}
	return slog.LevelDebug
}

// startStatsSampler logs the writer pool's queueing every 30 s (sampleContention),
// so a slow hold starving the backend shows in the logs instead of only as an
// unexplained freeze. Stopped by Close.
func (db *DB) startStatsSampler() {
	ctx, cancel := context.WithCancel(context.Background())
	db.stopSampler = cancel
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		var last sql.DBStats
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				last = db.sampleContention(ctx, last)
			}
		}
	}()
}

// sampleContention logs the writer pool's waiting since last (its stats at the
// previous tick) at contentionLevel's level, and returns the stats it read. It logs
// when either counter moved: a caller queued behind a stall is counted at the tick
// it is waiting through and timed at the tick after it gets the writer or gives
// up, which can come with no new waits. So a stall warns at its first tick (new
// callers, none timed, the writer held) and again once its callers' waits end.
func (db *DB) sampleContention(ctx context.Context, last sql.DBStats) sql.DBStats {
	s := db.writer.Stats()
	if waits, wait := s.WaitCount-last.WaitCount, s.WaitDuration-last.WaitDuration; waits > 0 || wait > 0 {
		db.log.Log(ctx, contentionLevel(waits, wait, s.InUse), "db writer pool contention",
			"waits", waits,
			"wait_time", wait.String(),
			"in_use", s.InUse)
	}
	return s
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

// migrationNames lists the embedded migrations in the order they apply.
func migrationNames() ([]string, error) { return migrationNamesFrom(migrationsFS) }

func migrationNamesFrom(source fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(source, "migrations")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if !migrationName.MatchString(e.Name()) {
			return nil, fmt.Errorf("invalid migration filename %q: must match NNNN_*.sql", e.Name())
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// migrate applies embedded migrations that have not yet been recorded. It runs
// entirely on the writer connection (setup-time, single-threaded).
func (db *DB) migrate(ctx context.Context) error { return db.migrateFrom(ctx, migrationsFS) }

func (db *DB) migrateFrom(ctx context.Context, source fs.FS) error {
	if _, err := db.writer.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create migrations table: %w", err)
	}
	names, err := migrationNamesFrom(source)
	if err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, db.writer)
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	// unknown ends holding the recorded names this source does not ship.
	unknown := nameSet(applied)
	for _, name := range names {
		if unknown[name] {
			delete(unknown, name)
			continue // already applied
		}

		body, err := fs.ReadFile(source, "migrations/"+name)
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
	// A newer AudioSilo ran on this database (a rollback). This server still
	// starts, but it does not know the newer tables and can lose what they hold
	// (an older scanner indexes a podcast library as books, say).
	if len(unknown) > 0 {
		db.log.Warn("this database was used by a newer AudioSilo server: features it added may not work here and their data can be lost; run the newer version again, or restore a backup made before the upgrade",
			"unknown_migrations", strings.Join(slices.Sorted(maps.Keys(unknown)), ","))
	}
	return nil
}

// appliedMigrations lists the migrations a database records as applied, in order.
func appliedMigrations(ctx context.Context, conn *sql.DB) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// nameSet is names as a set.
func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}
