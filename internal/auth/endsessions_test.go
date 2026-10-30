package auth

import (
	"context"
	"errors"
	"testing"
)

// everyKind is one person's live credentials of every kind (server#281): two
// sessions (phone, the caller, and laptop), a cast token minted from each, an
// API key, the calendar feed token and, as they are an admin, the Bridge token.
type everyKind struct {
	user                    *User
	phone, laptop           string
	phoneID                 int64
	castPhone, castLaptop   string
	key, feed, bridgeSecret string
}

func mintEveryKind(t *testing.T, s *Service, ctx context.Context, name string) everyKind {
	t.Helper()
	admin, bridgeTok := pairBridge(t, s, ctx, name)
	k := everyKind{user: admin, bridgeSecret: bridgeTok}
	session := func(device string) (string, int64) {
		tok, err := s.IssueToken(ctx, admin.ID, KindSession, device, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, cred, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession)
		if err != nil {
			t.Fatal(err)
		}
		return tok, cred.ID
	}
	var laptopID int64
	k.phone, k.phoneID = session("phone")
	k.laptop, laptopID = session("laptop")
	var err error
	if k.castPhone, _, err = s.IssueCastToken(ctx, admin.ID, k.phoneID, 3, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	if k.castLaptop, _, err = s.IssueCastToken(ctx, admin.ID, laptopID, 3, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	if k.key, _, err = s.IssueAPIToken(ctx, admin.ID, "script"); err != nil {
		t.Fatal(err)
	}
	k.feed = mustFeed(t, s, ctx, admin.ID, k.phoneID)
	// Every cacheable lookup warm: an end has to beat the cache.
	warm(t, s, k.phone, KindSession)
	warm(t, s, k.laptop, KindSession)
	warm(t, s, k.key, KindSession, KindAPI)
	warm(t, s, k.bridgeSecret, KindBridge)
	return k
}

// check asserts which of k's credentials still resolve: the named sessions
// (and the cast tokens minted from them) as live, the others refused, and the
// API key, feed and Bridge tokens always live.
func (k everyKind) check(t *testing.T, s *Service, ctx context.Context, phoneLive, laptopLive bool) {
	t.Helper()
	want := func(what string, live bool, err error) {
		t.Helper()
		switch {
		case live && err != nil:
			t.Errorf("%s = %v, want it accepted", what, err)
		case !live && !errors.Is(err, ErrInvalidToken):
			t.Errorf("%s = %v, want ErrInvalidToken", what, err)
		}
	}
	_, _, err := s.ResolveRequest(ctx, k.phone, Presence{}, KindSession)
	want("the phone's session", phoneLive, err)
	_, _, err = resolveCast(s, ctx, k.castPhone, "Author/Book")
	want("the phone's cast token", phoneLive, err)
	_, _, err = s.ResolveRequest(ctx, k.laptop, Presence{}, KindSession)
	want("the laptop's session", laptopLive, err)
	_, _, err = resolveCast(s, ctx, k.castLaptop, "Author/Book")
	want("the laptop's cast token", laptopLive, err)
	_, _, err = s.ResolveRequest(ctx, k.key, Presence{}, KindSession, KindAPI)
	want("the API key", true, err)
	_, _, err = s.ResolveRequest(ctx, k.feed, Presence{}, KindFeed)
	want("the feed token", true, err)
	_, _, err = s.ResolveRequest(ctx, k.bridgeSecret, Presence{}, KindBridge)
	want("the Bridge token", true, err)
}

// Security-critical (server#281): a password change ends the person's other
// sessions and the cast tokens hanging off them, keeping the caller's; API
// keys, the feed and Bridge tokens stay; nobody else's sessions move.
func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	k := mintEveryKind(t, s, ctx, "boss")
	bystander := mustUser(t, s, ctx, "bystander", "bystander-password", RoleUser)
	theirs, _ := s.IssueToken(ctx, bystander.ID, KindSession, "phone", 0)
	warm(t, s, theirs, KindSession)

	if err := s.SetPassword(ctx, k.user.ID, "a-new-password", k.phoneID); err != nil {
		t.Fatal(err)
	}
	k.check(t, s, ctx, true, false)
	if _, _, err := s.ResolveRequest(ctx, theirs, Presence{}, KindSession); err != nil {
		t.Fatalf("another person's session = %v, want it kept", err)
	}
}

// Security-critical: a reset with nothing to keep (an admin's for someone
// else) ends every session of theirs, and only sessions.
func TestPasswordResetEndsEverySession(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	k := mintEveryKind(t, s, ctx, "boss")
	if err := s.UpdateUser(ctx, k.user.ID, UserChanges{Password: new("reset-by-admin")}); err != nil {
		t.Fatal(err)
	}
	k.check(t, s, ctx, false, false)
}

// A first password replaces none, so it ends nothing; clearing one the person
// had ends their sessions like a reset; a change with no password field (a role,
// the disabled switch) ends nothing either.
func TestPasswordEndsSessionsOnlyWhenOneIsReplaced(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam := mustUser(t, s, ctx, "sam", "", RoleUser)
	phone, _ := s.IssueToken(ctx, sam.ID, KindSession, "phone", 0)
	if err := s.SetPassword(ctx, sam.ID, "first-password", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, phone, Presence{}, KindSession); err != nil {
		t.Fatalf("a session after a first password = %v, want it kept", err)
	}
	if err := s.UpdateUser(ctx, sam.ID, UserChanges{CanAddMembers: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, phone, Presence{}, KindSession); err != nil {
		t.Fatalf("a session after a change without a password = %v, want it kept", err)
	}
	if err := s.SetPassword(ctx, sam.ID, "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, phone, Presence{}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a session after the password was cleared = %v, want ErrInvalidToken", err)
	}
}

// A refused password change ends nothing (the write is all or nothing).
func TestRefusedPasswordChangeEndsNothing(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	k := mintEveryKind(t, s, ctx, "boss") // the only admin: can't clear theirs
	if err := s.SetPassword(ctx, k.user.ID, "", 0); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("clearing the last admin's password = %v, want ErrAdminNeedsPassword", err)
	}
	k.check(t, s, ctx, true, true)
}

