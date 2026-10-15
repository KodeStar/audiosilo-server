package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// pairBridge makes an admin, a pairing code and exchanges it.
func pairBridge(t *testing.T, s *Service, ctx context.Context, name string) (*User, string) {
	t.Helper()
	admin, err := s.CreateUser(ctx, name, "admin-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := s.IssueBridgePairCode(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.ExchangeBridgeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	return admin, token
}

// Security-critical: a bridge token resolves only where a route lists its kind
// (only the Bridge's own routes do), and a session or API key never resolves as
// one.
func TestBridgeTokenOnlyOnBridgeRoutes(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin, token := pairBridge(t, s, ctx, "boss")

	u, cred, err := s.ResolveRequest(ctx, token, Presence{}, KindBridge)
	if err != nil || u.ID != admin.ID || cred.Kind != KindBridge {
		t.Fatalf("allowed: the bridge kind = %v, %+v, %v", u, cred, err)
	}
	// Denied: the kinds every other route lists.
	for _, kinds := range [][]string{{KindSession, KindAPI}, {KindSession, KindAPI, KindCast}, {KindSession}} {
		if _, _, err := s.ResolveRequest(ctx, token, Presence{}, kinds...); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("ResolveRequest(%v) = %v, want ErrInvalidToken", kinds, err)
		}
	}
	if _, _, err := s.ResolveCastRequest(ctx, token, Presence{}, 1, "Book", KindSession, KindAPI, KindCast); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ResolveCastRequest = %v, want ErrInvalidToken", err)
	}
	// And an admin's own session or API key is no bridge token.
	sess, err := s.IssueToken(ctx, admin.ID, KindSession, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.IssueAPIToken(ctx, admin.ID, "cron")
	if err != nil {
		t.Fatal(err)
	}
	for name, secret := range map[string]string{"session": sess, "api key": key} {
		if _, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindBridge); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("a %s as a bridge token = %v, want ErrInvalidToken", name, err)
		}
	}
}

// Security-critical: a bridge token dies with its admin's admin role.
func TestBridgeTokenNeedsAnEnabledAdmin(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	if _, err := s.CreateUser(ctx, "other", "admin-password", RoleAdmin); err != nil {
		t.Fatal(err) // so demoting the pairing admin isn't refused as the last one
	}
	admin, token := pairBridge(t, s, ctx, "boss")
	user := RoleUser
	if err := s.UpdateUser(ctx, admin.ID, UserChanges{Role: &user}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, token, Presence{}, KindBridge); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("demoted admin's bridge token: %v, want ErrInvalidToken", err)
	}
	if p, _ := s.GetBridgePairing(ctx); p.Paired {
		t.Error("a demoted admin's bridge still reads as paired")
	}
	adminRole := RoleAdmin
	if err := s.UpdateUser(ctx, admin.ID, UserChanges{Role: &adminRole}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, token, Presence{}, KindBridge); err != nil {
		t.Fatalf("re-promoted admin's bridge token: %v", err)
	}
	disabled := true
	if err := s.UpdateUser(ctx, admin.ID, UserChanges{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, token, Presence{}, KindBridge); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled admin's bridge token: %v, want ErrInvalidToken", err)
	}
}

// A code works once, only before it expires, and pairing a new Bridge (or
// unpairing) revokes the old token.
func TestBridgePairingCode(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	admin, err := s.CreateUser(ctx, "boss", "admin-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangeBridgeCode(ctx, "0000-0000-0000-0000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("no code made: %v, want ErrInvalidCode", err)
	}
	code, expires, err := s.IssueBridgePairCode(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetBridgePairing(ctx); p.CodeExpiresAt != expires || p.Paired {
		t.Fatalf("pending pairing = %+v", p)
	}
	if _, err := s.ExchangeBridgeCode(ctx, "WRONG-CODE"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v", err)
	}
	first, err := s.ExchangeBridgeCode(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangeBridgeCode(ctx, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("a used code: %v, want ErrInvalidCode", err)
	}
	p, err := s.GetBridgePairing(ctx)
	if err != nil || !p.Paired || p.PairedBy == nil || p.PairedBy.ID != admin.ID || p.CodeExpiresAt != "" {
		t.Fatalf("paired = %+v, %v", p, err)
	}

	// An expired code.
	code2, _, err := s.IssueBridgePairCode(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(bridgePairTTL + time.Second)
	if _, err := s.ExchangeBridgeCode(ctx, code2); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code: %v, want ErrInvalidCode", err)
	}
	if _, _, err := s.ResolveRequest(ctx, first, Presence{}, KindBridge); err != nil {
		t.Fatalf("an expired code must not unpair the Bridge: %v", err)
	}

	// A new pairing revokes the first token.
	code3, _, err := s.IssueBridgePairCode(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ExchangeBridgeCode(ctx, code3)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, first, Presence{}, KindBridge); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the replaced token: %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.ResolveRequest(ctx, second, Presence{}, KindBridge); err != nil {
		t.Fatalf("the new token: %v", err)
	}
	if err := s.UnpairBridge(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, second, Presence{}, KindBridge); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after unpair: %v, want ErrInvalidToken", err)
	}
	if p, _ := s.GetBridgePairing(ctx); p.Paired {
		t.Error("still paired after unpair")
	}
}

// A code made by an admin who has since lost the role pairs nothing.
func TestBridgeCodeOfDemotedAdmin(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	if _, err := s.CreateUser(ctx, "other", "admin-password", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	admin, err := s.CreateUser(ctx, "boss", "admin-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := s.IssueBridgePairCode(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	user := RoleUser
	if err := s.UpdateUser(ctx, admin.ID, UserChanges{Role: &user}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExchangeBridgeCode(ctx, code); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("demoted admin's code: %v, want ErrInvalidCode", err)
	}
}

// A bridge token is never a device.
func TestBridgeTokenIsNotADevice(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin, _ := pairBridge(t, s, ctx, "boss")
	devices, err := s.ListDevices(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 0 {
		t.Fatalf("devices = %+v, want none", devices)
	}
}

// A bridge token's calls (a heartbeat every 30 s) are not its admin's activity:
// the token is never touched, so last_seen_at reads the admin's own devices.
func TestBridgeTokenIsNotActivity(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin, token := pairBridge(t, s, ctx, "boss")
	if _, _, err := s.ResolveRequest(ctx, token, Presence{IP: "192.0.2.7"}, KindBridge); err != nil {
		t.Fatal(err)
	}
	if u, err := s.GetUser(ctx, admin.ID); err != nil || u.LastSeenAt != "" {
		t.Fatalf("denied: the Bridge's call counted as the admin's activity: %q, %v", u.LastSeenAt, err)
	}
	sess, err := s.IssueToken(ctx, admin.ID, KindSession, "laptop", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, sess, Presence{IP: "192.0.2.8"}, KindSession); err != nil {
		t.Fatal(err)
	}
	if u, err := s.GetUser(ctx, admin.ID); err != nil || u.LastSeenAt == "" {
		t.Fatalf("allowed: the admin's own session didn't count: %v", err)
	}
}
