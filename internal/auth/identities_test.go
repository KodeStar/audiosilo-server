package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/oidc"
)

const testIssuer = "https://idp.example"

func claimsFor(sub, email string, verified bool, groups ...string) oidc.Claims {
	return oidc.Claims{Issuer: testIssuer, Subject: sub, Email: email, EmailVerified: verified,
		Username: "Alice.Smith", Name: "Alice Smith", Groups: groups}
}

func setEmail(t *testing.T, s *Service, ctx context.Context, id int64, email string) {
	t.Helper()
	if err := s.UpdateUser(ctx, id, UserChanges{Email: &email}); err != nil {
		t.Fatal(err)
	}
}

func signIn(t *testing.T, s *Service, ctx context.Context, c oidc.Claims, p oidc.Policy) OIDCResult {
	t.Helper()
	res, err := s.OIDCSignIn(ctx, c, p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestOIDCAutoLink: a verified email matching one ordinary account links it and
// signs in; the next sign-in finds it by (issuer, subject) alone.
func TestOIDCAutoLink(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	alice := mustUser(t, s, ctx, "alice", "", RoleUser)
	setEmail(t, s, ctx, alice.ID, " Alice@Example.org ")
	res := signIn(t, s, ctx, claimsFor("sub-a", "alice@example.org", true), oidc.Policy{})
	if res.Decision.Outcome != oidc.Link || res.User == nil || res.User.ID != alice.ID {
		t.Fatalf("auto-link: %+v", res)
	}
	ids, err := s.ListIdentities(ctx, alice.ID)
	if err != nil || len(ids) != 1 || ids[0].LinkMethod != LinkAutoEmail || ids[0].LinkedBy != nil || ids[0].LastSignInAt == nil {
		t.Fatalf("identities: %+v %v", ids, err)
	}
	// The email changes at the provider: the link holds.
	res = signIn(t, s, ctx, claimsFor("sub-a", "new@example.org", false), oidc.Policy{})
	if res.Decision.Outcome != oidc.SignIn || res.User.ID != alice.ID {
		t.Fatalf("linked sign-in: %+v", res)
	}
	if ids, _ := s.ListIdentities(ctx, alice.ID); ids[0].Email != "new@example.org" {
		t.Fatalf("the identity's email is not refreshed: %+v", ids[0])
	}
}

// TestOIDCTakeoverDenied: an unverified email, a shared email, an admin's email,
// a member's or a demo account's never link; nothing is linked, a pending row is
// written.
func TestOIDCTakeoverDenied(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	setEmail(t, s, ctx, admin.ID, "root@example.org")
	u1 := mustUser(t, s, ctx, "u1", "", RoleUser)
	u2 := mustUser(t, s, ctx, "u2", "", RoleUser)
	setEmail(t, s, ctx, u1.ID, "shared@example.org")
	setEmail(t, s, ctx, u2.ID, "shared@example.org")
	solo := mustUser(t, s, ctx, "solo", "", RoleUser)
	setEmail(t, s, ctx, solo.ID, "solo@example.org")
	m := mustMember(t, s, ctx, solo.ID, NewMember{Name: "Kiddo", Kind: MemberKid, Access: MemberAll})
	setEmail(t, s, ctx, m.ID, "kid@example.org")
	demo, err := s.CreateDemoUser(ctx, "demo_x")
	if err != nil {
		t.Fatal(err)
	}
	setEmail(t, s, ctx, demo.ID, "demo@example.org")

	for _, c := range []struct {
		email    string
		verified bool
		reason   string
	}{
		{"solo@example.org", false, oidc.ReasonUnverifiedEmail},
		{"shared@example.org", true, oidc.ReasonSeveralMatches},
		{"root@example.org", true, oidc.ReasonAdminAccount},
		{"kid@example.org", true, oidc.ReasonNotEligible},
		{"demo@example.org", true, oidc.ReasonNotEligible},
	} {
		res := signIn(t, s, ctx, claimsFor("sub-"+c.email, c.email, c.verified), oidc.Policy{AutoCreate: true})
		if res.Decision.Outcome != oidc.Pending || res.Decision.Reason != c.reason || res.User != nil || pendingID(t, s, ctx, "sub-"+c.email) == 0 {
			t.Errorf("%s: %+v; want pending %s", c.email, res, c.reason)
		}
	}
	for _, id := range []int64{admin.ID, u1.ID, u2.ID, solo.ID, m.ID, demo.ID} {
		if ids, _ := s.ListIdentities(ctx, id); len(ids) != 0 {
			t.Errorf("account %d was linked: %+v", id, ids)
		}
	}
	pending, err := s.ListPending(ctx)
	if err != nil || len(pending) != 5 {
		t.Fatalf("pending: %d %v", len(pending), err)
	}
}

// TestOIDCAlreadyLinkedNotEligible: an account linked to another subject at the
// same issuer is not linked again by email.
func TestOIDCAlreadyLinkedNotEligible(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	a := mustUser(t, s, ctx, "a", "", RoleUser)
	setEmail(t, s, ctx, a.ID, "a@example.org")
	signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true), oidc.Policy{})
	res := signIn(t, s, ctx, claimsFor("sub-2", "a@example.org", true), oidc.Policy{})
	if res.Decision.Reason != oidc.ReasonNotEligible {
		t.Fatalf("a second subject: %+v", res.Decision)
	}
	// Another issuer may link the same account.
	other := claimsFor("sub-2", "a@example.org", true)
	other.Issuer = "https://other.example"
	if res := signIn(t, s, ctx, other, oidc.Policy{}); res.Decision.Outcome != oidc.Link {
		t.Fatalf("another issuer: %+v", res.Decision)
	}
}

