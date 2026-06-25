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
	"regexp"
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

// DB wraps *sql.DB with AudioSilo-specific helpers.
type DB struct {
	*sql.DB
}

// Open opens (creating if needed) the SQLite database at dsn and applies any
// pending migrations. Pass ":memory:" for tests. Recommended pragmas for a
// server workload are enabled via the connection string.
func Open(ctx context.Context, dsn string) (*DB, error) {
	// WAL for concurrent readers, busy_timeout to ride out brief locks, and
	// foreign_keys for referential integrity. foreign_keys defaults OFF per
	// SQLite connection, and the schema relies on ~28 ON DELETE CASCADE rules,
	// so these pragmas must always be applied — append with the right separator
	// rather than skipping them when the caller's DSN already carries query
	// params (which would silently disable every cascade delete).
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	conn := dsn + sep + "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	sqlDB, err := sql.Open("sqlite", conn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite writers are serialized; a single connection avoids "database is
	// locked" churn while WAL still allows concurrent reads on separate conns.
	sqlDB.SetMaxOpenConns(1)
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	db := &DB{sqlDB}
	if err := db.migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// migrate applies embedded migrations that have not yet been recorded.
func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
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
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE name = ?`, name).Scan(&exists); err == nil {
			continue // already applied
		} else if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
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
