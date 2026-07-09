package auth

import (
	"context"
	"errors"
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

// newTestServiceWithClock builds a service on a frozen clock; tests advance it
// through the returned pointer to cross expiry boundaries.
func newTestServiceWithClock(t *testing.T) (*Service, context.Context, *time.Time) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now()
	return New(db, func() time.Time { return now }), ctx, &now
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

func TestDeleteUser(t *testing.T) {
	s, ctx := newTestService(t)
	admin, err := s.CreateUser(ctx, "admin", "s3cret-pass", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateUser(ctx, "member", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}

	// Deleting a normal user succeeds and the row is gone.
	if err := s.DeleteUser(ctx, member.ID); err != nil {
		t.Fatalf("delete member: %v", err)
	}
	if _, err := s.GetUser(ctx, member.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted member still present: %v", err)
	}

	// Deleting an unknown id is ErrNotFound.
	if err := s.DeleteUser(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete unknown = %v, want ErrNotFound", err)
	}

	// The last enabled admin can't be deleted (lockout guard).
	if err := s.DeleteUser(ctx, admin.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("delete last admin = %v, want ErrLastAdmin", err)
	}

	// With a second admin, deleting one of them is allowed.
	admin2, err := s.CreateUser(ctx, "admin2", "s3cret-pass", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, admin2.ID); err != nil {
		t.Fatalf("delete admin2 with another admin present: %v", err)
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

// TestPasswordLengthBoundary pins the MinPasswordLen boundary through both
// CreateUser and SetPassword: a 7-char password is rejected, an 8-char one is
// accepted, and an empty password is allowed (password-less non-admin).
func TestPasswordLengthBoundary(t *testing.T) {
	s, ctx := newTestService(t)

	// CreateUser: 7 chars (one below the minimum) is rejected.
	if _, err := s.CreateUser(ctx, "short", "1234567", RoleUser); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("CreateUser with 7-char password: err = %v, want ErrPasswordTooShort", err)
	}
	// CreateUser: exactly 8 chars is accepted.
	u, err := s.CreateUser(ctx, "exact", "12345678", RoleUser)
	if err != nil {
		t.Fatalf("CreateUser with 8-char password: %v", err)
	}
	// CreateUser: empty password is allowed for a non-admin (password-less).
	if _, err := s.CreateUser(ctx, "nopass", "", RoleUser); err != nil {
		t.Fatalf("CreateUser with empty password (non-admin): %v", err)
	}

	// SetPassword: 7 chars is rejected.
	if err := s.SetPassword(ctx, u.ID, "1234567"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("SetPassword with 7-char password: err = %v, want ErrPasswordTooShort", err)
	}
	// SetPassword: exactly 8 chars is accepted.
	if err := s.SetPassword(ctx, u.ID, "abcdefgh"); err != nil {
		t.Fatalf("SetPassword with 8-char password: %v", err)
	}
	// SetPassword: clearing to empty is allowed for a non-admin.
	if err := s.SetPassword(ctx, u.ID, ""); err != nil {
		t.Fatalf("SetPassword with empty password (non-admin): %v", err)
	}
}

// TestCheckPassword pins the self-service password challenge: the correct
// password matches, a wrong one and a password-less account both report
// ErrInvalidCreds, and a missing user reports ErrNotFound.
func TestCheckPassword(t *testing.T) {
	s, ctx := newTestService(t)

	u, err := s.CreateUser(ctx, "alice", "correct-pw", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckPassword(ctx, u.ID, "correct-pw"); err != nil {
		t.Fatalf("correct password: err = %v, want nil", err)
	}
	if err := s.CheckPassword(ctx, u.ID, "wrong-pw"); !errors.Is(err, ErrInvalidCreds) {
		t.Fatalf("wrong password: err = %v, want ErrInvalidCreds", err)
	}

	// A password-less account (empty hash) can never be challenged.
	pwless, err := s.CreateUser(ctx, "bob", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckPassword(ctx, pwless.ID, ""); !errors.Is(err, ErrInvalidCreds) {
		t.Fatalf("password-less account: err = %v, want ErrInvalidCreds", err)
	}

	// A non-existent user id resolves to ErrNotFound (no row to scan).
	if err := s.CheckPassword(ctx, 999999, "anything"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: err = %v, want ErrNotFound", err)
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
	s, ctx, now := newTestServiceWithClock(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	secret, _ := s.IssueToken(ctx, u.ID, KindPairing, "", time.Minute)
	*now = now.Add(2 * time.Minute) // advance past expiry
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
	// kid is the last admin - demoting/disabling must be refused.
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

// pairThrough resolves a code and mints its linked pairing token - the redeem
// step of the pairing flow - returning the token secret for ConsumePairingToken.
func pairThrough(t *testing.T, s *Service, ctx context.Context, code string) string {
	t.Helper()
	rc, err := s.ResolveAuthCode(ctx, code)
	if err != nil {
		t.Fatalf("resolve code: %v", err)
	}
	secret, err := s.IssuePairingToken(ctx, rc)
	if err != nil {
		t.Fatalf("issue pairing token: %v", err)
	}
	return secret
}

func TestResolveAuthCode(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	code, err := s.CreateAuthCode(ctx, admin.ID, "test", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := s.ResolveAuthCode(ctx, code)
	if err != nil || rc.User.ID != admin.ID {
		t.Fatalf("resolve: %v rc=%+v", err, rc)
	}
	if rc.Kind != CodeInvite || rc.MaxUses != 1 || rc.Uses != 0 {
		t.Fatalf("unexpected code metadata: %+v", rc)
	}
	if n := rc.UsesRemaining(); n == nil || *n != 1 {
		t.Fatalf("UsesRemaining = %v, want 1", n)
	}
	// Resolving consumes nothing: it repeats freely and never stamps acceptance.
	if _, err := s.ResolveAuthCode(ctx, code); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if codes, _ := s.ListAuthCodes(ctx, admin.ID); codes[0].Uses != 0 || codes[0].RedeemedAt != "" {
		t.Fatalf("resolve must not consume a use or stamp accepted: %+v", codes[0])
	}
	if _, err := s.ResolveAuthCode(ctx, "BOGUS-CODE-0000-0000"); err != ErrInvalidCode {
		t.Fatalf("expected ErrInvalidCode for bogus, got %v", err)
	}
	// An exhausted code is refused at resolve, so a QR that could never
	// exchange is never rendered.
	if _, err := s.ConsumePairingToken(ctx, pairThrough(t, s, ctx, code)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, code); err != ErrInvalidCode {
		t.Fatalf("exhausted code resolve = %v, want ErrInvalidCode", err)
	}
}

// TestConsumePairingTokenClaimsUse: the invite use is claimed at exchange, not
// at redeem, and a failed claim burns nothing (allowed + denied).
func TestConsumePairingTokenClaimsUse(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	code, _ := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	tok := pairThrough(t, s, ctx, code)
	got, err := s.ConsumePairingToken(ctx, tok)
	if err != nil || got.ID != u.ID {
		t.Fatalf("consume: %v user=%+v", err, got)
	}
	codes, _ := s.ListAuthCodes(ctx, u.ID)
	if codes[0].Uses != 1 || codes[0].RedeemedAt == "" {
		t.Fatalf("exchange must claim a use and stamp accepted: %+v", codes[0])
	}
	// The invite is spent, so the same token cannot pair another device - and
	// the refusal must not push uses past the cap.
	if _, err := s.ConsumePairingToken(ctx, tok); !errors.Is(err, ErrCodeExhausted) {
		t.Fatalf("expected ErrCodeExhausted on reuse, got %v", err)
	}
	if codes, _ := s.ListAuthCodes(ctx, u.ID); codes[0].Uses != 1 {
		t.Fatalf("failed consume must not burn a use: %+v", codes[0])
	}
}

// TestPairingClaimConcurrent guards the use-counter against a check-then-
// increment race: with max_uses = 2, exactly two of many concurrent exchanges
// of the same linked token may succeed (the WHERE-guarded UPDATE is the single
// atomic claim point).
func TestPairingClaimConcurrent(t *testing.T) {
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "admin", "pw-pw-pw-pw", RoleAdmin)
	code, err := s.CreateAuthCode(ctx, admin.ID, "test", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok := pairThrough(t, s, ctx, code)
	const workers = 8
	var ok int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ConsumePairingToken(ctx, tok); err == nil {
				atomic.AddInt64(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("exchanged %d times, want exactly 2 (cap must hold under concurrency)", ok)
	}
}

// TestUnlinkedPairingTokenSingleUse: tokens minted without a parent code
// (/auth/pair, demo) stay strictly single-use (allowed + denied).
func TestUnlinkedPairingTokenSingleUse(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	tok, err := s.IssueToken(ctx, u.ID, KindPairing, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumePairingToken(ctx, tok)
	if err != nil || got.ID != u.ID {
		t.Fatalf("consume: %v user=%+v", err, got)
	}
	if _, err := s.ConsumePairingToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken on second consume, got %v", err)
	}
}

func TestExchangeStampsRedeemedAt(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	code, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	tok := pairThrough(t, s, ctx, code)
	// Redeeming (opening the link) leaves the invite pending.
	if codes, _ := s.ListAuthCodes(ctx, u.ID); codes[0].RedeemedAt != "" {
		t.Fatalf("invite should stay pending until a device pairs, got %q", codes[0].RedeemedAt)
	}
	if _, err := s.ConsumePairingToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	codes, _ := s.ListAuthCodes(ctx, u.ID)
	first := codes[0].RedeemedAt
	if first == "" {
		t.Fatal("redeemed_at should be set after the first exchange (accepted)")
	}
	// COALESCE keeps the first acceptance stamp across later exchanges.
	if _, err := s.ConsumePairingToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if codes, _ := s.ListAuthCodes(ctx, u.ID); codes[0].RedeemedAt != first {
		t.Fatalf("redeemed_at must keep the first stamp, got %q want %q", codes[0].RedeemedAt, first)
	}
}

// TestExpiredInviteRefusesExchange: an invite-derived token dies with the
// invite's expiry - the claim rejects it with ErrCodeExpired and burns nothing.
func TestExpiredInviteRefusesExchange(t *testing.T) {
	s, ctx, now := newTestServiceWithClock(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	code, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, time.Hour)
	tok := pairThrough(t, s, ctx, code)
	if _, err := s.ConsumePairingToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Hour) // advance past the invite's expiry
	if _, err := s.ConsumePairingToken(ctx, tok); !errors.Is(err, ErrCodeExpired) {
		t.Fatalf("expected ErrCodeExpired past invite expiry, got %v", err)
	}
	if codes, _ := s.ListAuthCodes(ctx, u.ID); codes[0].Uses != 1 {
		t.Fatalf("expired refusal must not burn a use: %+v", codes[0])
	}
}

// TestRecoveryPairingTokenTTL: a recovery-derived token is bounded by its own
// TTL, not the code's (infinite) lifetime - otherwise pasting a recovery code
// into the connect page would mint an eternal QR.
func TestRecoveryPairingTokenTTL(t *testing.T) {
	s, ctx, now := newTestServiceWithClock(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	code, err := s.GenerateRecoveryCode(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	tok := pairThrough(t, s, ctx, code)
	// Within the window it exchanges repeatedly (max_uses = 0, nothing to claim
	// down); past the TTL it is refused.
	if _, err := s.ConsumePairingToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumePairingToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(recoveryPairingTTL + time.Minute)
	if _, err := s.ConsumePairingToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken past TTL, got %v", err)
	}
}

// TestRevokeAuthCodeKillsLinkedTokens: deleting an invite cascades to its
// pairing tokens - a revoked invite's QR must not keep pairing devices.
func TestRevokeAuthCodeKillsLinkedTokens(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	code, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	tok := pairThrough(t, s, ctx, code)
	if err := s.RevokeAuthCode(ctx, mustOneInviteID(t, s, ctx, u.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumePairingToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected ErrInvalidToken after code revoke, got %v", err)
	}
}

func TestRotateAuthCode(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	old, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	oldTok := pairThrough(t, s, ctx, old)
	id := mustOneInviteID(t, s, ctx, u.ID)

	fresh, err := s.RotateAuthCode(ctx, id)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if fresh == old {
		t.Fatal("rotate must produce a new secret")
	}
	// The old code stops working - and so does any pairing token (QR) minted
	// from it, since rotate revokes the code's linked tokens in the same tx.
	if _, err := s.ResolveAuthCode(ctx, old); err != ErrInvalidCode {
		t.Fatalf("old code after rotate = %v, want ErrInvalidCode", err)
	}
	if _, err := s.ConsumePairingToken(ctx, oldTok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("old QR token after rotate = %v, want ErrInvalidToken", err)
	}
	if _, err := s.ResolveAuthCode(ctx, fresh); err != nil {
		t.Fatalf("fresh code resolve: %v", err)
	}
	// Rotating still leaves a single invite row (in place, not a new one).
	if codes, _ := s.ListAuthCodes(ctx, u.ID); len(codes) != 1 {
		t.Fatalf("expected exactly one invite row after rotate, got %d", len(codes))
	}
	// A non-existent id is a not-found.
	if _, err := s.RotateAuthCode(ctx, 999999); err != ErrNotFound {
		t.Fatalf("rotate missing id = %v, want ErrNotFound", err)
	}
}

// TestRotatePreservesLifetime: rotating ("Resend") keeps the invite's max_uses
// and renews its expiry window, rather than silently resetting to defaults.
func TestRotatePreservesLifetime(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	if _, err := s.CreateAuthCode(ctx, u.ID, "invite", 50, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	id := mustOneInviteID(t, s, ctx, u.ID)
	if _, err := s.RotateAuthCode(ctx, id); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	codes, _ := s.ListAuthCodes(ctx, u.ID)
	if len(codes) != 1 || codes[0].MaxUses != 50 {
		t.Fatalf("rotate must preserve max_uses=50, got %+v", codes)
	}
	if codes[0].ExpiresAt == "" {
		t.Fatal("rotate must renew (not drop) the expiry window")
	}
	if exp, err := time.Parse(time.RFC3339, codes[0].ExpiresAt); err != nil || time.Until(exp) < 20*24*time.Hour {
		t.Fatalf("rotate should renew ~30d expiry, got %q", codes[0].ExpiresAt)
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
	if _, err := s.RotateAuthCode(ctx, id); err != ErrNotFound {
		t.Fatalf("rotate recovery code = %v, want ErrNotFound", err)
	}
}

// TestCreateInviteSupersedesActive: minting supersedes the user's still-redeemable
// invites (so there is one active invite), but leaves spent/used-up ones as
// history. A partially-redeemed multi-use invite counts as active and is replaced.
func TestCreateInviteSupersedesActive(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	// A spent (used-up) invite is history and must survive a new mint. Spending
	// now means a device exchanged, not that the link was opened.
	spent, _ := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	if _, err := s.ConsumePairingToken(ctx, pairThrough(t, s, ctx, spent)); err != nil { // uses=1 >= max → used up
		t.Fatal(err)
	}
	// A still-redeemable invite is active and must be superseded by a new mint.
	active, _ := s.CreateAuthCode(ctx, u.ID, "invite", 5, 0)
	activeTok := pairThrough(t, s, ctx, active)
	fresh, err := s.CreateInvite(ctx, u.ID, "invite", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, active); err != ErrInvalidCode {
		t.Fatalf("superseded active invite = %v, want ErrInvalidCode", err)
	}
	// The superseded invite's outstanding pairing token dies with it (cascade).
	if _, err := s.ConsumePairingToken(ctx, activeTok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("superseded invite's QR token = %v, want ErrInvalidToken", err)
	}
	if _, err := s.ResolveAuthCode(ctx, fresh); err != nil {
		t.Fatalf("fresh invite should resolve: %v", err)
	}
	// Two invite rows remain: the spent history one and the fresh one.
	if codes, _ := s.ListAuthCodes(ctx, u.ID); len(codes) != 2 {
		t.Fatalf("expected spent(history)+fresh = 2 invites, got %d", len(codes))
	}
}

// TestRecoveryRedeemReturnsOwner: a recovery code redeems as its owner, never
// another account - guards the user binding (account-takeover class).
func TestRecoveryRedeemReturnsOwner(t *testing.T) {
	s, ctx := newTestService(t)
	alice, _ := s.CreateUser(ctx, "alice", "", RoleUser)
	if _, err := s.CreateUser(ctx, "bob", "", RoleUser); err != nil {
		t.Fatal(err)
	}
	code, err := s.GenerateRecoveryCode(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Both steps of the pairing flow must bind to the owner: resolve and the
	// exchange of the resulting token.
	rc, err := s.ResolveAuthCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if rc.User.ID != alice.ID {
		t.Fatalf("recovery resolved as user %d, want owner %d", rc.User.ID, alice.ID)
	}
	got, err := s.ConsumePairingToken(ctx, pairThrough(t, s, ctx, code))
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != alice.ID {
		t.Fatalf("recovery exchanged as user %d, want owner %d", got.ID, alice.ID)
	}
}

// TestRedeemDisabledRejectedWithoutBurningUse: a disabled account's code is
// rejected at BOTH steps - resolve (opening the link) and exchange (a pairing
// token minted before the disable) - before any use is consumed or the invite
// is marked accepted, and access is restored on re-enable. Covers invites and
// recovery codes (security-critical: both an allowed and a denied path).
func TestRedeemDisabledRejectedWithoutBurningUse(t *testing.T) {
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "", RoleUser)
	invite, _ := s.CreateAuthCode(ctx, u.ID, "invite", 1, 0)
	recovery, err := s.GenerateRecoveryCode(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Mint the linked pairing token while enabled: the disable must gate the
	// exchange step too, not just redeem.
	tok := pairThrough(t, s, ctx, invite)
	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, invite); err != ErrInvalidCode {
		t.Fatalf("disabled invite resolve = %v, want ErrInvalidCode", err)
	}
	if _, err := s.ResolveAuthCode(ctx, recovery); err != ErrInvalidCode {
		t.Fatalf("disabled recovery resolve = %v, want ErrInvalidCode", err)
	}
	if _, err := s.ConsumePairingToken(ctx, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled exchange = %v, want ErrInvalidToken", err)
	}
	// The rejected attempts must not have consumed the invite's single use or
	// stamped it accepted.
	if codes, _ := s.ListAuthCodes(ctx, u.ID); len(codes) != 1 || codes[0].Uses != 0 || codes[0].RedeemedAt != "" {
		t.Fatalf("disabled attempt burned a use or stamped accepted: %+v", codes)
	}
	// Re-enabling restores access: the held QR token exchanges and claims the
	// invite's use, and the recovery code resolves again.
	if err := s.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumePairingToken(ctx, tok); err != nil {
		t.Fatalf("exchange after re-enable: %v", err)
	}
	if _, err := s.ResolveAuthCode(ctx, recovery); err != nil {
		t.Fatalf("recovery resolve after re-enable: %v", err)
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
	// Durable & reusable: it pairs repeatedly (unlike a bounded invite) - each
	// resolve mints a token and each token exchanges.
	for i := 0; i < 3; i++ {
		if _, err := s.ConsumePairingToken(ctx, pairThrough(t, s, ctx, code)); err != nil {
			t.Fatalf("recovery pairing #%d: %v", i+1, err)
		}
	}
	// Regenerating replaces the old one: the old code fails, and any pairing
	// token minted from it dies via the delete cascade.
	oldTok := pairThrough(t, s, ctx, code)
	fresh, _ := s.GenerateRecoveryCode(ctx, u.ID)
	if _, err := s.ResolveAuthCode(ctx, code); err != ErrInvalidCode {
		t.Fatalf("old recovery code after regen = %v, want ErrInvalidCode", err)
	}
	if _, err := s.ConsumePairingToken(ctx, oldTok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("old recovery QR token after regen = %v, want ErrInvalidToken", err)
	}
	if _, err := s.ResolveAuthCode(ctx, fresh); err != nil {
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

// TestAPITokenLifecycle covers minting, resolving, listing and revoking a
// personal API key at the auth layer: an api key resolves as kind=api (and not
// as a session), lists as metadata only, and stops resolving once revoked.
func TestAPITokenLifecycle(t *testing.T) {
	s, ctx := newTestService(t)
	user, err := s.CreateUser(ctx, "user", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}

	secret, meta, err := s.IssueAPIToken(ctx, user.ID, "cron job")
	if err != nil {
		t.Fatal(err)
	}
	if secret == "" || meta.ID == 0 || meta.Label != "cron job" || meta.CreatedAt == "" {
		t.Fatalf("unexpected mint meta: %+v", meta)
	}
	if meta.LastSeen != nil {
		t.Fatalf("a freshly minted key should have last_seen=null, got %q", *meta.LastSeen)
	}

	// Resolves as an api-kind credential (allowed), reporting kind=api...
	u, kind, err := s.ResolveTokenKinds(ctx, secret, KindAPI)
	if err != nil || u.ID != user.ID || kind != KindAPI {
		t.Fatalf("resolve api key = %v, kind=%q, %v", u, kind, err)
	}
	// ...and via the session+api set the middleware uses, still reporting api
	// (so the transport can bar an api key from credential-minting routes).
	if _, kind, err := s.ResolveTokenKinds(ctx, secret, KindSession, KindAPI); err != nil || kind != KindAPI {
		t.Fatalf("session+api resolver: kind=%q, %v", kind, err)
	}
	// Denied: it is NOT a session token, so a session-only resolve rejects it.
	if _, err := s.ResolveToken(ctx, secret, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("api key resolved as a session token: %v", err)
	}
	// Denied: and it is NOT a pairing token (exchange must never accept it).
	if _, err := s.ResolveToken(ctx, secret, KindPairing); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("api key resolved as a pairing token: %v", err)
	}

	keys, err := s.ListAPITokens(ctx, user.ID)
	if err != nil || len(keys) != 1 || keys[0].ID != meta.ID || keys[0].Label != "cron job" {
		t.Fatalf("list api keys = %+v, %v", keys, err)
	}

	// Revoke by id (owner-scoped) → the secret stops resolving and drops off the list.
	if err := s.RevokeTokenByID(ctx, user.ID, meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveTokenKinds(ctx, secret, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked api key still resolves: %v", err)
	}
	if keys, _ := s.ListAPITokens(ctx, user.ID); len(keys) != 0 {
		t.Fatalf("revoked key still listed: %+v", keys)
	}
}

// TestRevokeTokenByIDScoping pins the owner+kind scoping of RevokeTokenByID: a
// missing id, another user's key, or a session-token id all return ErrNotFound
// (never touching a token they should not).
func TestRevokeTokenByIDScoping(t *testing.T) {
	s, ctx := newTestService(t)
	owner, err := s.CreateUser(ctx, "owner", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateUser(ctx, "other", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	_, meta, err := s.IssueAPIToken(ctx, owner.ID, "key")
	if err != nil {
		t.Fatal(err)
	}

	// Denied: a missing id.
	if err := s.RevokeTokenByID(ctx, owner.ID, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id = %v, want ErrNotFound", err)
	}
	// Denied: another user cannot revoke the owner's key.
	if err := s.RevokeTokenByID(ctx, other.ID, meta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user revoke = %v, want ErrNotFound", err)
	}
	// Denied: a session-token id is not revocable through the api-only path.
	if _, err := s.IssueToken(ctx, owner.ID, KindSession, "device", 0); err != nil {
		t.Fatal(err)
	}
	var sessID int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT id FROM tokens WHERE user_id = ? AND kind = ?`, owner.ID, KindSession).Scan(&sessID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeTokenByID(ctx, owner.ID, sessID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a session id via the api path = %v, want ErrNotFound", err)
	}

	// Allowed: the owner revokes their own key.
	if err := s.RevokeTokenByID(ctx, owner.ID, meta.ID); err != nil {
		t.Fatalf("owner revoke: %v", err)
	}
}