// TestOIDCDisabledRefused: a disabled linked account is refused at the sign-in.
func TestOIDCDisabledRefused(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	a := mustUser(t, s, ctx, "a", "", RoleUser)
	setEmail(t, s, ctx, a.ID, "a@example.org")
	signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true), oidc.Policy{})
	yes := true
	if err := s.UpdateUser(ctx, a.ID, UserChanges{Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if res := signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true), oidc.Policy{}); res.Decision.Outcome != oidc.Refuse ||
		res.Decision.Reason != oidc.ReasonDisabled || res.User != nil {
		t.Fatalf("disabled: %+v", res)
	}
}

// TestOIDCCreate: auto_create makes an ordinary account named from the claims
// (numbered on a clash, never demo_), with the verified email only.
func TestOIDCCreate(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	mustUser(t, s, ctx, "alice-smith", "", RoleUser)
	res := signIn(t, s, ctx, claimsFor("sub-1", "new@example.org", true), oidc.Policy{AutoCreate: true})
	if res.Decision.Outcome != oidc.Create || res.User == nil || res.User.Username != "alice-smith-2" ||
		res.User.Role != RoleUser || res.User.Email != "new@example.org" || res.User.HasPassword {
		t.Fatalf("create: %+v %+v", res.Decision, res.User)
	}
	ids, _ := s.ListIdentities(ctx, res.User.ID)
	if len(ids) != 1 || ids[0].LinkMethod != LinkCreated {
		t.Fatalf("identities: %+v", ids)
	}
	c := claimsFor("sub-2", "other@example.org", false)
	c.Username = "demo_mallory"
	res = signIn(t, s, ctx, c, oidc.Policy{AutoCreate: true})
	if res.User == nil || res.User.Username != "demo-mallory" || res.User.Email != "" || res.User.IsDemo {
		t.Fatalf("demo_ name or unverified email: %+v", res.User)
	}
	// allowed_group refuses the others, recording them.
	res = signIn(t, s, ctx, claimsFor("sub-3", "", false, "guests"), oidc.Policy{AutoCreate: true, AllowedGroup: "family"})
	if res.Decision.Outcome != oidc.Pending || res.Decision.Reason != oidc.ReasonNotAllowed {
		t.Fatalf("not allowed: %+v", res.Decision)
	}
	res = signIn(t, s, ctx, claimsFor("sub-4", "", false, "family"), oidc.Policy{AutoCreate: true, AllowedGroup: "family"})
	if res.Decision.Outcome != oidc.Create {
		t.Fatalf("allowed: %+v", res.Decision)
	}
}

func TestIdentityUsernameBase(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		claims oidc.Claims
		want   string
	}{
		{oidc.Claims{Username: "Bob"}, "bob"},
		{oidc.Claims{Email: "carol.d@example.org"}, "carol-d"},
		{oidc.Claims{Name: "Dée Dee"}, "dee-dee"},
		{oidc.Claims{Username: "日本"}, "user"},
		{oidc.Claims{}, "user"},
	} {
		if got := identityUsernameBase(c.claims); got != c.want {
			t.Errorf("%+v: %q, want %q", c.claims, got, c.want)
		}
	}
}