// Security-critical: Sign out everywhere ends every session of the person but
// the one kept, with their cast tokens, and nothing of another kind.
func TestSignOutEverywhere(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	k := mintEveryKind(t, s, ctx, "boss")
	bystander := mustUser(t, s, ctx, "bystander", "bystander-password", RoleUser)
	theirs, _ := s.IssueToken(ctx, bystander.ID, KindSession, "phone", 0)

	// Denied: an unknown account.
	if _, err := s.SignOutEverywhere(ctx, 9999, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account = %v, want ErrNotFound", err)
	}
	// Kept: the caller's own (an admin signing themselves out everywhere else).
	n, err := s.SignOutEverywhere(ctx, k.user.ID, k.phoneID)
	if err != nil || n != 1 {
		t.Fatalf("sign out everywhere but the phone = %d, %v; want 1", n, err)
	}
	k.check(t, s, ctx, true, false)
	// Nothing kept: every session ends.
	if n, err = s.SignOutEverywhere(ctx, k.user.ID, 0); err != nil || n != 1 {
		t.Fatalf("sign out everywhere = %d, %v; want 1", n, err)
	}
	k.check(t, s, ctx, false, false)
	if n, err = s.SignOutEverywhere(ctx, k.user.ID, 0); err != nil || n != 0 {
		t.Fatalf("again = %d, %v; want 0", n, err)
	}
	if _, _, err := s.ResolveRequest(ctx, theirs, Presence{}, KindSession); err != nil {
		t.Fatalf("another person's session = %v, want it kept", err)
	}
	// The browsers they signed in from are forgotten too.
	var known int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tokens WHERE user_id = ? AND sign_in_key <> ''`, k.user.ID).Scan(&known); err != nil || known != 0 {
		t.Fatalf("browsers still known = %d, %v", known, err)
	}
}
