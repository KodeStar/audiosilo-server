package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/store/storetest"
)

// The first-run admin's row: waiting until an admin signs in, the printed
// password until the admin sets their own, and each note written once (a
// write moves the content generation, which empties the caches keyed on it).
// No row (the --setup wizard's admin, an older install): nothing waits,
// nothing was printed, and the notes write nothing.
func TestFirstAdminState(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t)
	s := New(db, time.Now)
	admin, err := s.CreateUser(ctx, "admin", "first-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	gen := db.ContentGeneration()
	if err := s.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() != gen {
		t.Error("a note without the row wrote")
	}
	if err := s.SetPassword(ctx, admin.ID, "second-password", 0); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.FirstAdminWaiting(ctx); u != nil {
		t.Error("waiting without the row")
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); p {
		t.Error("printed password without the row")
	}

	if err := s.MarkFirstAdmin(ctx, admin.ID, ""); err != nil {
		t.Fatal(err)
	}
	if u, err := s.FirstAdminWaiting(ctx); err != nil || u == nil || u.ID != admin.ID {
		t.Fatalf("FirstAdminWaiting = %+v, %v; want the admin", u, err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Error("no printed password after MarkFirstAdmin")
	}
	if err := s.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.FirstAdminWaiting(ctx); u != nil {
		t.Error("still waiting after a sign-in")
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Error("a sign-in replaced the printed password")
	}
	gen = db.ContentGeneration()
	if err := s.NoteAdminSignIn(ctx); err != nil {
		t.Fatal(err)
	}
	if db.ContentGeneration() != gen {
		t.Error("a second sign-in wrote again")
	}
	other, err := s.CreateUser(ctx, "alex", "alex-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, other.ID, "alex-second-password", 0); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Error("another admin's own password replaced the printed one")
	}
	if err := s.SetPassword(ctx, admin.ID, "chosen-password", 0); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); p {
		t.Error("printed password after the admin's was replaced")
	}
	// No longer an admin who can sign in (disabled, demoted): no hint names it.
	if err := s.MarkFirstAdmin(ctx, other.ID, ""); err != nil {
		t.Fatal(err)
	}
	if u, err := s.FirstAdminWaiting(ctx); err != nil || u == nil || u.ID != other.ID {
		t.Fatalf("FirstAdminWaiting = %+v, %v; want the second admin", u, err)
	}
	disabled, user := true, RoleUser
	for name, c := range map[string]UserChanges{"disabled": {Disabled: &disabled}, "demoted": {Role: &user}} {
		if err := s.UpdateUser(ctx, other.ID, c); err != nil {
			t.Fatal(err)
		}
		if u, err := s.FirstAdminWaiting(ctx); err != nil || u != nil {
			t.Errorf("FirstAdminWaiting for a %s account = %+v, %v", name, u, err)
		}
		enabled, admin := false, RoleAdmin
		if err := s.UpdateUser(ctx, other.ID, UserChanges{Disabled: &enabled, Role: &admin}); err != nil {
			t.Fatal(err)
		}
	}
	// The account gone: nothing to wait for.
	if err := s.MarkFirstAdmin(ctx, 9999, ""); err != nil {
		t.Fatal(err)
	}
	if u, err := s.FirstAdminWaiting(ctx); err != nil || u != nil {
		t.Errorf("FirstAdminWaiting for a deleted account = %+v, %v", u, err)
	}
}

