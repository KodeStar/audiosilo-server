package main

import (
	"context"
	"encoding/base64"
	"strings"
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

// TestRandomSecret pins randomSecret's entropy and encoding: it carries exactly
// nBytes of decoded entropy, is URL-safe (no '+', '/' or '=' padding), and is
// unpredictable (two calls differ).
func TestRandomSecret(t *testing.T) {
	for _, n := range []int{1, 16, 32} {
		s := randomSecret(n)

		decoded, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("randomSecret(%d) = %q does not base64url-decode: %v", n, s, err)
		}
		if len(decoded) != n {
			t.Fatalf("randomSecret(%d) decoded to %d bytes, want %d", n, len(decoded), n)
		}
		if strings.ContainsAny(s, "+/=") {
			t.Fatalf("randomSecret(%d) = %q contains non-URL-safe characters", n, s)
		}
	}

	// Two calls must not collide (vanishingly unlikely for crypto/rand entropy).
	if a, b := randomSecret(32), randomSecret(32); a == b {
		t.Fatalf("two randomSecret calls returned the same value %q", a)
	}
}
