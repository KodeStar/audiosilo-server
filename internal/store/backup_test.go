package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVacuumIntoAndInspect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, filepath.Join(dir, "audiosilo.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users(username, password_hash, role, created_at, updated_at) VALUES('sam', '', 'user', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "copy.db")
	if err := db.VacuumInto(ctx, out); err != nil {
		t.Fatal(err)
	}
	info, err := Inspect(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Users != 1 || info.Schema == "" {
		t.Fatalf("info = %+v", info)
	}
	// The copy is complete on its own: no -wal beside it.
	if _, err := os.Stat(out + "-wal"); err == nil {
		t.Fatal("copy has a -wal file")
	}
	// VACUUM INTO never overwrites.
	if err := db.VacuumInto(ctx, out); err == nil {
		t.Fatal("vacuum into an existing file succeeded")
	}
}

func TestInspectRefuses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte("this is not sqlite at all, not even close"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, junk); !errors.Is(err, ErrNotADatabase) {
		t.Fatalf("junk: err = %v", err)
	}

	// A database from a newer server: a migration this one doesn't ship.
	newer := filepath.Join(dir, "newer.db")
	db, err := Open(ctx, newer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO schema_migrations(name, applied_at) VALUES('9999_future.sql', 'x')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Inspect(ctx, newer); !errors.Is(err, ErrNewerDatabase) || !errors.Is(err, ErrNotADatabase) {
		t.Fatalf("newer: err = %v", err)
	}

	// Some other SQLite database.
	other := filepath.Join(dir, "other.db")
	odb, err := Open(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := odb.ExecContext(ctx, `DROP TABLE schema_migrations`); err != nil {
		t.Fatal(err)
	}
	_ = odb.Close()
	if _, err := Inspect(ctx, other); !errors.Is(err, ErrNotADatabase) || errors.Is(err, ErrNewerDatabase) {
		t.Fatalf("other: err = %v", err)
	}

	if _, err := Inspect(ctx, filepath.Join(dir, "missing.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: err = %v", err)
	}
}