// TestOIDCRoleSync: admin_group promotes and demotes at each sign-in, never the
// last enabled admin. Only a groups list that is there changes the role: an
// absent claim keeps it, whether the identity has carried one before or not
// (the owner's policy, server#405).
func TestOIDCRoleSync(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	root := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	a := mustUser(t, s, ctx, "a", "a-password", RoleAdmin)
	setEmail(t, s, ctx, a.ID, "a@example.org")
	if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, claimsFor("sub-1", "a@example.org", true)), a.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	p := oidc.Policy{AdminGroup: "admins"}
	// Never seen, absent: the role is left as it is (a provider that never
	// sends the claim demotes no one).
	res := signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true), p)
	if res.User.Role != RoleAdmin || res.Role != nil || !res.Decision.NoGroups || groupsSeen(t, s, ctx, "sub-1") {
		t.Fatalf("never-seen, no groups claim: %+v %+v", res.User, res.Role)
	}
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true, "admins"), p)
	if res.User.Role != RoleAdmin || res.Decision.NoGroups || !groupsSeen(t, s, ctx, "sub-1") {
		t.Fatalf("in the group: %+v %+v", res.User, res.Role)
	}
	// Allowed: seen, now absent (a renamed groups_claim, a dropped scope,
	// Entra's overage): the role stays, and the result says it was seen.
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true), p)
	if res.User.Role != RoleAdmin || res.Role != nil || !res.Decision.NoGroups || !res.GroupsSeen {
		t.Fatalf("absent after seen: %+v %+v %+v", res.User, res.Role, res.Decision)
	}
	// Denied: a list that is there without the group demotes, an empty one
	// included.
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true, "listeners"), p)
	if res.User.Role != RoleUser || res.Role == nil || !res.Role.Changed() || res.Decision.NoGroups {
		t.Fatalf("a list without the group: %+v %+v", res.User, res.Role)
	}
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true, "admins"), p)
	if res.User.Role != RoleAdmin || !res.Role.Changed() {
		t.Fatalf("promotion: %+v %+v", res.User, res.Role)
	}
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true, []string{}...), p)
	if res.User.Role != RoleUser || !res.Role.Changed() {
		t.Fatalf("demotion: %+v %+v", res.User, res.Role)
	}
	// Absent again: a demoted person isn't promoted either.
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true), p)
	if res.User.Role != RoleUser || res.Role != nil || !res.Decision.NoGroups {
		t.Fatalf("absent after a demotion: %+v %+v", res.User, res.Role)
	}
	// The last enabled admin, signing in out of the group, stays an admin.
	if err := s.UpdateUser(ctx, root.ID, UserChanges{Email: ptr("root@example.org")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, claimsFor("sub-root", "root@example.org", true)), root.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	res = signIn(t, s, ctx, claimsFor("sub-root", "root@example.org", true, []string{}...), p)
	if res.Decision.Outcome != oidc.SignIn || res.User.Role != RoleAdmin || !res.Role.Refused || res.Role.Changed() {
		t.Fatalf("last admin: %+v %+v", res.User, res.Role)
	}
	// No admin_group: no role change at all.
	res = signIn(t, s, ctx, claimsFor("sub-1", "a@example.org", true, "admins"), oidc.Policy{})
	if res.Role != nil || res.User.Role != RoleUser {
		t.Fatalf("no admin_group: %+v %+v", res.User, res.Role)
	}
}

// TestOIDCGroupsSeenByEmptyList: a sign-in's empty groups list marks the
// identity as one the provider sends groups for, whatever admin_group says,
// and the admin's link from a waiting row with an empty list doesn't (its
// stored list can't tell an absent claim from an empty one), while one from a
// row holding a group does.
func TestOIDCGroupsSeenByEmptyList(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	root := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	b := mustUser(t, s, ctx, "b", "", RoleUser)
	setEmail(t, s, ctx, b.ID, "b@example.org")
	// Linked from a waiting row that held an empty list: not seen yet.
	pend := claimsFor("sub-b", "b@example.org", true, []string{}...)
	if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, pend), b.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	if groupsSeen(t, s, ctx, "sub-b") {
		t.Fatal("an admin's link marked the groups claim seen")
	}
	// Its first sign-in carries an empty list, with no admin_group set.
	signIn(t, s, ctx, claimsFor("sub-b", "b@example.org", true, []string{}...), oidc.Policy{})
	if !groupsSeen(t, s, ctx, "sub-b") {
		t.Fatal("an empty groups list didn't mark the claim seen")
	}
	// An absent claim later never clears it.
	signIn(t, s, ctx, claimsFor("sub-b", "b@example.org", true), oidc.Policy{})
	if !groupsSeen(t, s, ctx, "sub-b") {
		t.Fatal("an absent claim cleared groups_seen")
	}
	// A sign-in that links by email with the claim marks it at once.
	c := mustUser(t, s, ctx, "c", "", RoleUser)
	setEmail(t, s, ctx, c.ID, "c@example.org")
	if res := signIn(t, s, ctx, claimsFor("sub-c", "c@example.org", true, []string{}...), oidc.Policy{}); res.Decision.Outcome != oidc.Link {
		t.Fatalf("link: %+v", res.Decision)
	}
	if !groupsSeen(t, s, ctx, "sub-c") {
		t.Fatal("a linking sign-in's empty list didn't mark the claim seen")
	}
	// A waiting row that holds a group carried the claim: the admin's Create
	// (and Link) from it marks it seen.
	dPend := pendingFor(t, s, ctx, claimsFor("sub-d", "d@example.org", true, "staff"))
	if _, _, err := s.CreateFromPending(ctx, dPend, "", root.ID); err != nil {
		t.Fatal(err)
	}
	if !groupsSeen(t, s, ctx, "sub-d") {
		t.Fatal("a waiting row holding a group didn't mark the claim seen")
	}
}

