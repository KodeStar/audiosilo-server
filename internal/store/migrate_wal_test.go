package store

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// A migration run that applied something leaves an empty WAL file behind; one
// that applied nothing leaves the file as it was.
func TestMigrateTruncatesWALOnlyWhenOneApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "wal.db")
	db, err := Open(ctx, dsn, WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	walSize := func() int64 {
		t.Helper()
		info, err := os.Stat(dsn + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	if got := walSize(); got != 0 {
		t.Fatalf("WAL after the first start's migrations = %d bytes, want 0", got)
	}

	// Well past the auto-checkpoint's 1000 pages: the checkpoint copies them into
	// the database and leaves the file at its size.
	if _, err := db.ExecContext(ctx, `CREATE TABLE wal_fill(b BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 2000)
		INSERT INTO wal_fill SELECT randomblob(4096) FROM n`); err != nil {
		t.Fatal(err)
	}
	grown := walSize()
	if grown < 4<<20 {
		t.Fatalf("WAL after the fill = %d bytes, want at least 4 MiB", grown)
	}

	if err := db.migrateFrom(ctx, migrationsFS); err != nil {
		t.Fatal(err)
	}
	if got := walSize(); got != grown {
		t.Fatalf("WAL after a run that applied nothing = %d bytes, want %d", got, grown)
	}

	source := fstest.MapFS{}
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		raw, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		source["migrations/"+name] = &fstest.MapFile{Data: raw}
	}
	source["migrations/9999_wal_test.sql"] = &fstest.MapFile{Data: []byte(`CREATE TABLE wal_test(value TEXT);`)}
	if err := db.migrateFrom(ctx, source); err != nil {
		t.Fatal(err)
	}
	if got := walSize(); got != 0 {
		t.Fatalf("WAL after a run that applied one = %d bytes, want 0", got)
	}
}
