// Package storetest opens test databases quickly. Migrating a new database runs
// every migration, which under the race detector costs about a second (the
// pure-Go SQLite is instrumented throughout), so a package whose every test
// migrates its own spends most of its run doing that. Open migrates one
// database per test binary and gives each test a copy of the file instead.
//
// A copy is what store.Open makes of a new file: file-backed, so reads go to the
// read-only reader pool and a write sent through a read method fails as in
// production (where :memory:, one pool, would hide it).
package storetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

// template is the migrated database file, built once per test binary.
var template = sync.OnceValues(build)

// Open returns a migrated database in t's temp dir, closed when the test ends.
func Open(t testing.TB) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), Path(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Path writes a migrated database file, audiosilo.db, into a new temp dir of
// t's and returns its path, for a test that opens it itself.
func Path(t testing.TB) string {
	t.Helper()
	b, err := template()
	if err != nil {
		t.Fatalf("migrate the template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "audiosilo.db")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// build migrates a database and reads its file back. Closing it checkpoints the
// WAL into the file and removes the -wal, so the one file is the whole database
// (still in WAL mode, as every copy will be opened).
func build() ([]byte, error) {
	dir, err := os.MkdirTemp("", "audiosilo-storetest")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		return nil, err
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, fmt.Errorf("closing left %s-wal behind", path)
	}
	return os.ReadFile(path)
}
