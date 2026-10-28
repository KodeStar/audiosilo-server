package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// VacuumInto writes a consistent, compacted copy of the whole database to path
// (SQLite's VACUUM INTO), which must not exist yet. It only reads the database, in
// one read transaction, so writers carry on meanwhile (WAL) and the copy is the
// database as of the moment it began. It needs a connection of its own: the
// reader pool's query_only refuses it, and the one writer would hold every write
// back for as long as the copy takes. An in-memory database (tests) has no second
// connection to the same data, so it uses the writer.
func (db *DB) VacuumInto(ctx context.Context, path string) error {
	if !isMemoryDSN(db.dsn) {
		return VacuumFile(ctx, db.dsn, path)
	}
	if _, err := db.writer.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// VacuumFile writes a copy of the database file at path (with its -wal) into to,
// as VacuumInto does, without migrating or otherwise opening it as the server's
// database (a restore copies the database it is about to replace this way).
func VacuumFile(ctx context.Context, path, to string) error {
	c, err := sql.Open("sqlite", dsnPragmas(path))
	if err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	defer func() { _ = c.Close() }()
	c.SetMaxOpenConns(1)
	if _, err := c.ExecContext(ctx, `VACUUM INTO ?`, to); err != nil {
		return fmt.Errorf("vacuum into: %w", err)
	}
	return nil
}

// ErrNotADatabase marks a file that isn't an AudioSilo database this server can
// open: not SQLite, damaged, or not AudioSilo's. ErrNewerDatabase (which wraps it)
// marks one a newer server made.
var (
	ErrNotADatabase  = errors.New("not a usable AudioSilo database")
	ErrNewerDatabase = fmt.Errorf("%w: made by a newer server", ErrNotADatabase)
)

// Info describes a database file (Inspect).
type Info struct {
	// Schema is the newest migration it has applied.
	Schema string
	// Users is how many accounts it holds.
	Users int
}

// Inspect opens the SQLite file at path read-only and checks that this server can
// use it as its database: it is an AudioSilo database, every migration it has
// applied is one this server knows (a backup from a newer server would have tables
// this one can't read) and, when full, it passes SQLite's quick_check (which reads
// every page: seconds on a big database). Nothing is written to the file. A failed check wraps ErrNotADatabase (ErrNewerDatabase for the last).
func Inspect(ctx context.Context, path string, full bool) (Info, error) {
	var info Info
	if _, err := os.Stat(path); err != nil {
		return info, err
	}
	conn, err := openReadOnly(path)
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotADatabase, err)
	}
	defer func() { _ = conn.Close() }()

	if full {
		var check string
		if err := conn.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
			return info, fmt.Errorf("%w: %v", ErrNotADatabase, err)
		}
		if check != "ok" {
			return info, fmt.Errorf("%w: damaged (%s)", ErrNotADatabase, check)
		}
	}
	known, err := knownMigrations()
	if err != nil {
		return info, err
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return info, fmt.Errorf("%w: no schema record", ErrNotADatabase)
	}
	for _, name := range applied {
		if !known[name] {
			return info, fmt.Errorf("%w (%s)", ErrNewerDatabase, name)
		}
		info.Schema = name
	}
	if info.Schema == "" {
		return info, fmt.Errorf("%w: no schema record", ErrNotADatabase)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&info.Users); err != nil {
		return info, fmt.Errorf("%w: no accounts table", ErrNotADatabase)
	}
	return info, nil
}

// PendingMigrations lists the migrations this server would apply to the database
// file at path, in order, reading it read-only. It is empty when there is nothing
// to protect from them: no file, or a database with no migration recorded yet (a
// fresh install), and when the database is up to date.
func PendingMigrations(ctx context.Context, path string) ([]string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	conn, err := openReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var tables int
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&tables); err != nil {
		return nil, err
	}
	if tables == 0 {
		return nil, nil
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil || len(applied) == 0 {
		return nil, err
	}
	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	done := nameSet(applied)
	var pending []string
	for _, name := range names {
		if !done[name] {
			pending = append(pending, name)
		}
	}
	return pending, nil
}

// openReadOnly opens the SQLite file at path read-only on one connection. mode=ro
// opens without creating; immutable is not set, so a -wal beside the file (there
// should be none for a VACUUM INTO copy) is still read correctly.
func openReadOnly(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return conn, nil
}

// knownMigrations is the set of migration names this server ships.
func knownMigrations() (map[string]bool, error) {
	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	return nameSet(names), nil
}
