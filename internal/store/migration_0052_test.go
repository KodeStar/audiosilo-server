package store

import (
	"context"
	"testing"
)

func TestMigration0052Podcasts(t *testing.T) {
	t.Parallel()
	dsn, exec, closeRaw := openBefore(t, "0052")
	exec(`INSERT INTO libraries(id,name,root,created_at) VALUES(1,'Books','/books','t')`)
	closeRaw()
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var kind string
	if err := db.QueryRowContext(context.Background(), `SELECT kind FROM libraries WHERE id=1`).Scan(&kind); err != nil || kind != "books" {
		t.Fatalf("kind %q %v", kind, err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE libraries SET kind='video' WHERE id=1`); err == nil {
		t.Fatal("invalid kind accepted")
	}
}
