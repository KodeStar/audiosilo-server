package auth

import (
	"context"
	"sync"
	"sync/atomic"
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

func TestOptionalPasswordForNonAdmins(t *testing.T) {
	s, ctx := newTestService(t)
	// A non-admin may be created without a password (auth-code pairing only).
	u, err := s.CreateUser(ctx, "kid", "", RoleUser)
	if err != nil {
		t.Fatalf("create password-less user: %v", err)
	}
	if u.HasPassword {
		t.Fatal("password-less user should report HasPassword=false")
	}
	// Such an account can never password-login, whatever is presented.
	if _, err := s.Authenticate(ctx, "kid", ""); err != ErrInvalidCreds {
		t.Fatalf("empty password should be rejected, got %v", err)
	}
	if _, err := s.Authenticate(ctx, "kid", "guess"); err != ErrInvalidCreds {
		t.Fatalf("any password should be rejected, got %v", err)
	}
	// Admins still require a password.
	if _, err := s.CreateUser(ctx, "boss", "", RoleAdmin); err == nil {
		t.Fatal("expected error creating password-less admin")
	}
}

func TestSetRoleGuards(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	kid, _ := s.CreateUser(ctx, "kid", "", RoleUser)

	// Promoting a password-less account to admin is refused until it has a password.
	if err := s.SetRole(ctx, kid.ID, RoleAdmin); err != ErrAdminNeedsPassword {
		t.Fatalf("expected ErrAdminNeedsPassword, got %v", err)
	}
	if err := s.SetPassword(ctx, kid.ID, "kid-password"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, kid.ID, RoleAdmin); err != nil {
		t.Fatalf("promote after setting password: %v", err)
	}

	// Now there are two admins; demoting the original is allowed.
	if err := s.SetRole(ctx, admin.ID, RoleUser); err != nil {
		t.Fatalf("demote with another admin present: %v", err)
	}
	// kid is the last admin — demoting/disabling must be refused.
	if err := s.SetRole(ctx, kid.ID, RoleUser); err != ErrLastAdmin {
		t.Fatalf("expected ErrLastAdmin on demote, got %v", err)
	}
	if err := s.SetDisabled(ctx, kid.ID, true); err != ErrLastAdmin {
		t.Fatalf("expected ErrLastAdmin on disable, got %v", err)
	}
}

func TestClearingAdminPasswordRefused(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	if err := s.SetPassword(ctx, admin.ID, ""); err != ErrAdminNeedsPassword {
		t.Fatalf("expected ErrAdminNeedsPassword clearing admin password, got %v", err)
	}
}

func TestListAndRevokeAuthCodes(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	if _, err := s.CreateAuthCode(ctx, admin.ID, "invite", 1, time.Hour); err != nil {
		t.Fatal(err)
	}
	codes, err := s.ListAuthCodes(ctx, admin.ID)
	if err != nil || len(codes) != 1 {
		t.Fatalf("list auth codes: %v len=%d", err, len(codes))
	}
	if codes[0].Label != "invite" || codes[0].MaxUses != 1 || codes[0].ExpiresAt == "" {
		t.Fatalf("unexpected code metadata: %+v", codes[0])
	}
	if err := s.RevokeAuthCode(ctx, codes[0].ID); err != nil {
		t.Fatal(err)
	}
	if codes, _ := s.ListAuthCodes(ctx, admin.ID); len(codes) != 0 {
		t.Fatalf("expected no codes after revoke, got %d", len(codes))
	}
}

func TestLastSeenTracksTokenActivity(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	if got, _ := s.GetUser(ctx, u.ID); got.LastSeenAt != "" {
		t.Fatalf("fresh user should have no last-seen, got %q", got.LastSeenAt)
	}
	secret, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	if _, err := s.ResolveToken(ctx, secret, KindSession); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUser(ctx, u.ID)
	if got.LastSeenAt == "" {
		t.Fatal("last-seen should be set after an authenticated request")
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

// TestAuthCodeRedeemConcurrent guards the use-counter against a check-then-
// increment race: with max_uses = 2, exactly two of many concurrent redemptions
// may succeed (the WHERE-guarded UPDATE is the single atomic claim point).
func TestAuthCodeRedeemConcurrent(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	code, err := s.CreateAuthCode(ctx, admin.ID, "test", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.RedeemAuthCode(ctx, code); err == nil {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("redeemed %d times, want exactly 2 (cap must hold under concurrency)", ok)
	}
}

func TestRedeemStampsRedeemedAt(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	code, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	if codes, _ := s.ListAuthCodes(ctx, u.ID); codes[0].RedeemedAt != "" {
		t.Fatalf("fresh invite should be pending (no redeemed_at), got %q", codes[0].RedeemedAt)
	}
	if _, err := s.RedeemAuthCode(ctx, code); err != nil {
		t.Fatal(err)
	}
	codes, _ := s.ListAuthCodes(ctx, u.ID)
	if codes[0].RedeemedAt == "" {
		t.Fatal("redeemed_at should be set after the first redemption (accepted)")
	}
}

func TestRotateAuthCode(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	old, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	if _, err := s.RedeemAuthCode(ctx, old); err != nil {
		t.Fatal(err)
	}
	id := mustOneInviteID(t, s, ctx, u.ID)

	fresh, err := s.RotateAuthCode(ctx, id, 5, 0)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh == old {
		t.Fatal("rotate must produce a new secret")
	}
	// The old code stops working; the fresh one redeems; rotating made it pending.
	if _, err := s.RedeemAuthCode(ctx, old); err != ErrInvalidCode {
		t.Fatalf("old code after rotate = %v, want ErrInvalidCode", err)
	}
	if _, err := s.RedeemAuthCode(ctx, fresh); err != nil {
		t.Fatalf("fresh code redeem: %v", err)
	}
	// Rotating still leaves a single invite row (in place, not a new one).
	if codes, _ := s.ListAuthCodes(ctx, u.ID); len(codes) != 1 {
		t.Fatalf("expected exactly one invite row after rotate, got %d", len(codes))
	}
	// A non-existent id is a not-found.
	if _, err := s.RotateAuthCode(ctx, 999999, 5, 0); err != ErrNotFound {
		t.Fatalf("rotate missing id = %v, want ErrNotFound", err)
	}
}

func TestRotateRefusesRecoveryCode(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	if _, err := s.GenerateRecoveryCode(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	// The recovery row exists but is not an invite; rotate must not touch it.
	var id int64
	s.db.QueryRowContext(ctx, `SELECT id FROM auth_codes WHERE user_id = ? AND kind = ?`, u.ID, CodeRecovery).Scan(&id)
	if _, err := s.RotateAuthCode(ctx, id, 5, 0); err != ErrNotFound {
		t.Fatalf("rotate recovery code = %v, want ErrNotFound", err)
	}
}

func TestSupersedePendingInvites(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	accepted, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	if _, err := s.RedeemAuthCode(ctx, accepted); err != nil { // uses > 0
		t.Fatal(err)
	}
	if _, err := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0); err != nil { // pending, uses == 0
		t.Fatal(err)
	}
	if err := s.SupersedePendingInvites(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	// Only the never-redeemed (pending) invite is dropped; the accepted one stays
	// as history.
	codes, _ := s.ListAuthCodes(ctx, u.ID)
	if len(codes) != 1 || codes[0].RedeemedAt == "" {
		t.Fatalf("expected one accepted invite to survive supersede, got %+v", codes)
	}
}

func TestRecoveryCodeLifecycle(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	if got, _ := s.GetUser(ctx, u.ID); got.HasRecovery {
		t.Fatal("fresh user should have no recovery code")
	}
	code, err := s.GenerateRecoveryCode(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(ctx, u.ID); !got.HasRecovery {
		t.Fatal("HasRecovery should be true after generating a recovery code")
	}
	// Recovery codes are excluded from the admin invite list.
	if codes, _ := s.ListAuthCodes(ctx, u.ID); len(codes) != 0 {
		t.Fatalf("recovery code must not appear as an invite, got %d", len(codes))
	}
	// Durable & reusable: it redeems repeatedly (unlike a bounded invite).
	for i := 0; i < 3; i++ {
		if _, err := s.RedeemAuthCode(ctx, code); err != nil {
			t.Fatalf("recovery redeem #%d: %v", i+1, err)
		}
	}
	// Regenerating replaces the old one (old fails, exactly one row remains).
	fresh, _ := s.GenerateRecoveryCode(ctx, u.ID)
	if _, err := s.RedeemAuthCode(ctx, code); err != ErrInvalidCode {
		t.Fatalf("old recovery code after regen = %v, want ErrInvalidCode", err)
	}
	if _, err := s.RedeemAuthCode(ctx, fresh); err != nil {
		t.Fatalf("fresh recovery code: %v", err)
	}
	// Clearing removes it.
	if err := s.ClearRecoveryCode(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(ctx, u.ID); got.HasRecovery {
		t.Fatal("HasRecovery should be false after clearing")
	}
}

// mustOneInviteID returns the id of the single invite row for a user.
func mustOneInviteID(t *testing.T, s *Service, ctx context.Context, userID int64) int64 {
	t.Helper()
	codes, err := s.ListAuthCodes(ctx, userID)
	if err != nil || len(codes) != 1 {
		t.Fatalf("expected one invite, got %d (err=%v)", len(codes), err)
	}
	return codes[0].ID
}