// groupsSeen reads the test issuer's identity subject's groups_seen.
func groupsSeen(t *testing.T, s *Service, ctx context.Context, subject string) bool {
	t.Helper()
	var seen bool
	if err := s.db.QueryRowContext(ctx, `SELECT groups_seen FROM user_identities WHERE issuer = ? AND subject = ?`,
		testIssuer, subject).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

func ptr[T any](v T) *T { return &v }

// pendingFor records c as pending (auto_create off, no matching account) and
// returns its row's id.
func pendingFor(t *testing.T, s *Service, ctx context.Context, c oidc.Claims) int64 {
	t.Helper()
	c.Email = "" // nothing to match on
	res := signIn(t, s, ctx, c, oidc.Policy{})
	if res.Decision.Outcome != oidc.Pending {
		t.Fatalf("not pending: %+v", res.Decision)
	}
	return pendingID(t, s, ctx, c.Subject)
}

// pendingID is the waiting row of subject's id, 0 for none.
func pendingID(t *testing.T, s *Service, ctx context.Context, subject string) int64 {
	t.Helper()
	rows, err := s.ListPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range rows {
		if p.Subject == subject {
			return p.ID
		}
	}
	return 0
}

// TestPendingLinkCreateDismiss: the console's three actions on a waiting
// sign-in, and their refusals.
func TestPendingLinkCreateDismiss(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	bob := mustUser(t, s, ctx, "bob", "", RoleUser)
	owner := mustUser(t, s, ctx, "owner", "", RoleUser)
	kid := mustMember(t, s, ctx, owner.ID, NewMember{Name: "Kid", Kind: MemberKid, Access: MemberAll})

	id := pendingFor(t, s, ctx, claimsFor("sub-bob", "", false))
	// A second attempt refreshes the row.
	if again := pendingFor(t, s, ctx, claimsFor("sub-bob", "", false)); again != id {
		t.Fatalf("a second attempt made row %d, not %d", again, id)
	}
	rows, _ := s.ListPending(ctx)
	if len(rows) != 1 || rows[0].Attempts != 2 {
		t.Fatalf("pending rows: %+v", rows)
	}
	if _, _, err := s.LinkPending(ctx, id, kid.ID, admin.ID); !errors.Is(err, ErrNotLinkable) {
		t.Fatalf("link to a member: %v", err)
	}
	if _, _, err := s.LinkPending(ctx, id, 9999, admin.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("link to no account: %v", err)
	}
	if _, u, err := s.LinkPending(ctx, id, bob.ID, admin.ID); err != nil || u.ID != bob.ID {
		t.Fatalf("link: %v", err)
	}
	ids, _ := s.ListIdentities(ctx, bob.ID)
	if len(ids) != 1 || ids[0].LinkMethod != LinkAdmin || ids[0].LinkedBy == nil || ids[0].LinkedBy.ID != admin.ID {
		t.Fatalf("bob's identity: %+v", ids)
	}
	if _, _, err := s.LinkPending(ctx, id, bob.ID, admin.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the row is gone: %v", err)
	}
	// Bob's next sign-in is his.
	if res := signIn(t, s, ctx, claimsFor("sub-bob", "", false), oidc.Policy{}); res.Decision.Outcome != oidc.SignIn || res.User.ID != bob.ID {
		t.Fatalf("bob signs in: %+v", res.Decision)
	}
	// Bob has an identity from this issuer: another can't join it.
	id2 := pendingFor(t, s, ctx, claimsFor("sub-other", "", false))
	if _, _, err := s.LinkPending(ctx, id2, bob.ID, admin.ID); !errors.Is(err, ErrUserHasIdentity) {
		t.Fatalf("a second identity: %v", err)
	}
	// Create: the given username, or a taken one refused.
	if _, _, err := s.CreateFromPending(ctx, id2, "bob", admin.ID); !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("create with a taken name: %v", err)
	}
	_, u, err := s.CreateFromPending(ctx, id2, "carol", admin.ID)
	if err != nil || u.Username != "carol" || u.Role != RoleUser {
		t.Fatalf("create: %+v %v", u, err)
	}
	// Dismiss.
	id3 := pendingFor(t, s, ctx, claimsFor("sub-eve", "", false))
	if p, err := s.DismissPending(ctx, id3); err != nil || p.Subject != "sub-eve" {
		t.Fatalf("dismiss: %+v %v", p, err)
	}
	if _, err := s.DismissPending(ctx, id3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dismiss again: %v", err)
	}
	// The next waiting sign-in never takes the dismissed (newest) row's id, so
	// a console list read before the dismissal can't link someone else.
	if id4 := pendingFor(t, s, ctx, claimsFor("sub-frank", "", false)); id4 == id3 {
		t.Fatalf("the dismissed row's id %d was reused", id3)
	}
}

