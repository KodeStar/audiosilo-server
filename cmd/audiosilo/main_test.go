package main

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/store"
)

// ensureAdmin must create the admin from DATABASE state, not config-file
// existence — so dropping in a config.yaml before the first start can't suppress
// first-run admin creation — and it must be idempotent on later starts.
func TestEnsureAdminKeysOffDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	authSvc := auth.New(db, time.Now)
	cfg := config.Default(t.TempDir())

	if err := ensureAdmin(ctx, cfg, authSvc); err != nil {
		t.Fatal(err)
	}
	if ok, _ := authSvc.AdminExists(ctx); !ok {
		t.Fatal("expected an admin to be created on first call")
	}
	users, _ := authSvc.ListUsers(ctx)
	if len(users) != 1 {
		t.Fatalf("expected exactly 1 user after bootstrap, got %d", len(users))
	}

	// Idempotent: a second call (e.g. a normal restart) must not create another.
	if err := ensureAdmin(ctx, cfg, authSvc); err != nil {
		t.Fatal(err)
	}
	if users, _ := authSvc.ListUsers(ctx); len(users) != 1 {
		t.Fatalf("ensureAdmin should be idempotent, got %d users", len(users))
	}
}
