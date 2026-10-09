package storetest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/store"
)

const insertUser = `INSERT INTO users(username, password_hash, role, created_at, updated_at)
	VALUES('u','x','user','t','t')`

func migrationCount(t *testing.T, db *store.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A copy is the database a new file migrates to, and each copy is its own.
func TestOpenIsMigratedAndIndependent(t *testing.T) {
	ctx := context.Background()
	fresh, err := store.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	want := migrationCount(t, fresh)

	a, b := Open(t), Open(t)
	if got := migrationCount(t, a); got != want {
		t.Fatalf("migrations = %d, want %d", got, want)
	}
	if _, err := a.ExecContext(ctx, insertUser); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a write to one copy shows in another: %d users", n)
	}
}

// Reads go to the read-only reader pool, as for any file-backed database: a
// write sent through a read method fails.
func TestOpenReaderIsReadOnly(t *testing.T) {
	db := Open(t)
	_, err := db.QueryContext(context.Background(), insertUser+" RETURNING id")
	if err == nil {
		t.Fatal("a write through the reader pool succeeded")
	}
}
