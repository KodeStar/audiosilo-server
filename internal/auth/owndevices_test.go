package auth

import (
	"errors"
	"testing"
	"time"
)

// RevokeOwnDevice signs out only the caller's own live sessions and API keys:
// another user's token (or an unknown, revoked or pairing one) is ErrNotFound and
// left signed in.
func TestRevokeOwnDevice(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam, _ := s.CreateUser(ctx, "sam", "", RoleUser)
	kim, _ := s.CreateUser(ctx, "kim", "", RoleUser)
	issue := func(user int64, kind string, ttl time.Duration) (string, int64) {
		t.Helper()
		secret, err := s.IssueToken(ctx, user, kind, "device", ttl)
		if err != nil {
			t.Fatal(err)
		}
		_, cred, err := s.ResolveRequest(ctx, secret, Presence{}, kind)
		if err != nil {
			t.Fatal(err)
		}
		return secret, cred.ID
	}
	samPhone, samPhoneID := issue(sam.ID, KindSession, 0)
	_, samKeyID := issue(sam.ID, KindAPI, 0)
	kimPhone, kimPhoneID := issue(kim.ID, KindSession, 0)
	_, samPairingID := issue(sam.ID, KindPairing, time.Hour)

	// Denied: someone else's device, a pairing token, an unknown id, no owner.
	for _, tc := range []struct {
		name     string
		user, id int64
	}{
		{"another user's session", sam.ID, kimPhoneID},
		{"a pairing token", sam.ID, samPairingID},
		{"an unknown id", sam.ID, 999999},
		{"no owner", 0, samPhoneID},
	} {
		if err := s.RevokeOwnDevice(ctx, tc.user, tc.id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", tc.name, err)
		}
	}
	if _, _, err := s.ResolveRequest(ctx, kimPhone, Presence{}, KindSession); err != nil {
		t.Fatalf("kim's device after sam's refused revoke: %v", err)
	}
	if _, _, err := s.ResolveRequest(ctx, samPhone, Presence{}, KindSession); err != nil {
		t.Fatalf("sam's phone after the refused revokes: %v", err)
	}

	// Allowed: sam's own API key and session.
	for _, id := range []int64{samKeyID, samPhoneID} {
		if err := s.RevokeOwnDevice(ctx, sam.ID, id); err != nil {
			t.Fatalf("revoke own device %d: %v", id, err)
		}
	}
	if _, _, err := s.ResolveRequest(ctx, samPhone, Presence{}, KindSession); err == nil {
		t.Fatal("a revoked session still authenticates")
	}
	if err := s.RevokeOwnDevice(ctx, sam.ID, samPhoneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: err = %v, want ErrNotFound", err)
	}
	devices, err := s.ListDevices(ctx, sam.ID)
	if err != nil || len(devices) != 0 {
		t.Fatalf("sam's devices after revoking both = %+v %v", devices, err)
	}
}
