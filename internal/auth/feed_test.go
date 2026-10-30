package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mustFeed mints user's feed token from session (0: none) and checks it
// resolves, which also caches it: what an end has to beat.
func mustFeed(t *testing.T, s *Service, ctx context.Context, user, session int64) string {
	t.Helper()
	feed, _, err := s.IssueFeedToken(ctx, user, session)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, feed, Presence{}, KindFeed); err != nil {
		t.Fatalf("a new feed token = %v", err)
	}
	return feed
}

// Security-critical: a feed token resolves only where a route lists its kind
// (only the calendar feed does), and no other kind resolves as one.
func TestFeedTokenOnlyOnTheFeed(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam := mustUser(t, s, ctx, "sam", "sam-password", RoleUser)
	feed, created, err := s.IssueFeedToken(ctx, sam.ID, 0)
	if err != nil || feed == "" || created == "" {
		t.Fatalf("mint = %q %q %v", feed, created, err)
	}
	u, cred, err := s.ResolveRequest(ctx, feed, Presence{}, KindFeed)
	if err != nil || u.ID != sam.ID || cred.Kind != KindFeed {
		t.Fatalf("allowed: the feed kind = %v, %+v, %v", u, cred, err)
	}
	// Denied: the kinds every other route lists.
	for _, kinds := range [][]string{{KindSession, KindAPI}, {KindSession, KindAPI, KindCast}, {KindBridge}, {KindPairing}} {
		if _, _, err := s.ResolveRequest(ctx, feed, Presence{}, kinds...); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("ResolveRequest(%v) = %v, want ErrInvalidToken", kinds, err)
		}
	}
	if _, _, err := s.ResolveCastRequest(ctx, feed, Presence{}, 1, "Book", KindSession, KindAPI, KindCast); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ResolveCastRequest = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.ConsumePairing(ctx, feed); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("ConsumePairing(feed) = %v, want ErrInvalidToken", err)
	}
	// And no other credential of the person's is a feed token.
	sess, _ := s.IssueToken(ctx, sam.ID, KindSession, "phone", 0)
	key, _, _ := s.IssueAPIToken(ctx, sam.ID, "cron")
	pairing, _ := s.IssueToken(ctx, sam.ID, KindPairing, "", 10*time.Minute)
	for name, secret := range map[string]string{"session": sess, "api key": key, "pairing": pairing} {
		if _, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("a %s as a feed token = %v, want ErrInvalidToken", name, err)
		}
	}
}