// TestPendingIdentityLinkedMeanwhile: a row whose identity signed in (or was
// linked) since is refused, not linked twice.
func TestPendingIdentityLinkedMeanwhile(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	a := mustUser(t, s, ctx, "a", "", RoleUser)
	b := mustUser(t, s, ctx, "b", "b-password", RoleUser)
	id := pendingFor(t, s, ctx, claimsFor("sub-x", "", false))
	if _, err := s.db.ExecContext(ctx, `INSERT INTO user_identities(user_id, issuer, subject, link_method, linked_at)
		VALUES(?, ?, 'sub-x', 'admin', 't')`, a.ID, testIssuer); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LinkPending(ctx, id, b.ID, admin.ID); !errors.Is(err, ErrIdentityLinked) {
		t.Fatalf("linked meanwhile: %v", err)
	}
}

// TestPendingCapAndPrune: the oldest rows past the cap go, and rows unseen for
// 30 days are neither listed nor kept.
func TestPendingCapAndPrune(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	for i := range maxPendingSignIns + 3 {
		*now = now.Add(time.Second)
		// Past the cap, each new row evicts the oldest (the callback tells
		// the admin); none before it.
		want := int64(0)
		if i >= maxPendingSignIns {
			want = 1
		}
		if res := signIn(t, s, ctx, claimsFor(fmt.Sprintf("sub-%d", i), "", false), oidc.Policy{}); res.PendingEvicted != want {
			t.Fatalf("row %d evicted %d, want %d", i, res.PendingEvicted, want)
		}
	}
	rows, _ := s.ListPending(ctx)
	if len(rows) != maxPendingSignIns || rows[len(rows)-1].Subject != "sub-3" {
		t.Fatalf("cap: %d rows, oldest %q", len(rows), rows[len(rows)-1].Subject)
	}
	// A clock stepped back past every row: the row just written is the
	// oldest, yet the prune keeps it, so the id returned names a row.
	back := *now
	*now = now.Add(-time.Hour)
	res := signIn(t, s, ctx, claimsFor("sub-back", "", false), oidc.Policy{})
	if res.PendingID == 0 || pendingID(t, s, ctx, "sub-back") != res.PendingID {
		t.Fatalf("the row just written was pruned: %+v", res)
	}

	*now = back.AddDate(0, 0, pendingDays+1)
	if rows, _ := s.ListPending(ctx); len(rows) != 0 {
		t.Fatalf("listed after 30 days: %d", len(rows))
	}
	first := pendingFor(t, s, ctx, claimsFor("sub-new", "", false))
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oidc_pending`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("kept after the prune: %d %v", n, err)
	}
	// The same person back once their row is no longer listed: a new row (a
	// new id, so the admins are told again), not the old one brought back.
	*now = now.AddDate(0, 0, pendingDays+1)
	res = signIn(t, s, ctx, claimsFor("sub-new", "", false), oidc.Policy{})
	rows, _ = s.ListPending(ctx)
	if res.PendingID == 0 || res.PendingID == first || len(rows) != 1 || rows[0].ID != res.PendingID || rows[0].Attempts != 1 {
		t.Fatalf("a row unseen for %d days came back: first id %d, now %+v, listed %+v", pendingDays, first, res, rows)
	}
}

// TestPendingGroupsCapped: the groups list kept is bounded.
func TestPendingGroupsCapped(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	groups := make([]string, maxPendingGroups+10)
	for i := range groups {
		groups[i] = fmt.Sprintf("g%d", i)
	}
	pendingFor(t, s, ctx, claimsFor("sub-1", "", false, groups...))
	rows, _ := s.ListPending(ctx)
	if len(rows[0].Groups) != maxPendingGroups {
		t.Fatalf("groups kept: %d", len(rows[0].Groups))
	}
}

// TestUnlinkIdentity: unlinking works, except a password-less admin's last way
// in (decision 5), and ends the account's sign-ins not yet finished.
func TestUnlinkIdentity(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	s.SetSSOIssuer(func() string { return testIssuer })
	root := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	sso := mustUser(t, s, ctx, "sso", "", RoleUser)
	if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, claimsFor("sub-sso", "", false)), sso.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	// Linked: a password-less account may become an admin now.
	if err := s.UpdateUser(ctx, sso.ID, UserChanges{Role: ptr(RoleAdmin)}); err != nil {
		t.Fatalf("promote a password-less account with an identity: %v", err)
	}
	ids, _ := s.ListIdentities(ctx, sso.ID)
	if _, err := s.UnlinkIdentity(ctx, sso.ID, ids[0].ID); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("unlink a password-less admin's last identity: %v", err)
	}
	if _, err := s.UnlinkIdentity(ctx, root.ID, ids[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unlink another account's identity: %v", err)
	}
	if err := s.UpdateUser(ctx, sso.ID, UserChanges{Password: ptr("a-new-password")}); err != nil {
		t.Fatal(err)
	}
	// A sign-in through it finishes while it is linked (allowed); one not yet
	// finished when it is unlinked ends with it (denied).
	done, err := s.IssueToken(ctx, sso.ID, KindOIDC, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeOIDC(ctx, done); err != nil {
		t.Fatal(err)
	}
	open, err := s.IssueToken(ctx, sso.ID, KindOIDC, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.UnlinkIdentity(ctx, sso.ID, ids[0].ID); err != nil || got.Subject != "sub-sso" {
		t.Fatalf("unlink with a password: %+v %v", got, err)
	}
	if _, err := s.ConsumeOIDC(ctx, open); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a sign-in survived its identity's unlink: %v", err)
	}
}

// TestAdminPasswordRule: an admin's password can be cleared only while an
// identity is linked; a password-less account becomes an admin only with one.
func TestAdminPasswordRule(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	s.SetSSOIssuer(func() string { return testIssuer })
	root := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	other := mustUser(t, s, ctx, "other", "admin-password", RoleAdmin)
	plain := mustUser(t, s, ctx, "plain", "", RoleUser)
	if err := s.UpdateUser(ctx, other.ID, UserChanges{Password: ptr("")}); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("clear an admin's password without an identity: %v", err)
	}
	if err := s.UpdateUser(ctx, plain.ID, UserChanges{Role: ptr(RoleAdmin)}); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("promote a password-less account without an identity: %v", err)
	}
	if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, claimsFor("sub-o", "", false)), other.ID, root.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, other.ID, UserChanges{Password: ptr("")}); err != nil {
		t.Fatalf("clear a linked admin's password: %v", err)
	}
	u, _ := s.GetUser(ctx, other.ID)
	if u.HasPassword || u.Role != RoleAdmin {
		t.Fatalf("after clearing: %+v", u)
	}
}

// TestIdentityWayInOnlyForTheRunningSSO: an identity is an admin's way into
// the console only while it is from the single sign-on that can run now: with
// it off (or never set), or configured for another provider, a password-less
// admin's identity neither replaces a password nor blocks its own unlinking.
func TestIdentityWayInOnlyForTheRunningSSO(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	root := mustUser(t, s, ctx, "root", "admin-password", RoleAdmin)
	other := mustUser(t, s, ctx, "other", "admin-password", RoleAdmin)
	plain := mustUser(t, s, ctx, "plain", "", RoleUser)
	for _, u := range []*User{other, plain} {
		if _, _, err := s.LinkPending(ctx, pendingFor(t, s, ctx, claimsFor("sub-"+u.Username, "", false)), u.ID, root.ID); err != nil {
			t.Fatal(err)
		}
	}
	var active string
	s.SetSSOIssuer(func() string { return active })
	for _, iss := range []string{"", "https://another-idp.example"} {
		active = iss
		if err := s.UpdateUser(ctx, other.ID, UserChanges{Password: ptr("")}); !errors.Is(err, ErrAdminNeedsPassword) {
			t.Fatalf("issuer %q: clear a linked admin's password: %v", iss, err)
		}
		if err := s.UpdateUser(ctx, plain.ID, UserChanges{Role: ptr(RoleAdmin)}); !errors.Is(err, ErrAdminNeedsPassword) {
			t.Fatalf("issuer %q: promote a linked, password-less account: %v", iss, err)
		}
	}
	// The running sign-in's: a way in.
	active = testIssuer
	if err := s.UpdateUser(ctx, plain.ID, UserChanges{Role: ptr(RoleAdmin)}); err != nil {
		t.Fatalf("promote with the running sign-in's identity: %v", err)
	}
	ids, _ := s.ListIdentities(ctx, plain.ID)
	if _, err := s.UnlinkIdentity(ctx, plain.ID, ids[0].ID); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("unlink the running sign-in's last way in: %v", err)
	}
	// Single sign-on turned off: the identity is no way in, so unlinking it
	// strands no one it hadn't.
	active = ""
	// Allowed: the console's save re-sending a password-less admin's role
	// changes nothing, so it isn't refused for the way in single sign-on no
	// longer gives (server#405).
	if err := s.UpdateUser(ctx, plain.ID, UserChanges{Role: ptr(RoleAdmin), Email: ptr("plain@example.org")}); err != nil {
		t.Fatalf("re-send an admin's role while single sign-on is off: %v", err)
	}
	if u, _ := s.GetUser(ctx, plain.ID); u.Email != "plain@example.org" || u.Role != RoleAdmin {
		t.Fatalf("the save beside the role: %+v", u)
	}
	// Denied: a promotion is still checked.
	plain2 := mustUser(t, s, ctx, "plain2", "", RoleUser)
	if err := s.UpdateUser(ctx, plain2.ID, UserChanges{Role: ptr(RoleAdmin)}); !errors.Is(err, ErrAdminNeedsPassword) {
		t.Fatalf("promote a password-less account while single sign-on is off: %v", err)
	}
	if _, err := s.UnlinkIdentity(ctx, plain.ID, ids[0].ID); err != nil {
		t.Fatalf("unlink while single sign-on is off: %v", err)
	}
}

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{" a@b.org ": "a@b.org", "": "", "A.B@Example.ORG": "A.B@Example.ORG"} {
		if got, err := NormalizeEmail(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"no-at", "@b.org", "a@", "a b@c.org", "a@b\n.org"} {
		if _, err := NormalizeEmail(bad); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	s, ctx := newTestService(t)
	u := mustUser(t, s, ctx, "u", "", RoleUser)
	if err := s.UpdateUser(ctx, u.ID, UserChanges{Email: ptr("nope")}); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("invalid email saved: %v", err)
	}
	if _, err := s.CreateUserWithEmail(ctx, "v", "", RoleUser, "v@example.org"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetUser(ctx, u.ID+1); got.Email != "v@example.org" {
		t.Fatalf("created email: %q", got.Email)
	}
}

// TestOIDCSignInContextCancel: a failed transaction changes nothing.
func TestOIDCSignInContextCancel(t *testing.T) {
	t.Parallel()
	s, _ := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.OIDCSignIn(ctx, claimsFor("sub", "", false), oidc.Policy{}, time.Minute); err == nil {
		t.Fatal("a cancelled sign-in succeeded")
	}
}

// TestPendingClearedBySignIn: a sign-in that waited and is later placed by a
// sign-in itself (the admin gave the account the email, or turned auto_create
// on) leaves no waiting row behind for the console to offer.
func TestPendingClearedBySignIn(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	alice := mustUser(t, s, ctx, "alice", "", RoleUser)
	pendingFor(t, s, ctx, claimsFor("sub-a", "alice@example.org", true))
	pendingFor(t, s, ctx, claimsFor("sub-b", "", false))
	setEmail(t, s, ctx, alice.ID, "alice@example.org")
	if res := signIn(t, s, ctx, claimsFor("sub-a", "alice@example.org", true), oidc.Policy{}); res.Decision.Outcome != oidc.Link {
		t.Fatalf("link: %+v", res.Decision)
	}
	if res := signIn(t, s, ctx, claimsFor("sub-b", "", false), oidc.Policy{AutoCreate: true}); res.Decision.Outcome != oidc.Create {
		t.Fatalf("create: %+v", res.Decision)
	}
	if rows, err := s.ListPending(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("waiting rows left behind: %+v %v", rows, err)
	}
}

// TestConsumeOIDC: only a token a sign-in minted (KindOIDC) finishes one, once;
// a pairing token or a session presented there is refused and left unspent,
// and a sign-in's token is no pairing token for an exchange. A sign-out
// everywhere ends one not yet finished, and the daily prune drops the spent.
func TestConsumeOIDC(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	alice := mustUser(t, s, ctx, "alice", "", RoleUser)
	sso, err := s.IssueToken(ctx, alice.ID, KindOIDC, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := s.IssueOwnPairingToken(ctx, alice.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.IssueToken(ctx, alice.ID, KindSession, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	// Denied: another kind's token, which stays as it was.
	for name, tok := range map[string]string{"pairing": pair, "session": sess, "unknown": "guess"} {
		if _, err := s.ConsumeOIDC(ctx, tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("%s token finished a sign-in: %v", name, err)
		}
	}
	if u, _, err := s.ConsumePairing(ctx, pair); err != nil || u.ID != alice.ID {
		t.Fatalf("the refused pairing token was spent: %v", err)
	}
	if _, err := s.ResolveToken(ctx, sess, KindSession); err != nil {
		t.Fatalf("the refused session was ended: %v", err)
	}
	// Denied: the sign-in's token at an exchange; it stays unspent too.
	if _, _, err := s.ConsumePairing(ctx, sso); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an exchange took a sign-in's token: %v", err)
	}
	// Allowed: once.
	if u, err := s.ConsumeOIDC(ctx, sso); err != nil || u.ID != alice.ID {
		t.Fatalf("finish: %v %v", u, err)
	}
	if _, err := s.ConsumeOIDC(ctx, sso); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a second finish: %v", err)
	}
	// A sign-out everywhere ends one in flight.
	open, err := s.IssueToken(ctx, alice.ID, KindOIDC, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SignOutEverywhere(ctx, alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeOIDC(ctx, open); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a sign-in survived a sign-out everywhere: %v", err)
	}
	if _, err := s.PruneSingleUseTokens(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE kind = ?`, KindOIDC).Scan(&left); err != nil || left != 0 {
		t.Fatalf("spent sign-in tokens kept: %d %v", left, err)
	}
}

