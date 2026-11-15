package oidc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	t.Parallel()
	verified := Claims{Email: "a@example.org", EmailVerified: true, Groups: []string{"listeners"}}
	unverified := Claims{Email: "a@example.org"}
	user := Account{ID: 7}
	cases := []struct {
		name   string
		in     Input
		want   Outcome
		reason string
		acct   int64
	}{
		{"linked signs in", Input{Linked: &Account{ID: 3}}, SignIn, "", 3},
		{"linked disabled refused", Input{Linked: &Account{ID: 3, Disabled: true}}, Refuse, ReasonDisabled, 0},
		{"linked member refused", Input{Linked: &Account{ID: 3, Member: true}}, Refuse, ReasonMember, 0},
		{"linked demo refused", Input{Linked: &Account{ID: 3, Demo: true}}, Refuse, ReasonMember, 0},
		{"linked admin signs in", Input{Linked: &Account{ID: 3, Admin: true}}, SignIn, "", 3},
		{"one eligible verified match links", Input{Claims: verified, Matches: []Account{user}}, Link, "", 7},
		{"unverified match is pending", Input{Claims: unverified, Matches: []Account{user}}, Pending, ReasonUnverifiedEmail, 0},
		{"unverified match never auto-creates", Input{Claims: unverified, Matches: []Account{user}, Policy: Policy{AutoCreate: true}},
			Pending, ReasonUnverifiedEmail, 0},
		{"admin match is pending (decision 4)", Input{Claims: verified, Matches: []Account{{ID: 1, Admin: true}}}, Pending, ReasonAdminAccount, 0},
		{"admin beside an eligible match is pending", Input{Claims: verified, Matches: []Account{user, {ID: 1, Admin: true}}},
			Pending, ReasonAdminAccount, 0},
		{"admin match with auto_create is still pending", Input{Claims: verified, Matches: []Account{{ID: 1, Admin: true}},
			Policy: Policy{AutoCreate: true}}, Pending, ReasonAdminAccount, 0},
		{"several eligible matches", Input{Claims: verified, Matches: []Account{user, {ID: 8}}}, Pending, ReasonSeveralMatches, 0},
		{"one eligible among ineligible links", Input{Claims: verified, Matches: []Account{user, {ID: 9, Member: true}}}, Link, "", 7},
		{"member match not eligible", Input{Claims: verified, Matches: []Account{{ID: 9, Member: true}}}, Pending, ReasonNotEligible, 0},
		{"demo match not eligible", Input{Claims: verified, Matches: []Account{{ID: 9, Demo: true}}}, Pending, ReasonNotEligible, 0},
		{"disabled match not eligible", Input{Claims: verified, Matches: []Account{{ID: 9, Disabled: true}}}, Pending, ReasonNotEligible, 0},
		{"already linked match not eligible", Input{Claims: verified, Matches: []Account{{ID: 9, Linked: true}}}, Pending, ReasonNotEligible, 0},
		{"no match, auto_create off, verified", Input{Claims: verified}, Pending, ReasonAutoCreateOff, 0},
		{"no match, auto_create off, no email", Input{}, Pending, ReasonNoMatch, 0},
		{"no match, auto_create off, unverified", Input{Claims: unverified}, Pending, ReasonNoMatch, 0},
		{"no match, auto_create on", Input{Claims: verified, Policy: Policy{AutoCreate: true}}, Create, "", 0},
		{"auto_create in allowed group", Input{Claims: verified, Policy: Policy{AutoCreate: true, AllowedGroup: "listeners"}}, Create, "", 0},
		{"auto_create outside allowed group", Input{Claims: verified, Policy: Policy{AutoCreate: true, AllowedGroup: "staff"}},
			Pending, ReasonNotAllowed, 0},
		{"allowed group is exact", Input{Claims: verified, Policy: Policy{AutoCreate: true, AllowedGroup: "Listeners"}},
			Pending, ReasonNotAllowed, 0},
	}
	for _, c := range cases {
		d := Decide(c.in)
		if d.Outcome != c.want || d.Reason != c.reason || d.Account != c.acct {
			t.Errorf("%s: got %+v; want %s %q account %d", c.name, d, c.want, c.reason, c.acct)
		}
		if d.Role != "" {
			t.Errorf("%s: role %q without admin_group", c.name, d.Role)
		}
	}
}

