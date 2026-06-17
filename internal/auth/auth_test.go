package auth

import (
	"context"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store"
)

func newTestService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, time.Now), ctx
}

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", hash); ok {
		t.Fatal("expected mismatch for wrong password")
	}
}

func TestNormalizeCodeForgiving(t *testing.T) {
	// O/I/L look-alikes and formatting must normalize to the same canonical form.
	a := normalizeCode("o1il-23ab")
	b := normalizeCode("0 1 1 1 2 3 A B")
	if a != b {
		t.Fatalf("normalize mismatch: %q vs %q", a, b)
	}
}

func TestCreateAuthenticateUser(t *testing.T) {
	s, ctx := newTestService(t)
	if _, err := s.CreateUser(ctx, "admin", "s3cret-pass", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AdminExists(ctx); !ok {
		t.Fatal("admin should exist")
	}
	u, err := s.Authenticate(ctx, "admin", "s3cret-pass")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.Role != RoleAdmin {
		t.Fatalf("role = %q", u.Role)
	}
	if _, err := s.Authenticate(ctx, "admin", "nope"); err != ErrInvalidCreds {
		t.Fatalf("expected ErrInvalidCreds, got %v", err)
	}
}

func TestTokenLifecycle(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	secret, err := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ResolveToken(ctx, secret, KindSession)
	if err != nil || got.ID != u.ID {
		t.Fatalf("resolve: %v user=%+v", err, got)
	}
	// Wrong kind must not resolve.
	if _, err := s.ResolveToken(ctx, secret, KindPairing); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken for wrong kind, got %v", err)
	}
	// After revoke it must fail.
	if err := s.RevokeToken(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveToken(ctx, secret, KindSession); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken after revoke, got %v", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now()
	s := New(db, func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	secret, _ := s.IssueToken(ctx, u.ID, KindPairing, "", time.Minute)
	now = now.Add(2 * time.Minute) // advance past expiry
	if _, err := s.ResolveToken(ctx, secret, KindPairing); err != ErrInvalidToken {
		t.Fatalf("expected expired token rejected, got %v", err)
	}
}

func TestAuthCodeRedeem(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	code, err := s.CreateAuthCode(ctx, admin.ID, "test", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.RedeemAuthCode(ctx, code)
	if err != nil || u.ID != admin.ID {
		t.Fatalf("redeem: %v user=%+v", err, u)
	}
	// max_uses = 1, so a second redemption must fail.
	if _, err := s.RedeemAuthCode(ctx, code); err != ErrInvalidCode {
		t.Fatalf("expected ErrInvalidCode on reuse, got %v", err)
	}
	if _, err := s.RedeemAuthCode(ctx, "BOGUS-CODE-0000-0000"); err != ErrInvalidCode {
		t.Fatalf("expected ErrInvalidCode for bogus, got %v", err)
	}
}