// TestOIDCSignInMintsToken: an outcome that signs in mints the finish token in
// the sign-in's own transaction (server#405), so a sign-out everywhere after
// the decision ends it; one that doesn't sign in mints none.
func TestOIDCSignInMintsToken(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	res, err := s.OIDCSignIn(ctx, claimsFor("sub-a", "", false), oidc.Policy{AutoCreate: true}, time.Minute)
	if err != nil || res.Decision.Outcome != oidc.Create || res.Token == "" {
		t.Fatalf("create: %+v %v", res, err)
	}
	// Allowed: the token finishes the sign-in, once.
	if u, err := s.ConsumeOIDC(ctx, res.Token); err != nil || u.ID != res.User.ID {
		t.Fatalf("finish: %v %v", u, err)
	}
	// Denied: a sign-out everywhere once the sign-in decided ends its token.
	res, err = s.OIDCSignIn(ctx, claimsFor("sub-a", "", false), oidc.Policy{}, time.Minute)
	if err != nil || res.Decision.Outcome != oidc.SignIn || res.Token == "" {
		t.Fatalf("sign in: %+v %v", res, err)
	}
	if _, err := s.SignOutEverywhere(ctx, res.User.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeOIDC(ctx, res.Token); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a sign-in's token survived a sign-out everywhere: %v", err)
	}
	// Never a token that wouldn't expire.
	if _, err := s.OIDCSignIn(ctx, claimsFor("sub-a", "", false), oidc.Policy{}, 0); err == nil {
		t.Fatal("a sign-in with no token life went on")
	}
	// No token for a sign-in that waits.
	if res, err := s.OIDCSignIn(ctx, claimsFor("sub-b", "", false), oidc.Policy{}, time.Minute); err != nil ||
		res.Decision.Outcome != oidc.Pending || res.Token != "" {
		t.Fatalf("pending: %+v %v", res, err)
	}
}