func TestDecideRoleSync(t *testing.T) {
	t.Parallel()
	in := Input{Claims: Claims{Groups: []string{"audiosilo-admins"}}, Linked: &Account{ID: 3}, Policy: Policy{AdminGroup: "audiosilo-admins"}}
	if d := Decide(in); d.Role != RoleAdmin {
		t.Fatalf("in the admin group: role %q, want admin", d.Role)
	}
	in.Claims.Groups = []string{"other"}
	in.Linked.Admin = true
	if d := Decide(in); d.Role != RoleUser {
		t.Fatalf("out of the admin group: role %q, want user", d.Role)
	}
	in.Policy.AutoCreate, in.Linked = true, nil
	in.Claims.Groups = []string{"audiosilo-admins"}
	if d := Decide(in); d.Outcome != Create || d.Role != RoleAdmin {
		t.Fatalf("create in the admin group: %+v", d)
	}
	// No role for a sign-in that doesn't happen.
	in.Policy.AutoCreate = false
	if d := Decide(in); d.Outcome != Pending || d.Role != "" {
		t.Fatalf("pending: %+v", d)
	}
	in.Linked = &Account{ID: 3, Disabled: true}
	if d := Decide(in); d.Outcome != Refuse || d.Role != "" {
		t.Fatalf("refused: %+v", d)
	}
}

// TestDecideNoGroupsClaim: with admin_group set, a sign-in that carried no
// groups claim at all leaves the role alone (NoGroups) while its identity has
// never carried one, while an empty list is a claim and demotes.
func TestDecideNoGroupsClaim(t *testing.T) {
	t.Parallel()
	in := Input{Claims: Claims{}, Linked: &Account{ID: 3, Admin: true}, Policy: Policy{AdminGroup: "audiosilo-admins"}}
	if d := Decide(in); d.Outcome != SignIn || d.Role != "" || !d.NoGroups {
		t.Fatalf("no groups claim: %+v", d)
	}
	in.Claims.Groups = []string{}
	if d := Decide(in); d.Role != RoleUser || d.NoGroups {
		t.Fatalf("an empty groups claim: %+v", d)
	}
	// Without admin_group nothing is said of groups.
	in.Claims.Groups, in.Policy.AdminGroup = nil, ""
	if d := Decide(in); d.Role != "" || d.NoGroups {
		t.Fatalf("no admin_group: %+v", d)
	}
}

// TestDecideGroupsSeen: once a linked identity has carried a groups claim, a
// sign-in without one is out of every group (Keycloak leaves the claim out for
// a person in no group): an admin is demoted, nothing is logged as missing.
// A sign-in in the group still promotes.
func TestDecideGroupsSeen(t *testing.T) {
	t.Parallel()
	in := Input{Linked: &Account{ID: 3, Admin: true}, GroupsSeen: true, Policy: Policy{AdminGroup: "audiosilo-admins"}}
	if d := Decide(in); d.Outcome != SignIn || d.Role != RoleUser || d.NoGroups {
		t.Fatalf("absent after seen: %+v", d)
	}
	in.Claims.Groups = []string{"audiosilo-admins"}
	if d := Decide(in); d.Role != RoleAdmin || d.NoGroups {
		t.Fatalf("in the group: %+v", d)
	}
	// The flag is the linked identity's: a first sign-in (no identity yet)
	// without the claim leaves the role alone.
	in = Input{Claims: Claims{Email: "a@example.org", EmailVerified: true}, Matches: []Account{{ID: 4}},
		Policy: Policy{AdminGroup: "audiosilo-admins"}}
	if d := Decide(in); d.Outcome != Link || d.Role != "" || !d.NoGroups {
		t.Fatalf("a first sign-in: %+v", d)
	}
}