// A new password on the first-run admin, whoever sets it (the Your account
// step, another admin, reset-admin: all through UpdateUser), ends the auth
// code printed beside the old one in the same log (D4, server#391), and
// printed_password with it. Denied: the printed code pairs no more. Allowed:
// the admin's other codes, and a code another admin's password change leaves.
func TestNewPasswordEndsThePrintedCode(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin, err := s.CreateUser(ctx, "admin", "first-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	printed, err := s.CreateAuthCode(ctx, admin.ID, InitialAdminCodeLabel, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateAuthCode(ctx, admin.ID, "my phone", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFirstAdmin(ctx, admin.ID, printed); err != nil {
		t.Fatal(err)
	}
	alex, err := s.CreateUser(ctx, "alex", "alex-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, alex.ID, "alex-new-password", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, printed); err != nil {
		t.Fatalf("another admin's password change ended the printed code: %v", err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Fatal("another admin's password change replaced the printed one")
	}
	// Another admin resets the first-run admin's password (server#391: the
	// printed one is gone all the same).
	if err := s.SetPassword(ctx, admin.ID, "set-by-alex", alex.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, printed); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("the printed code after a new password = %v, want ErrInvalidCode", err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); p {
		t.Error("printed_password after another admin set the first-run admin's")
	}
	if _, err := s.ResolveAuthCode(ctx, other); err != nil {
		t.Errorf("the admin's other code went too: %v", err)
	}
}

// Clearing the first-run admin's password (one who signs in with single
// sign-on now) ends the printed password and the code printed beside it as a
// new one does: the log's password no longer works, so its code must not
// either. No one holds a password by it, so the start's hint is not ended as a
// sign-in ends it. Denied: the printed code pairs no more. Allowed: the admin's
// other code, and a clear the server refuses (no other way in) ends nothing.
func TestClearedPasswordEndsThePrintedCode(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin := mustUser(t, s, ctx, "admin", "first-password", RoleAdmin)
	printed, err := s.CreateAuthCode(ctx, admin.ID, InitialAdminCodeLabel, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateAuthCode(ctx, admin.ID, "my phone", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFirstAdmin(ctx, admin.ID, printed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, admin.ID, "", 0); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("clearing an admin's only way in = %v, want ErrAdminNeedsPassword", err)
	}
	if _, err := s.ResolveAuthCode(ctx, printed); err != nil {
		t.Fatalf("a refused clear ended the printed code: %v", err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); !p {
		t.Fatal("a refused clear ended the printed password")
	}
	s.SetSSOIssuer(func() string { return testIssuer })
	if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, claimsFor("sub-admin", "", false)), admin.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, admin.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, printed); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("the printed code after the password was cleared = %v, want ErrInvalidCode", err)
	}
	if p, _ := s.PrintedPassword(ctx, admin.ID); p {
		t.Error("printed_password after the password was cleared")
	}
	if _, err := s.ResolveAuthCode(ctx, other); err != nil {
		t.Errorf("the admin's other code went too: %v", err)
	}
	if fa, ok, err := s.readFirstAdmin(ctx); err != nil || !ok || fa.SignedIn {
		t.Errorf("first-run row after a clear = %+v, %v, %v; want it not signed in", fa, ok, err)
	}
}

// A first-run code the admin rotated was never printed: the row names the
// printed one by id and stamp, which a rotation renews, so a new password
// leaves the rotated code pairing.
func TestNewPasswordKeepsARotatedCode(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	admin, err := s.CreateUser(ctx, "admin", "first-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	printed, err := s.CreateAuthCode(ctx, admin.ID, InitialAdminCodeLabel, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFirstAdmin(ctx, admin.ID, printed); err != nil {
		t.Fatal(err)
	}
	codes, err := s.ListAuthCodes(ctx, admin.ID)
	if err != nil || len(codes) != 1 {
		t.Fatalf("codes = %+v, %v", codes, err)
	}
	*now = now.Add(time.Hour)
	rotated, err := s.RotateAuthCode(ctx, codes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, admin.ID, "chosen-password", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, rotated.Code); err != nil {
		t.Fatalf("a new password ended a rotated code: %v", err)
	}
	if revoked, err := s.RevokeInitialAdminCode(ctx, admin.ID); err != nil || revoked {
		t.Fatalf("RevokeInitialAdminCode = %v, %v; want nothing revoked", revoked, err)
	}
}

// An upgraded install's row names no code (written before it did): a new
// password on the first-run admin ends the printed code all the same, found by
// its label and shape as reset-admin finds it, so the account step's "ends the
// auth code printed beside it" holds there too. Allowed: the admin's other
// codes, and another admin's password change leaves the code.
func TestNewPasswordEndsThePrintedCodeOnAnUpgradedRow(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin, err := s.CreateUser(ctx, "admin", "first-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	printed, err := s.CreateAuthCode(ctx, admin.ID, InitialAdminCodeLabel, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateAuthCode(ctx, admin.ID, "my phone", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFirstAdmin(ctx, admin.ID, ""); err != nil {
		t.Fatal(err)
	}
	alex, err := s.CreateUser(ctx, "alex", "alex-password", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, alex.ID, "alex-new-password", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, printed); err != nil {
		t.Fatalf("another admin's password change ended the printed code: %v", err)
	}
	if err := s.SetPassword(ctx, admin.ID, "chosen-password", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveAuthCode(ctx, printed); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("the printed code after a new password on an upgraded row = %v, want ErrInvalidCode", err)
	}
	if _, err := s.ResolveAuthCode(ctx, other); err != nil {
		t.Errorf("the admin's other code went too: %v", err)
	}
}
