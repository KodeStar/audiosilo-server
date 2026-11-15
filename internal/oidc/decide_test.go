package oidc

import (
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