// TestOIDCTokenEndsWhenJoiningAHousehold: a sign-in's token not yet finished
// is ended when its account becomes a member (the callback refuses members),
// a grown-up or a kid, as an open pairing token is. Allowed: an account left
// independent still finishes.
func TestOIDCTokenEndsWhenJoiningAHousehold(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	for _, kind := range []string{MemberGrownup, MemberKid} {
		u := mustUser(t, s, ctx, "joins-"+kind, "", RoleUser)
		tok, err := s.IssueToken(ctx, u.ID, KindOIDC, "", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateUser(ctx, u.ID, UserChanges{Household: &HouseholdChange{Owner: adam.ID, Kind: kind, Access: MemberAll}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ConsumeOIDC(ctx, tok); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("a %s member finished a sign-in started before joining: %v", kind, err)
		}
	}
	stays := mustUser(t, s, ctx, "stays", "", RoleUser)
	tok, err := s.IssueToken(ctx, stays.ID, KindOIDC, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUser(ctx, stays.ID, UserChanges{Household: &HouseholdChange{}}); err != nil {
		t.Fatal(err)
	}
	if u, err := s.ConsumeOIDC(ctx, tok); err != nil || u.ID != stays.ID {
		t.Fatalf("an independent account's sign-in was ended: %v", err)
	}
	// Denied: a token minted after the account joined (the callback decided
	// before the join committed), which leaveOwners never saw.
	late := mustUser(t, s, ctx, "late", "", RoleUser)
	if err := s.UpdateUser(ctx, late.ID, UserChanges{Household: &HouseholdChange{Owner: adam.ID, Kind: MemberGrownup, Access: MemberAll}}); err != nil {
		t.Fatal(err)
	}
	tok, err = s.IssueToken(ctx, late.ID, KindOIDC, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeOIDC(ctx, tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a member finished a sign-in minted after joining: %v", err)
	}
}
