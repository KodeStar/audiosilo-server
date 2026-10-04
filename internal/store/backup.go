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
// use it as its database: it passes SQLite's quick_check, it is an AudioSilo
// database, and every migration it has applied is one this server knows (a backup
// from a newer server would have tables this one can't read). Nothing is written to
// the file. A failed check wraps ErrNotADatabase (ErrNewerDatabase for the last).
func Inspect(ctx context.Context, path string) (Info, error) {
	var info Info
	if _, err := os.Stat(path); err != nil {
		return info, err
	}
	// mode=ro opens without creating; immutable is not set, so a -wal beside the file
	// (there should be none for a VACUUM INTO copy) is still read correctly.
	conn, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro")
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotADatabase, err)
	}
	defer func() { _ = conn.Close() }()
	conn.SetMaxOpenConns(1)

	var check string
	if err := conn.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return info, fmt.Errorf("%w: %v", ErrNotADatabase, err)
	}
	if check != "ok" {
		return info, fmt.Errorf("%w: damaged (%s)", ErrNotADatabase, check)
	}
	known, err := knownMigrations()
	if err != nil {
		return info, err
	}
	rows, err := conn.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		return info, fmt.Errorf("%w: no schema record", ErrNotADatabase)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return info, err
		}
		if !known[name] {
			return info, fmt.Errorf("%w (%s)", ErrNewerDatabase, name)
		}
		info.Schema = name
	}
	if err := rows.Err(); err != nil {
		return info, err
	}
	if info.Schema == "" {
		return info, fmt.Errorf("%w: no schema record", ErrNotADatabase)
	}
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&info.Users); err != nil {
		return info, fmt.Errorf("%w: no accounts table", ErrNotADatabase)
	}
	return info, nil
}

// knownMigrations is the set of migration names this server ships.
func knownMigrations() (map[string]bool, error) {
	names, err := migrationNames()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}