// TestFillEmailAsSent: an email is vouched for only as the provider sent it;
// one clean had to change (cut, trimmed, a control character dropped) is kept
// for the console but counts as unverified, so it never links by itself.
func TestFillEmailAsSent(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", MaxEmail) + "@example.org"
	for _, tc := range []struct {
		raw      string
		verified bool
	}{
		{"alice@example.org", true},
		{" alice@example.org", false},
		{"alice@example.org\n", false},
		{"ali\u0000ce@example.org", false},
		{long, false},
	} {
		raw, _ := json.Marshal(tc.raw)
		var c Claims
		c.fill(claimSet{"email": raw, "email_verified": json.RawMessage("true")}, "u", "g")
		if c.Email == "" || c.EmailVerified != tc.verified {
			t.Errorf("%q: email %q verified %v, want verified %v", tc.raw, c.Email, c.EmailVerified, tc.verified)
		}
	}
}

// TestGroupsMatchedWhole: every group a large directory sends is there to
// match (the admin's group may come after the hundredth), and a name that had
// to be mended (cut, a control character dropped) is no group at all, so it
// can't turn into admin_group.
func TestGroupsMatchedWhole(t *testing.T) {
	t.Parallel()
	list := []string{" listeners ", "listeners", "", "audiosilo-\u0000admins", strings.Repeat("g", maxGroupClaim) + "-admins"}
	for i := range 300 {
		list = append(list, fmt.Sprintf("team-%d", i))
	}
	list = append(list, "audiosilo-admins")
	raw, _ := json.Marshal(list)
	var c Claims
	c.fill(claimSet{"g": raw}, "u", "g")
	if len(c.Groups) != 302 || c.Groups[0] != "listeners" || !c.InGroup("audiosilo-admins") {
		t.Fatalf("%d groups, first %q, admin %v", len(c.Groups), c.Groups[0], c.InGroup("audiosilo-admins"))
	}
	if c.InGroup(strings.Repeat("g", maxGroupClaim)) {
		t.Fatal("a cut name matched")
	}
	// The mended name alone: not in the group.
	raw, _ = json.Marshal([]string{"audiosilo-\u0000admins"})
	c = Claims{}
	c.fill(claimSet{"g": raw}, "u", "g")
	if c.Groups == nil || len(c.Groups) != 0 || c.InGroup("audiosilo-admins") {
		t.Fatalf("a name with a control character matched: %q", c.Groups)
	}
	// A list with no string to read (a provider sending objects) is no claim,
	// not an empty one: it leaves the role alone rather than demoting.
	c = Claims{}
	c.fill(claimSet{"g": json.RawMessage(`[{"name":"audiosilo-admins"},7]`)}, "u", "g")
	if c.Groups != nil {
		t.Fatalf("an unreadable list read as a claim: %q", c.Groups)
	}
}

// TestWithoutBody: a failed request's error keeps its status, never the
// provider's response body.
func TestWithoutBody(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"401 Unauthorized: {\"error\":\"invalid_token\",\"echo\":\"secret\"}": "401 Unauthorized",
		"404 Not Found: <html>long page</html>":                               "404 Not Found",
		`Get "https://idp.example/.well-known": dial tcp: refused`:            `Get "https://idp.example/.well-known": dial tcp: refused`,
		"oidc: failed to decode provider discovery object: EOF":               "oidc: failed to decode provider discovery object: EOF",
		// A signature that can't be checked: the keys endpoint's answer follows.
		"failed to verify signature: fetching keys oidc: get keys failed: 502 Bad Gateway <html>long page</html>": "failed to verify signature: fetching keys oidc: get keys failed: 502",
		"failed to verify signature: fetching keys oidc: failed to decode keys: EOF <html>long page</html>":       "failed to verify signature: fetching keys oidc: failed to decode keys",
	} {
		if got := withoutBody(in); got != want {
			t.Errorf("withoutBody(%q) = %q, want %q", in, got, want)
		}
	}
}