// One per user: a new mint ends the old one in the same write, the index keeps
// a second live row out, the secret is stored as its hash only, and the status
// says whether one is live.
func TestFeedTokenLifecycle(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam := mustUser(t, s, ctx, "sam", "sam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)

	if st, err := s.FeedTokenStatus(ctx, sam.ID); err != nil || st.Active || st.CreatedAt != "" {
		t.Fatalf("status before = %+v %v", st, err)
	}
	first, created, err := s.IssueFeedToken(ctx, sam.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := s.FeedTokenStatus(ctx, sam.ID); err != nil || !st.Active || st.CreatedAt != created {
		t.Fatalf("status after = %+v %v (created %s)", st, err, created)
	}
	bobFeed := mustFeed(t, s, ctx, bob.ID, 0)
	// Resolve once so a cached row would be the thing a reset has to beat.
	if _, _, err := s.ResolveRequest(ctx, first, Presence{}, KindFeed); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.IssueFeedToken(ctx, sam.ID, 0)
	if err != nil || second == first {
		t.Fatalf("re-mint = %q %v", second, err)
	}
	if _, _, err := s.ResolveRequest(ctx, first, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the old feed token after a re-mint = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.ResolveRequest(ctx, second, Presence{}, KindFeed); err != nil {
		t.Fatalf("the new feed token = %v", err)
	}
	if _, _, err := s.ResolveRequest(ctx, bobFeed, Presence{}, KindFeed); err != nil {
		t.Fatalf("another person's feed token is untouched by sam's reset: %v", err)
	}
	var rows, plain int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(token_hash = ? OR token_hash = ?) FROM tokens WHERE user_id = ? AND kind = 'feed'`,
		first, second, sam.ID).Scan(&rows, &plain); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || plain != 0 {
		t.Fatalf("sam's feed rows = %d, stored in plain = %d; want 1, 0", rows, plain)
	}
	// The index: a second live row is refused whatever writes it.
	if _, err := s.insertToken(ctx, s.db, sam.ID, hashSecret("another"), KindFeed, FeedTokenName, nil, nil, ""); err == nil ||
		!strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("a second live feed row = %v, want a UNIQUE refusal", err)
	}
	// Never touched: a calendar's polls are no activity of the person's.
	if _, _, err := s.ResolveRequest(ctx, second, Presence{IP: "203.0.113.9", Client: ClientInfo{App: "cal"}}, KindFeed); err != nil {
		t.Fatal(err)
	}
	var seen *string
	if err := s.db.QueryRowContext(ctx, `SELECT last_seen FROM tokens WHERE user_id = ? AND kind = 'feed'`, sam.ID).Scan(&seen); err != nil || seen != nil {
		t.Fatalf("feed last_seen = %v %v, want NULL", seen, err)
	}
	// Never a device.
	devs, err := s.ListDevices(ctx, sam.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devs {
		if d.Kind == KindFeed {
			t.Fatalf("a feed token is listed as a device: %+v", d)
		}
	}

	// Turn off: gone, and again is fine.
	for range 2 {
		if err := s.RevokeFeedToken(ctx, sam.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.ResolveRequest(ctx, second, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the feed token after turning off = %v", err)
	}
	if st, _ := s.FeedTokenStatus(ctx, sam.ID); st.Active {
		t.Fatalf("status after turning off = %+v", st)
	}
}

// Ended by the switch to a kid (and never minted for one), by disabling the
// account and by deleting it.
func TestFeedTokenEndedWithTheAccount(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	ellis := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll})
	feed := mustFeed(t, s, ctx, ellis.ID, 0)
	kid := MemberKid
	if _, _, err := s.UpdateMember(ctx, adam.ID, ellis.ID, MemberUpdate{Kind: &kid}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, feed, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a kid's old feed token = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.IssueFeedToken(ctx, ellis.ID, 0); !errors.Is(err, ErrKidAccount) {
		t.Fatalf("a kid's mint = %v, want ErrKidAccount", err)
	}

	// The admin's switch to a kid (UpdateUser) too.
	bea := mustUser(t, s, ctx, "bea", "bea-password", RoleUser)
	beaFeed := mustFeed(t, s, ctx, bea.ID, 0)
	if err := s.UpdateUser(ctx, bea.ID, UserChanges{Password: new(string), Household: &HouseholdChange{Owner: adam.ID, Kind: MemberKid, Access: MemberAll}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, beaFeed, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an assigned kid's feed token = %v, want ErrInvalidToken", err)
	}

	// Disabled: refused while disabled.
	cal := mustUser(t, s, ctx, "cal", "cal-password", RoleUser)
	calFeed := mustFeed(t, s, ctx, cal.ID, 0)
	yes := true
	if err := s.UpdateUser(ctx, cal.ID, UserChanges{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, calFeed, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a disabled account's feed token = %v, want ErrInvalidToken", err)
	}
	// Deleted: the row goes with the account.
	if _, err := s.DeleteUser(ctx, cal.ID, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ?`, cal.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a deleted account's tokens = %d %v", n, err)
	}
}

// Joining another household ends the calendar link made on the old owner's
// device with the sign-in it was made from (leaveOwners), and only that one: a
// link made from the person's own device survives a move, and made independent
// the person keeps a link made on the old owner's device, as the sign-in made
// there stays.
func TestFeedTokenMadeOnAnotherOwnersDevice(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	session := func(secret string) int64 {
		t.Helper()
		_, cred, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession)
		if err != nil {
			t.Fatal(err)
		}
		return cred.ID
	}
	live := func(feed string) bool {
		_, _, err := s.ResolveRequest(ctx, feed, Presence{}, KindFeed)
		return err == nil
	}
	moveTo := func(user, owner int64) {
		t.Helper()
		change := &HouseholdChange{}
		if owner > 0 {
			change = &HouseholdChange{Owner: owner, Kind: MemberGrownup, Access: MemberAll}
		}
		if err := s.UpdateUser(ctx, user, UserChanges{Household: change}); err != nil {
			t.Fatal(err)
		}
	}
	phone, _ := s.IssueToken(ctx, adam.ID, KindSession, "phone", 0)
	onAdamsPhone := func(name string) (Member, int64) {
		t.Helper()
		m, secret, _, err := s.CreateMember(ctx, adam.ID, NewMember{Name: name, Kind: MemberGrownup, Access: MemberAll,
			SignInHere: true, DeviceName: "phone", OnToken: session(phone)}, testPolicy)
		if err != nil {
			t.Fatal(err)
		}
		return m, session(secret)
	}

	// Denied: made on adam's phone, the link ends with the move to bob's.
	ellis, ellisOnPhone := onAdamsPhone("Ellis")
	feed := mustFeed(t, s, ctx, ellis.ID, ellisOnPhone)
	moveTo(ellis.ID, bob.ID)
	if live(feed) {
		t.Fatal("a link made on the old owner's phone survived the move")
	}
	if st, err := s.FeedTokenStatus(ctx, ellis.ID); err != nil || st.Active {
		t.Fatalf("status after the move = %+v %v", st, err)
	}

	// Allowed: a link made from her own device survives a move back.
	own, _ := s.IssueToken(ctx, ellis.ID, KindSession, "own phone", 0)
	ownFeed := mustFeed(t, s, ctx, ellis.ID, session(own))
	moveTo(ellis.ID, adam.ID)
	if !live(ownFeed) {
		t.Fatal("a link made on her own device ended with the move")
	}

	// Allowed: made independent, a link made on the old owner's phone stays.
	gus, gusOnPhone := onAdamsPhone("Gus")
	gusFeed := mustFeed(t, s, ctx, gus.ID, gusOnPhone)
	moveTo(gus.ID, 0)
	if !live(gusFeed) {
		t.Fatal("made independent, the link made on the old owner's phone ended")
	}
}

// created_at is milliseconds (millisUTC), the status answers the mint's value
// exactly, and two resets within one millisecond (a frozen clock) still differ
// and keep their order: the player compares it to tell a stale link copy.
func TestFeedTokenCreatedAtDistinct(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	*now = time.Date(2026, 10, 30, 12, 0, 0, 123_000_000, time.UTC)
	sam := mustUser(t, s, ctx, "sam", "sam-password", RoleUser)
	_, first, err := s.IssueFeedToken(ctx, sam.ID, 0)
	if err != nil || first != "2026-10-30T12:00:00.123Z" {
		t.Fatalf("first created_at = %q %v", first, err)
	}
	_, second, err := s.IssueFeedToken(ctx, sam.ID, 0)
	if err != nil || second != "2026-10-30T12:00:00.124Z" {
		t.Fatalf("a reset in the same millisecond = %q %v, want the next millisecond", second, err)
	}
	if st, err := s.FeedTokenStatus(ctx, sam.ID); err != nil || st.CreatedAt != second {
		t.Fatalf("status created_at = %q %v, want %q", st.CreatedAt, err, second)
	}
	*now = now.Add(time.Second)
	if _, third, _ := s.IssueFeedToken(ctx, sam.ID, 0); third != "2026-10-30T12:00:01.123Z" {
		t.Fatalf("a later reset = %q", third)
	}
}

// A live link stamped before the milliseconds (whole-second RFC3339) still
// pushes a same-instant reset to the next millisecond.
func TestFeedTokenCreatedAtAfterWholeSecondStamp(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	*now = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	sam := mustUser(t, s, ctx, "sam", "sam-password", RoleUser)
	if _, _, err := s.IssueFeedToken(ctx, sam.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE tokens SET created_at = '2026-10-30T12:00:00Z' WHERE kind = ?`, KindFeed); err != nil {
		t.Fatal(err)
	}
	if _, got, err := s.IssueFeedToken(ctx, sam.ID, 0); err != nil || got != "2026-10-30T12:00:00.001Z" {
		t.Fatalf("a reset after a whole-second stamp = %q %v", got, err)
	}
}
