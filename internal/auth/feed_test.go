package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Security-critical: a feed token resolves only where a route lists its kind
// (only the calendar feed does), and no other kind resolves as one.
func TestFeedTokenOnlyOnTheFeed(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam := mustUser(t, s, ctx, "sam", "sam-password", RoleUser)
	feed, created, err := s.IssueFeedToken(ctx, sam.ID)
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
	first, created, err := s.IssueFeedToken(ctx, sam.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := s.FeedTokenStatus(ctx, sam.ID); err != nil || !st.Active || st.CreatedAt != created {
		t.Fatalf("status after = %+v %v (created %s)", st, err, created)
	}
	bobFeed, _, err := s.IssueFeedToken(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Resolve once so a cached row would be the thing a reset has to beat.
	if _, _, err := s.ResolveRequest(ctx, first, Presence{}, KindFeed); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.IssueFeedToken(ctx, sam.ID)
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
	feed, _, err := s.IssueFeedToken(ctx, ellis.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, feed, Presence{}, KindFeed); err != nil {
		t.Fatal(err)
	}
	kid := MemberKid
	if _, _, err := s.UpdateMember(ctx, adam.ID, ellis.ID, MemberUpdate{Kind: &kid}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, feed, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a kid's old feed token = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.IssueFeedToken(ctx, ellis.ID); !errors.Is(err, ErrKidAccount) {
		t.Fatalf("a kid's mint = %v, want ErrKidAccount", err)
	}

	// The admin's switch to a kid (UpdateUser) too.
	bea := mustUser(t, s, ctx, "bea", "bea-password", RoleUser)
	beaFeed, _, _ := s.IssueFeedToken(ctx, bea.ID)
	if err := s.UpdateUser(ctx, bea.ID, UserChanges{Password: new(string), Household: &HouseholdChange{Owner: adam.ID, Kind: MemberKid, Access: MemberAll}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, beaFeed, Presence{}, KindFeed); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an assigned kid's feed token = %v, want ErrInvalidToken", err)
	}

	// Disabled: refused while disabled.
	cal := mustUser(t, s, ctx, "cal", "cal-password", RoleUser)
	calFeed, _, _ := s.IssueFeedToken(ctx, cal.ID)
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
