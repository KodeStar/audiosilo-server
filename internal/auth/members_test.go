package auth

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

var testPolicy = MemberPolicy{Enabled: true, Max: 6}

func mustUser(t *testing.T, s *Service, ctx context.Context, name, password, role string) *User {
	t.Helper()
	u, err := s.CreateUser(ctx, name, password, role)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustMember(t *testing.T, s *Service, ctx context.Context, owner int64, in NewMember) Member {
	t.Helper()
	m, _, err := s.CreateMember(ctx, owner, in, testPolicy)
	if err != nil {
		t.Fatalf("create %q: %v", in.Name, err)
	}
	return m
}

func TestUsernameSlug(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"Evie", "evie"},
		{"  Évie  Marie ", "evie-marie"},
		{"Zoë-Ann!!", "zoe-ann"},
		{"小明", "member"},
		{"---", "member"},
		{"Ångström Ölander Øystein Bartholomew", "angstrom-olander-ystein"},
		{"ＥＶＩＥ２", "evie2"}, // NFKD folds full-width forms
	} {
		if got := usernameBase(c.in); got != c.want {
			t.Errorf("usernameBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if len(usernameBase(strings.Repeat("a", 60))) != maxUsernameBase {
		t.Error("base not cut to 24 bytes")
	}
}

// Create: an ordinary user adds a kid signed in on their device and a grown-up
// with no session; the account is a password-less user with all three member
// columns; usernames are derived, then made unique.
func TestMembers_Create(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "Adam", "adam-password", RoleUser)
	phone, err := s.IssueToken(ctx, adam.ID, KindSession, "Adam's phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, cred, _ := s.ResolveRequest(ctx, phone, Presence{}, KindSession)

	evie, secret, err := s.CreateMember(ctx, adam.ID, NewMember{Name: " Evie ", Kind: MemberKid, Access: MemberCollections,
		SignInHere: true, DeviceName: cred.DeviceName, OnToken: cred.ID}, testPolicy)
	if err != nil || secret == "" {
		t.Fatalf("create kid = %v %q", err, secret)
	}
	if evie.Name != "Evie" || evie.Username != "evie" || evie.Kind != MemberKid || evie.Access != MemberCollections || evie.Devices != 1 ||
		len(evie.CreatedAt) != len("2006-01-02T15:04:05.000Z") {
		t.Fatalf("kid = %+v", evie)
	}
	u, kcred, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession)
	if err != nil || u.ID != evie.ID || kcred.DeviceName != "Adam's phone" {
		t.Fatalf("kid session = %+v %+v %v", u, kcred, err)
	}
	var on sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT created_on_token_id FROM tokens WHERE id = ?`, kcred.ID).Scan(&on); err != nil || on.Int64 != cred.ID {
		t.Fatalf("created_on_token_id = %v %v", on, err)
	}
	full, err := s.GetUser(ctx, evie.ID)
	if err != nil || full.Role != RoleUser || full.HasPassword || full.ManagedBy == nil || full.ManagedBy.ID != adam.ID ||
		full.ManagedBy.Username != "Adam" || !full.IsKid() || *full.MemberAccess != MemberCollections || full.DisplayName != "Evie" {
		t.Fatalf("kid account = %+v %v", full, err)
	}

	ellis, secret, err := s.CreateMember(ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll}, testPolicy)
	if err != nil || secret != "" || ellis.Devices != 0 {
		t.Fatalf("create grown-up = %+v %q %v", ellis, secret, err)
	}

	// Usernames: the base, then with the owner's, then numbered.
	second := mustMember(t, s, ctx, adam.ID, NewMember{Name: "EVIE", Kind: MemberKid, Access: MemberAll})
	third := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Évie", Kind: MemberKid, Access: MemberAll})
	if second.Username != "evie-adam" || third.Username != "evie-2" {
		t.Fatalf("usernames = %q %q", second.Username, third.Username)
	}
	list, err := s.ListMembers(ctx, adam.ID)
	if err != nil || len(list) != 4 || list[0].ID != evie.ID || list[3].ID != third.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	if others, _ := s.ListMembers(ctx, ellis.ID); len(others) != 0 {
		t.Fatal("a member lists members")
	}
	adamNow, _ := s.GetUser(ctx, adam.ID)
	if adamNow.MembersCount != 4 || adamNow.IsMember() || !adamNow.CanAddMembers {
		t.Fatalf("owner = %+v", adamNow)
	}
}

// Create refusals, each in the transport's order; nothing is written.
func TestMembers_CreateRefused(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	demo, err := s.CreateDemoUser(ctx, "demo_1")
	if err != nil {
		t.Fatal(err)
	}
	ellis := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll})
	kid := NewMember{Name: "Evie", Kind: MemberKid, Access: MemberCollections}
	for _, c := range []struct {
		name   string
		owner  int64
		in     NewMember
		policy MemberPolicy
		want   error
	}{
		{"demo", demo.ID, kid, MemberPolicy{Enabled: false, Max: 6}, ErrDemoAccount},
		{"member", ellis.ID, kid, MemberPolicy{Enabled: false, Max: 6}, ErrMemberCannotManage},
		{"feature off", adam.ID, kid, MemberPolicy{Enabled: false, Max: 6}, ErrFeatureOff},
		{"at the cap", adam.ID, kid, MemberPolicy{Enabled: true, Max: 1}, ErrMemberLimit},
		{"no name", adam.ID, NewMember{Name: "  ", Kind: MemberKid, Access: MemberAll}, testPolicy, ErrInvalidName},
		{"long name", adam.ID, NewMember{Name: strings.Repeat("é", 41), Kind: MemberKid, Access: MemberAll}, testPolicy, ErrInvalidName},
		{"control", adam.ID, NewMember{Name: "Ev\x07ie", Kind: MemberKid, Access: MemberAll}, testPolicy, ErrInvalidName},
		{"kind", adam.ID, NewMember{Name: "Evie", Kind: "teen", Access: MemberAll}, testPolicy, ErrInvalidKind},
		{"access", adam.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: "some"}, testPolicy, ErrInvalidAccess},
		{"share fails", adam.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberAll,
			Share: func(*sql.Tx, int64) error { return ErrNotFound }}, testPolicy, ErrNotFound},
	} {
		if _, _, err := s.CreateMember(ctx, c.owner, c.in, c.policy); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := s.SetCanAddMembers(ctx, adam.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateMember(ctx, adam.ID, kid, testPolicy); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("switched off: %v", err)
	}
	if list, _ := s.ListMembers(ctx, adam.ID); len(list) != 1 {
		t.Fatalf("a refused create wrote: %+v", list)
	}
	var users int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil || users != 3 {
		t.Fatalf("users = %d %v", users, err)
	}
}

// A member never logs in with a password: the account has none, so the
// username/password login refuses it (an empty password included).
func TestMembers_KidPasswordLoginRefused(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	mustMember(t, s, ctx, adam.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberAll})
	for _, pw := range []string{"", "anything-at-all"} {
		if _, err := s.Authenticate(ctx, "evie", pw); !errors.Is(err, ErrInvalidCreds) {
			t.Errorf("login %q = %v", pw, err)
		}
	}
	if _, err := s.Authenticate(ctx, "adam", "adam-password"); err != nil {
		t.Fatalf("the owner's own login: %v", err)
	}
}

// Kind changes: a grown-up switched to a kid loses their password, API keys,
// pairing tokens and recovery code; their sessions stay. Access is untouched.
func TestMembers_KindChangeClearsCredentials(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	ellis := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll})
	if err := s.SetPassword(ctx, ellis.ID, "ellis-password"); err != nil {
		t.Fatal(err)
	}
	session, _ := s.IssueToken(ctx, ellis.ID, KindSession, "Ellis's phone", 0)
	key, _, _ := s.IssueAPIToken(ctx, ellis.ID, "dash")
	pairing, _ := s.IssueToken(ctx, ellis.ID, KindPairing, "", memberPairingTTL)
	if _, err := s.GenerateRecoveryCode(ctx, ellis.ID); err != nil {
		t.Fatal(err)
	}

	kid := MemberKid
	m, changes, err := s.UpdateMember(ctx, adam.ID, ellis.ID, MemberUpdate{Kind: &kid})
	if err != nil || m.Kind != MemberKid || m.Access != MemberAll || changes["kind"] != MemberKid || len(changes) != 1 {
		t.Fatalf("switch = %+v %v %v", m, changes, err)
	}
	u, _ := s.GetUser(ctx, ellis.ID)
	if u.HasPassword || u.HasRecovery {
		t.Fatalf("kid keeps a password or recovery code: %+v", u)
	}
	if _, err := s.ResolveToken(ctx, session, KindSession); err != nil {
		t.Fatalf("session signed out: %v", err)
	}
	if _, err := s.ResolveToken(ctx, key, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("API key still works: %v", err)
	}
	if _, _, err := s.ConsumePairing(ctx, pairing); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("pairing token still works: %v", err)
	}

	// Back to a grown-up and the access both ways: nothing else changes.
	gr, coll, all := MemberGrownup, MemberCollections, MemberAll
	if m, _, err = s.UpdateMember(ctx, adam.ID, ellis.ID, MemberUpdate{Kind: &gr, Access: &coll}); err != nil || m.Access != MemberCollections {
		t.Fatalf("to collections = %+v %v", m, err)
	}
	if m, changes, err = s.UpdateMember(ctx, adam.ID, ellis.ID, MemberUpdate{Access: &all}); err != nil || m.Access != MemberAll || m.Kind != MemberGrownup ||
		len(changes) != 1 {
		t.Fatalf("to all = %+v %v %v", m, changes, err)
	}
	// Denied: another person can't change them (ErrNotMember), nor bad input.
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	if _, _, err := s.UpdateMember(ctx, bob.ID, ellis.ID, MemberUpdate{Kind: &kid}); !errors.Is(err, ErrNotMember) {
		t.Fatalf("bob's change = %v", err)
	}
	if _, _, err := s.UpdateMember(ctx, ellis.ID, ellis.ID, MemberUpdate{Kind: &gr}); !errors.Is(err, ErrNotMember) {
		t.Fatalf("own change = %v", err)
	}
	bad := "teen"
	if _, _, err := s.UpdateMember(ctx, adam.ID, ellis.ID, MemberUpdate{Kind: &bad}); !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("bad kind = %v", err)
	}
}

// Ownership: every per-member operation answers ErrNotMember for anyone else's
// member, the member themselves and an unknown id.
func TestMembers_Ownership(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	evie := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberAll})
	tok, _ := s.IssueToken(ctx, evie.ID, KindSession, "tablet", 0)
	_, cred, _ := s.ResolveRequest(ctx, tok, Presence{}, KindSession)

	// Allowed for adam.
	if _, err := s.Member(ctx, adam.ID, evie.ID); err != nil {
		t.Fatal(err)
	}
	if d, err := s.MemberDevices(ctx, adam.ID, evie.ID); err != nil || len(d) != 1 {
		t.Fatalf("devices = %v %v", d, err)
	}
	if p, err := s.MemberPairing(ctx, adam.ID, evie.ID); err != nil || p == "" {
		t.Fatalf("pairing = %v", err)
	}
	for _, who := range []int64{bob.ID, evie.ID, 0} {
		for name, err := range map[string]error{
			"member":  func() error { _, err := s.Member(ctx, who, evie.ID); return err }(),
			"session": func() error { _, _, err := s.MemberSession(ctx, who, evie.ID, "x", 0); return err }(),
			"pair":    func() error { _, err := s.MemberPairing(ctx, who, evie.ID); return err }(),
			"devices": func() error { _, err := s.MemberDevices(ctx, who, evie.ID); return err }(),
			"revoke":  s.RevokeMemberDevice(ctx, who, evie.ID, cred.ID),
			"delete":  func() error { _, err := s.DeleteMember(ctx, who, evie.ID); return err }(),
		} {
			if !errors.Is(err, ErrNotMember) {
				t.Errorf("%d %s = %v", who, name, err)
			}
		}
	}
	if _, err := s.Member(ctx, adam.ID, 99999); !errors.Is(err, ErrNotMember) {
		t.Error("unknown id")
	}
	if err := s.RevokeMemberDevice(ctx, adam.ID, evie.ID, 99999); !errors.Is(err, ErrNotFound) {
		t.Error("a device not the member's")
	}
	if err := s.RevokeMemberDevice(ctx, adam.ID, evie.ID, cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveToken(ctx, tok, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("revoked member device still works")
	}
	if _, err := s.DeleteMember(ctx, adam.ID, evie.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUser(ctx, evie.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted member still there")
	}
}

// Sign-ins: signing the owner's phone out keeps the members' sessions made on
// it, unless alsoMembers; their tokens elsewhere, and another owner's member,
// stay.
func TestMembers_SignIns(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	phone, _ := s.IssueToken(ctx, adam.ID, KindSession, "phone", 0)
	_, pc, _ := s.ResolveRequest(ctx, phone, Presence{}, KindSession)
	evie, evieHere, _ := s.CreateMember(ctx, adam.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberAll,
		SignInHere: true, DeviceName: "phone", OnToken: pc.ID}, testPolicy)
	ellis := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll})
	ellisHere, _, err := s.MemberSession(ctx, adam.ID, ellis.ID, "phone", pc.ID)
	if err != nil {
		t.Fatal(err)
	}
	evieElsewhere, _ := s.IssueToken(ctx, evie.ID, KindSession, "tablet", 0)
	// Bob's member signed in on a token id of adam's (bad data): never adam's.
	bobKid := mustMember(t, s, ctx, bob.ID, NewMember{Name: "Kit", Kind: MemberKid, Access: MemberAll})
	bobKidTok, _, _ := s.MemberSession(ctx, bob.ID, bobKid.ID, "x", pc.ID)

	ins, err := s.MemberSignIns(ctx, adam.ID)
	if err != nil || len(ins[pc.ID]) != 2 || ins[pc.ID][0] != (MemberSignIn{evie.ID, "Evie"}) || ins[pc.ID][1].ID != ellis.ID {
		t.Fatalf("sign-ins = %+v %v", ins, err)
	}

	// Without alsoMembers: only the phone.
	n, err := s.RevokeOwnDeviceAndMembers(ctx, adam.ID, pc.ID, false)
	if err != nil || n != 0 {
		t.Fatalf("revoke = %d %v", n, err)
	}
	if _, err := s.ResolveToken(ctx, evieHere, KindSession); err != nil {
		t.Fatal("member signed out without alsoMembers")
	}

	// A second phone with both members on it, signed out with alsoMembers.
	phone2, _ := s.IssueToken(ctx, adam.ID, KindSession, "phone 2", 0)
	_, pc2, _ := s.ResolveRequest(ctx, phone2, Presence{}, KindSession)
	e2, _, _ := s.MemberSession(ctx, adam.ID, evie.ID, "phone 2", pc2.ID)
	l2, _, _ := s.MemberSession(ctx, adam.ID, ellis.ID, "phone 2", pc2.ID)
	n, err = s.RevokeOwnDeviceAndMembers(ctx, adam.ID, pc2.ID, true)
	if err != nil || n != 2 {
		t.Fatalf("revoke with members = %d %v", n, err)
	}
	for _, tok := range []string{e2, l2} {
		if _, err := s.ResolveToken(ctx, tok, KindSession); !errors.Is(err, ErrInvalidToken) {
			t.Error("member token made on the phone survived")
		}
	}
	for _, tok := range []string{evieElsewhere, evieHere, ellisHere, bobKidTok} {
		if _, err := s.ResolveToken(ctx, tok, KindSession); err != nil {
			t.Error("a token made elsewhere (or another owner's member) was signed out")
		}
	}
	// Denied: bob can't use his own call on adam's device.
	if _, err := s.RevokeOwnDeviceAndMembers(ctx, bob.ID, pc.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob revoked adam's device: %v", err)
	}
}

// Deletion: deleting an owner deletes their members, except those kept, which
// become ordinary accounts; a keep id that isn't theirs refuses everything.
func TestMembers_DeleteOwner(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	mustUser(t, s, ctx, "root", "root-password", RoleAdmin)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	evie := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberCollections})
	ellis := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll})
	kit := mustMember(t, s, ctx, bob.ID, NewMember{Name: "Kit", Kind: MemberKid, Access: MemberAll})

	if _, err := s.DeleteUser(ctx, adam.ID, []int64{kit.ID}); !errors.Is(err, ErrInvalidKeepMembers) {
		t.Fatalf("keep another's member = %v", err)
	}
	if _, err := s.GetUser(ctx, evie.ID); err != nil {
		t.Fatal("a refused delete deleted a member")
	}
	out, err := s.DeleteUser(ctx, adam.ID, []int64{ellis.ID})
	if err != nil || out.MembersDeleted != 1 || !slices.Equal(out.MembersKept, []string{"ellis"}) {
		t.Fatalf("delete = %+v %v", out, err)
	}
	if _, err := s.GetUser(ctx, evie.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("unkept member survived")
	}
	kept, err := s.GetUser(ctx, ellis.ID)
	if err != nil || kept.IsMember() || kept.MemberKind != nil || kept.MemberAccess != nil {
		t.Fatalf("kept = %+v %v", kept, err)
	}
	if out, err := s.DeleteUser(ctx, bob.ID, nil); err != nil || out.MembersDeleted != 1 {
		t.Fatalf("delete bob = %+v %v", out, err)
	}
}

// The admin's member fields: assign (each refusal), make independent, the role
// rule both ways.
func TestMembers_AdminAssign(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	root := mustUser(t, s, ctx, "root", "root-password", RoleAdmin)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	demo, _ := s.CreateDemoUser(ctx, "demo_1")
	ellis := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Ellis", Kind: MemberGrownup, Access: MemberAll})
	key, _, _ := s.IssueAPIToken(ctx, bob.ID, "dash")

	for name, c := range map[string]struct {
		id, owner int64
		kind      string
		want      error
	}{
		"self":            {bob.ID, bob.ID, MemberKid, ErrSelfManaged},
		"owner is member": {bob.ID, ellis.ID, MemberKid, ErrOwnerIsMember},
		"owner is demo":   {bob.ID, demo.ID, MemberKid, ErrOwnerIsDemo},
		"member is demo":  {demo.ID, adam.ID, MemberGrownup, ErrMemberIsDemo},
		"admin member":    {root.ID, adam.ID, MemberKid, ErrMemberNotAdmin},
		"has members":     {adam.ID, bob.ID, MemberKid, ErrHasMembers},
		"unknown owner":   {bob.ID, 99999, MemberKid, ErrNotFound},
		"unknown person":  {99999, adam.ID, MemberKid, ErrNotFound},
		"bad kind":        {bob.ID, adam.ID, "teen", ErrInvalidKind},
	} {
		if err := s.AssignMember(ctx, c.id, c.owner, c.kind, MemberAll); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if err := s.AssignMember(ctx, bob.ID, adam.ID, MemberKid, MemberCollections); err != nil {
		t.Fatal(err)
	}
	u, _ := s.GetUser(ctx, bob.ID)
	if !u.IsKid() || u.ManagedBy.ID != adam.ID || u.HasPassword {
		t.Fatalf("assigned = %+v", u)
	}
	if _, err := s.ResolveToken(ctx, key, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("assigned kid keeps an API key")
	}
	if err := s.SetRole(ctx, ellis.ID, RoleAdmin); !errors.Is(err, ErrMemberNotAdmin) {
		t.Fatalf("member promoted: %v", err)
	}
	if err := s.MakeIndependent(ctx, ellis.ID); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.GetUser(ctx, ellis.ID); u.IsMember() || u.MemberKind != nil {
		t.Fatalf("independent = %+v", u)
	}
	if err := s.MakeIndependent(ctx, root.ID); err != nil {
		t.Fatalf("no-op on a non-member: %v", err)
	}
	if err := s.MakeIndependent(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown id")
	}
}
