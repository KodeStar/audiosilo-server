package auth

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// everyKind is one person's live credentials of every kind (server#281): two
// sessions (phone, the caller, and laptop), a cast token minted from each, an
// API key, the calendar feed token, an open pairing code, a recovery code and,
// as they are an admin, the Bridge token.
type everyKind struct {
	user                    *User
	phone, laptop           string
	phoneID                 int64
	castPhone, castLaptop   string
	key, feed, bridgeSecret string
	pairing, recovery       string
}

// mustSession signs userID in on device and returns the session token and its
// credential id.
func mustSession(t *testing.T, s *Service, ctx context.Context, userID int64, device string) (string, int64) {
	t.Helper()
	tok, err := s.IssueToken(ctx, userID, KindSession, device, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, cred, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	return tok, cred.ID
}

// plantSessionOn writes a session for member recording on as the device it was
// made on, past MemberSession's checks: the bad data a few tests plant (another
// owner's member on a device, which MemberSession refuses).
func plantSessionOn(t *testing.T, s *Service, ctx context.Context, member, on int64) string {
	t.Helper()
	secret, hash, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.insertTokenOn(ctx, s.db, member, hash, KindSession, "here", nil, nil, "", on, s.ts()); err != nil {
		t.Fatal(err)
	}
	return secret
}

// mustSessionID is mustSession's token id alone.
func mustSessionID(t *testing.T, s *Service, ctx context.Context, userID int64, device string) int64 {
	t.Helper()
	_, id := mustSession(t, s, ctx, userID, device)
	return id
}

// wantLive asserts err is nil when live, ErrInvalidToken when not.
func wantLive(t *testing.T, what string, live bool, err error) {
	t.Helper()
	switch {
	case live && err != nil:
		t.Errorf("%s = %v, want it accepted", what, err)
	case !live && !errors.Is(err, ErrInvalidToken):
		t.Errorf("%s = %v, want ErrInvalidToken", what, err)
	}
}

func mintEveryKind(t *testing.T, s *Service, ctx context.Context, name string) everyKind {
	t.Helper()
	admin, bridgeTok := pairBridge(t, s, ctx, name)
	k := everyKind{user: admin, bridgeSecret: bridgeTok}
	session := func(device string) (string, int64) { return mustSession(t, s, ctx, admin.ID, device) }
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
	if k.key, _, err = s.IssueAPIToken(ctx, admin.ID, k.phoneID, "script"); err != nil {
		t.Fatal(err)
	}
	k.feed = mustFeed(t, s, ctx, admin.ID, k.phoneID)
	// The ways back in a session can make without the password.
	if k.pairing, err = s.IssueOwnPairingToken(ctx, admin.ID, k.phoneID, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if k.recovery, err = s.GenerateRecoveryCode(ctx, admin.ID, k.phoneID); err != nil {
		t.Fatal(err)
	}
	// Every cacheable lookup warm: an end has to beat the cache.
	warm(t, s, k.phone, KindSession)
	warm(t, s, k.laptop, KindSession)
	warm(t, s, k.key, KindSession, KindAPI)
	warm(t, s, k.bridgeSecret, KindBridge)
	return k
}

// check asserts which of k's credentials still resolve: the named sessions
// (and the cast tokens minted from them) as live, the others refused, the open
// pairing code and the recovery code live only while no session has ended, and
// the API key, feed and Bridge tokens always live.
func (k everyKind) check(t *testing.T, s *Service, ctx context.Context, phoneLive, laptopLive bool) {
	t.Helper()
	want := func(what string, live bool, err error) { t.Helper(); wantLive(t, what, live, err) }
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
	// Read without spending them: a lookup, and a redeem (which never consumes).
	ended := !phoneLive || !laptopLive
	_, _, err = s.lookupToken(ctx, hashSecret(k.pairing), KindPairing)
	want("the open pairing code", !ended, err)
	_, err = s.ResolveAuthCode(ctx, k.recovery)
	switch {
	case !ended && err != nil:
		t.Errorf("the recovery code = %v, want it accepted", err)
	case ended && !errors.Is(err, ErrInvalidCode):
		t.Errorf("the recovery code = %v, want ErrInvalidCode", err)
	}
}

// Security-critical (server#281): a password change ends the person's other
// sessions and the cast tokens hanging off them, keeping the caller's, with
// their open pairing codes and recovery code; API keys, the feed and Bridge
// tokens stay; nobody else's sessions move.
func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	k := mintEveryKind(t, s, ctx, "boss")
	bystander := mustUser(t, s, ctx, "bystander", "bystander-password", RoleUser)
	theirs, _ := s.IssueToken(ctx, bystander.ID, KindSession, "phone", 0)
	warm(t, s, theirs, KindSession)

	n, err := s.ChangePassword(ctx, k.user.ID, "a-new-password", k.phoneID)
	if err != nil {
		t.Fatal(err)
	}
	// The count is the laptop alone (server#285): the person's own other session.
	if n != 1 {
		t.Fatalf("ChangePassword ended %d sessions, want 1", n)
	}
	k.check(t, s, ctx, true, false)
	if _, _, err := s.ResolveRequest(ctx, theirs, Presence{}, KindSession); err != nil {
		t.Fatalf("another person's session = %v, want it kept", err)
	}
	// A refused change counts nothing.
	if n, err := s.ChangePassword(ctx, k.user.ID, "short", k.phoneID); err == nil || n != 0 {
		t.Fatalf("a refused change = %d %v, want 0 and an error", n, err)
	}
}

// Security-critical (server#285 Q21): EndKeys ends every API key and the
// calendar feed link of the person, on the next lookup, and nothing else:
// their sessions, pairing and recovery codes, Bridge token and another
// person's key and link stay. An unknown account is ErrNotFound.
func TestEndKeys(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	k := mintEveryKind(t, s, ctx, "boss")
	key2, _, err := s.IssueAPIToken(ctx, k.user.ID, k.phoneID, "cron")
	if err != nil {
		t.Fatal(err)
	}
	other := mustUser(t, s, ctx, "bystander", "bystander-password", RoleUser)
	theirPhone := mustSessionID(t, s, ctx, other.ID, "phone")
	theirKey, _, err := s.IssueAPIToken(ctx, other.ID, theirPhone, "dash")
	if err != nil {
		t.Fatal(err)
	}
	theirFeed := mustFeed(t, s, ctx, other.ID, theirPhone)
	warm(t, s, theirKey, KindSession, KindAPI)

	got, err := s.EndKeys(ctx, k.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != (EndedKeys{APIKeys: 2, CalendarLink: true}) {
		t.Fatalf("EndKeys = %+v, want 2 keys and the link", got)
	}
	for name, secret := range map[string]string{"the API key": k.key, "the second key": key2} {
		_, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession, KindAPI)
		wantLive(t, name, false, err)
	}
	_, _, err = s.ResolveRequest(ctx, k.feed, Presence{}, KindFeed)
	wantLive(t, "the calendar link", false, err)
	for name, c := range map[string]struct {
		secret string
		kind   string
	}{
		"the phone's session":  {k.phone, KindSession},
		"the laptop's session": {k.laptop, KindSession},
		"the Bridge token":     {k.bridgeSecret, KindBridge},
		"another's key":        {theirKey, KindAPI},
		"another's link":       {theirFeed, KindFeed},
	} {
		_, _, err := s.ResolveRequest(ctx, c.secret, Presence{}, c.kind)
		wantLive(t, name, true, err)
	}
	if _, _, err := s.lookupToken(ctx, hashSecret(k.pairing), KindPairing); err != nil {
		t.Errorf("the open pairing code = %v, want it kept", err)
	}
	if _, err := s.ResolveAuthCode(ctx, k.recovery); err != nil {
		t.Errorf("the recovery code = %v, want it kept", err)
	}

	if got, err := s.EndKeys(ctx, k.user.ID); err != nil || got != (EndedKeys{}) {
		t.Fatalf("EndKeys again = %+v %v, want nothing ended", got, err)
	}
	if _, err := s.EndKeys(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("EndKeys(unknown) = %v, want ErrNotFound", err)
	}
}

// Security-critical: a reset with nothing to keep (an admin's for someone
// else) ends every session of theirs and the ways back in a session made, and
// nothing else.
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
// the one kept, with their cast tokens, open pairing codes and recovery code,
// and no API key, feed or Bridge token.
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
	// The browsers they signed in from are forgotten too: a password sign-in
	// from one is a new device again.
	const browser = "8c4a2f1e-0b9d-4c57-9e1a-3f6d2b7c8e90"
	signIn := func() bool {
		t.Helper()
		verified, err := s.Authenticate(ctx, k.user.Username, "admin-password")
		if err != nil {
			t.Fatal(err)
		}
		_, known, err := s.IssueSession(ctx, verified, "admin-web", browser)
		if err != nil {
			t.Fatal(err)
		}
		return known
	}
	signIn()
	if !signIn() {
		t.Fatal("setup: a second sign-in from the browser should know it")
	}
	if n, err = s.SignOutEverywhere(ctx, k.user.ID, 0); err != nil || n != 2 {
		t.Fatalf("sign out everywhere after two browser sign-ins = %d, %v; want 2", n, err)
	}
	if signIn() {
		t.Fatal("a browser is still known after Sign out everywhere")
	}
}

// Security-critical: a password sign-in whose verify ran before a change
// committed gets no session after it (the change ended the ones there were),
// one verified after it does, and a user no Authenticate verified gets none.
func TestSignInRacingAPasswordChangeGetsNoSession(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	mustUser(t, s, ctx, "sam", "old-password", RoleUser)
	early, err := s.Authenticate(ctx, "sam", "old-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, early.ID, "new-password", 0); err != nil {
		t.Fatal(err)
	}
	if secret, _, err := s.IssueSession(ctx, early, "laptop", ""); !errors.Is(err, ErrInvalidCreds) || secret != "" {
		t.Fatalf("a sign-in verified before the change = %q, %v; want ErrInvalidCreds", secret, err)
	}
	if _, _, err := s.IssueSession(ctx, &User{ID: early.ID}, "laptop", ""); !errors.Is(err, ErrInvalidCreds) {
		t.Fatalf("an unverified user = %v, want ErrInvalidCreds", err)
	}
	late, err := s.Authenticate(ctx, "sam", "new-password")
	if err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.IssueSession(ctx, late, "laptop", "")
	if err != nil || secret == "" {
		t.Fatalf("a sign-in with the new password = %q, %v", secret, err)
	}
	if _, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession); err != nil {
		t.Fatalf("its session = %v, want it accepted", err)
	}
}

// householdSignIns is an owner with a phone (the caller's device) and a laptop,
// a member signed in on each, the member on a device of their own (paired, no
// created_on_token_id), a member signed in on a device the owner signed out
// earlier, an open pairing code for the member, and a neighbour's member whose
// session claims the owner's laptop (bad data) and whose pairing code is open.
type householdSignIns struct {
	owner                                   *User
	phoneID                                 int64
	onPhone, onLaptop, ownDevice, onOldTab  string
	pairing, neighbourOnLaptop, neighbourPC string
}

func mintHouseholdSignIns(t *testing.T, s *Service, ctx context.Context) householdSignIns {
	t.Helper()
	owner := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	neighbour := mustUser(t, s, ctx, "nina", "nina-password", RoleUser)
	device := func(name string) int64 { _, id := mustSession(t, s, ctx, owner.ID, name); return id }
	phone, laptop, oldTab := device("phone"), device("laptop"), device("old tablet")
	evie := mustMember(t, s, ctx, owner.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberAll})
	h := householdSignIns{owner: owner, phoneID: phone}
	var err error
	sessionOn := func(owner, member, on int64) string {
		tok, _, err := s.MemberSession(ctx, owner, member, "here", on)
		if err != nil {
			t.Fatal(err)
		}
		warm(t, s, tok, KindSession)
		return tok
	}
	h.onPhone = sessionOn(owner.ID, evie.ID, phone)
	h.onLaptop = sessionOn(owner.ID, evie.ID, laptop)
	h.onOldTab = sessionOn(owner.ID, evie.ID, oldTab)
	if _, err = s.RevokeOwnDeviceAndMembers(ctx, owner.ID, oldTab, false); err != nil {
		t.Fatal(err)
	}
	if h.ownDevice, err = s.IssueToken(ctx, evie.ID, KindSession, "kid's tablet", 0); err != nil {
		t.Fatal(err)
	}
	if _, h.pairing, err = s.MemberPairing(ctx, owner.ID, evie.ID, phone, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	kit := mustMember(t, s, ctx, neighbour.ID, NewMember{Name: "Kit", Kind: MemberKid, Access: MemberAll})
	h.neighbourOnLaptop = plantSessionOn(t, s, ctx, kit.ID, laptop)
	warm(t, s, h.neighbourOnLaptop, KindSession)
	if _, h.neighbourPC, err = s.MemberPairing(ctx, neighbour.ID, kit.ID, mustSessionID(t, s, ctx, neighbour.ID, "nina's phone"), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	return h
}

// check asserts the members' sign-ins after the owner's sessions ended
// (keeping the phone or not): the members' sessions on the owner's ended
// devices and their open pairing code are refused; the one on the kept device,
// the member's own device and everything of the neighbour's stay.
func (h householdSignIns) check(t *testing.T, s *Service, ctx context.Context, phoneKept bool) {
	t.Helper()
	want := func(what string, live bool, err error) { t.Helper(); wantLive(t, what, live, err) }
	session := func(tok string) error {
		_, _, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession)
		return err
	}
	pairing := func(tok string) error {
		_, _, err := s.lookupToken(ctx, hashSecret(tok), KindPairing)
		return err
	}
	want("the member on the kept phone", phoneKept, session(h.onPhone))
	want("the member on the laptop", false, session(h.onLaptop))
	want("the member on a device signed out earlier", false, session(h.onOldTab))
	want("the member's own device", true, session(h.ownDevice))
	want("the member's open pairing code", false, pairing(h.pairing))
	want("the neighbour's member on the laptop (bad data)", true, session(h.neighbourOnLaptop))
	want("the neighbour's member's pairing code", true, pairing(h.neighbourPC))
}

// Security-critical (server#285, owner decision 2026-11-09): the owner's
// password change ends their household members' sessions made on the owner's
// other devices and the members' open pairing codes, keeping the member on the
// device the change was made on, the member's own device and every other
// owner's members.
func TestPasswordChangeEndsMembersOnOwnersDevices(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	h := mintHouseholdSignIns(t, s, ctx)
	if err := s.SetPassword(ctx, h.owner.ID, "a-new-password", h.phoneID); err != nil {
		t.Fatal(err)
	}
	h.check(t, s, ctx, true)
}

// Security-critical (server#285): Sign out everywhere with nothing kept ends
// the members on every one of the owner's devices; kept the caller's, the
// member there stays.
func TestSignOutEverywhereEndsMembersOnOwnersDevices(t *testing.T) {
	t.Parallel()
	for _, keepPhone := range []bool{false, true} {
		s, ctx := newTestService(t)
		h := mintHouseholdSignIns(t, s, ctx)
		keep := int64(0)
		if keepPhone {
			keep = h.phoneID
		}
		if _, err := s.SignOutEverywhere(ctx, h.owner.ID, keep); err != nil {
			t.Fatal(err)
		}
		h.check(t, s, ctx, keepPhone)
	}
}

// Denied: a change that ends nothing of the owner's (an edit with no
// password) leaves the members alone too.
func TestNoPasswordChangeKeepsMembers(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	h := mintHouseholdSignIns(t, s, ctx)
	if err := s.UpdateUser(ctx, h.owner.ID, UserChanges{CanAddMembers: new(true)}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{h.onPhone, h.onLaptop, h.onOldTab, h.ownDevice} {
		if _, _, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession); err != nil {
			t.Errorf("an edit without a password ended a member's session: %v", err)
		}
	}
	if _, _, err := s.lookupToken(ctx, hashSecret(h.pairing), KindPairing); err != nil {
		t.Errorf("an edit without a password ended a member's pairing code: %v", err)
	}
}

// grownUpCredentials is an owner's grown-up member's API keys and calendar
// links: made from a sign-in on the owner's laptop, on the owner's phone (the
// device kept), and on the member's own device, plus another member's link made
// on their own device.
type grownUpCredentials struct {
	owner                             *User
	phoneID                           int64
	keyOnLaptop, keyOnPhone, keyOwn   string
	feedOnLaptop, otherMembersOwnFeed string
}

func mintGrownUpCredentials(t *testing.T, s *Service, ctx context.Context) grownUpCredentials {
	t.Helper()
	owner := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	_, phone := mustSession(t, s, ctx, owner.ID, "phone")
	_, laptop := mustSession(t, s, ctx, owner.ID, "laptop")
	gus := mustMember(t, s, ctx, owner.ID, NewMember{Name: "Gus", Kind: MemberGrownup, Access: MemberAll})
	hal := mustMember(t, s, ctx, owner.ID, NewMember{Name: "Hal", Kind: MemberGrownup, Access: MemberAll})
	// A member's session made on one of the owner's devices, or on the member's
	// own (no created_on_token_id), as the session id a request carries.
	sessionOn := func(member, on int64) int64 {
		t.Helper()
		if on == 0 {
			_, id := mustSession(t, s, ctx, member, "own phone")
			return id
		}
		tok, _, err := s.MemberSession(ctx, owner.ID, member, "here", on)
		if err != nil {
			t.Fatal(err)
		}
		_, cred, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession)
		if err != nil {
			t.Fatal(err)
		}
		return cred.ID
	}
	key := func(member, session int64) string {
		t.Helper()
		k, _, err := s.IssueAPIToken(ctx, member, session, "dash")
		if err != nil {
			t.Fatal(err)
		}
		warm(t, s, k, KindSession, KindAPI)
		return k
	}
	gusLaptop := sessionOn(gus.ID, laptop)
	return grownUpCredentials{
		owner:               owner,
		phoneID:             phone,
		keyOnLaptop:         key(gus.ID, gusLaptop),
		keyOnPhone:          key(gus.ID, sessionOn(gus.ID, phone)),
		keyOwn:              key(gus.ID, sessionOn(gus.ID, 0)),
		feedOnLaptop:        mustFeed(t, s, ctx, gus.ID, gusLaptop),
		otherMembersOwnFeed: mustFeed(t, s, ctx, hal.ID, sessionOn(hal.ID, 0)),
	}
}

func (g grownUpCredentials) check(t *testing.T, s *Service, ctx context.Context, phoneKept bool) {
	t.Helper()
	want := func(what string, live bool, err error) { t.Helper(); wantLive(t, what, live, err) }
	key := func(k string) error {
		_, _, err := s.ResolveRequest(ctx, k, Presence{}, KindSession, KindAPI)
		return err
	}
	feed := func(f string) error { _, _, err := s.ResolveRequest(ctx, f, Presence{}, KindFeed); return err }
	want("the member's key made on the owner's laptop", false, key(g.keyOnLaptop))
	want("the member's calendar link made on the owner's laptop", false, feed(g.feedOnLaptop))
	want("the member's key made on the kept phone", phoneKept, key(g.keyOnPhone))
	want("the member's key made on their own device", true, key(g.keyOwn))
	want("another member's link made on their own device", true, feed(g.otherMembersOwnFeed))
}

// Security-critical (D5, server#361): the owner's password change ends the API
// keys and calendar links their grown-up members made while signed in on the
// owner's other devices, as it ends those sign-ins; what a member made on the
// device kept, or on their own device, stays.
func TestPasswordChangeEndsMembersKeysAndLinks(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	g := mintGrownUpCredentials(t, s, ctx)
	if err := s.SetPassword(ctx, g.owner.ID, "a-new-password", g.phoneID); err != nil {
		t.Fatal(err)
	}
	g.check(t, s, ctx, true)
}

// Security-critical (D5): Sign out everywhere does the same, the device kept
// or none; an edit with no password ends none of them.
func TestSignOutEverywhereEndsMembersKeysAndLinks(t *testing.T) {
	t.Parallel()
	for _, keepPhone := range []bool{false, true} {
		s, ctx := newTestService(t)
		g := mintGrownUpCredentials(t, s, ctx)
		keep := int64(0)
		if keepPhone {
			keep = g.phoneID
		}
		if _, err := s.SignOutEverywhere(ctx, g.owner.ID, keep); err != nil {
			t.Fatal(err)
		}
		g.check(t, s, ctx, keepPhone)
	}
	s, ctx := newTestService(t)
	g := mintGrownUpCredentials(t, s, ctx)
	if err := s.UpdateUser(ctx, g.owner.ID, UserChanges{CanAddMembers: new(true)}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{g.keyOnLaptop, g.keyOnPhone, g.keyOwn} {
		if _, _, err := s.ResolveRequest(ctx, k, Presence{}, KindSession, KindAPI); err != nil {
			t.Errorf("an edit without a password ended a member's key: %v", err)
		}
	}
	if _, _, err := s.ResolveRequest(ctx, g.feedOnLaptop, Presence{}, KindFeed); err != nil {
		t.Errorf("an edit without a password ended a member's link: %v", err)
	}
}

// Security-critical (D5): a member's request that passed the route's check just
// before the owner's Sign out everywhere committed mints nothing from the
// sign-in that ended (madeOn, inside the mint's own write): a key or link made
// then would be made after the pass that ends them, and outlive it. Denied: a
// key and a link from the ended sign-in. Allowed: the link the refused mint
// would have replaced keeps working, and both mint from the member's own
// device, still signed in.
func TestMintFromAnEndedSessionRefused(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	owner := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	_, laptop := mustSession(t, s, ctx, owner.ID, "laptop")
	gus := mustMember(t, s, ctx, owner.ID, NewMember{Name: "Gus", Kind: MemberGrownup, Access: MemberAll})
	tok, _, err := s.MemberSession(ctx, owner.ID, gus.ID, "here", laptop)
	if err != nil {
		t.Fatal(err)
	}
	_, onLaptop, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	_, own := mustSession(t, s, ctx, gus.ID, "own phone")
	link := mustFeed(t, s, ctx, gus.ID, own)
	if _, err := s.SignOutEverywhere(ctx, owner.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssueAPIToken(ctx, gus.ID, onLaptop.ID, "dash"); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a key from the ended sign-in = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.IssueFeedToken(ctx, gus.ID, onLaptop.ID); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a link from the ended sign-in = %v, want ErrInvalidToken", err)
	}
	if keys, err := s.ListAPITokens(ctx, gus.ID); err != nil || len(keys) != 0 {
		t.Errorf("keys after the refused mint = %+v, %v; want none", keys, err)
	}
	if _, _, err := s.ResolveRequest(ctx, link, Presence{}, KindFeed); err != nil {
		t.Errorf("the refused mint ended the link it would have replaced: %v", err)
	}
	if _, _, err := s.IssueAPIToken(ctx, gus.ID, own, "dash"); err != nil {
		t.Errorf("a key from the member's own device: %v", err)
	}
	mustFeed(t, s, ctx, gus.ID, own)
}

// Security-critical (server#444): every credential a session asks for (a
// member's sign-in, a member's pairing code, a member added and signed in on
// the device, and the person's own pairing code, API key, calendar link,
// recovery code, cast token and editor token) reads the device it is asked from
// inside the mint's write: one signed out since the request's check, another
// person's, or an account disabled since, is ErrInvalidToken and nothing is
// made. Allowed: each from the owner's device still signed in, and again once
// the account is enabled.
func TestMintsReadTheAskingDeviceInTheWrite(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	adam := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	_, phone := mustSession(t, s, ctx, adam.ID, "phone")
	_, laptop := mustSession(t, s, ctx, adam.ID, "laptop")
	_, bobs := mustSession(t, s, ctx, bob.ID, "bob's phone")
	gus := mustMember(t, s, ctx, adam.ID, NewMember{Name: "Gus", Kind: MemberGrownup, Access: MemberAll})
	if err := s.RevokeOwnDevice(ctx, adam.ID, laptop); err != nil {
		t.Fatal(err)
	}
	mints := map[string]func(device int64) error{
		"member session": func(d int64) error { _, _, err := s.MemberSession(ctx, adam.ID, gus.ID, "here", d); return err },
		"member pairing": func(d int64) error { _, _, err := s.MemberPairing(ctx, adam.ID, gus.ID, d, time.Minute); return err },
		"member signed in here": func(d int64) error {
			_, _, _, err := s.CreateMember(ctx, adam.ID, NewMember{Name: "Ida", Kind: MemberKid, Access: MemberAll,
				SignInHere: true, DeviceName: "here", OnToken: d}, testPolicy)
			return err
		},
		"own pairing":   func(d int64) error { _, err := s.IssueOwnPairingToken(ctx, adam.ID, d, time.Minute); return err },
		"API key":       func(d int64) error { _, _, err := s.IssueAPIToken(ctx, adam.ID, d, "dash"); return err },
		"calendar link": func(d int64) error { _, _, err := s.IssueFeedToken(ctx, adam.ID, d); return err },
		"recovery code": func(d int64) error { _, err := s.GenerateRecoveryCode(ctx, adam.ID, d); return err },
		"cast token":    func(d int64) error { _, _, err := s.IssueCastToken(ctx, adam.ID, d, 3, "Author/Book"); return err },
		"editor token":  func(d int64) error { _, _, err := s.IssueEditorToken(ctx, adam.ID, d, 3, "Author/Book"); return err },
	}
	// tokens: the members', adam's own but his sessions, and his recovery code.
	counts := func() (tokens, members int) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT
			(SELECT COUNT(*) FROM tokens WHERE user_id IN (SELECT id FROM users WHERE managed_by = ?1) OR (user_id = ?1 AND kind <> ?2))
			+ (SELECT COUNT(*) FROM auth_codes WHERE user_id = ?1),
			(SELECT COUNT(*) FROM users WHERE managed_by = ?1)`, adam.ID, KindSession).Scan(&tokens, &members); err != nil {
			t.Fatal(err)
		}
		return tokens, members
	}
	// Adding a member from a disabled account was ErrNotAllowed already
	// (checkOwner, read in the same write).
	refused := func(why string, device int64) {
		t.Helper()
		tokens, members := counts()
		for name, mint := range mints {
			err := mint(device)
			if !errors.Is(err, ErrInvalidToken) && (name != "member signed in here" || !errors.Is(err, ErrNotAllowed)) {
				t.Errorf("%s %s = %v, want ErrInvalidToken", name, why, err)
			}
		}
		if nt, nm := counts(); nt != tokens || nm != members {
			t.Errorf("mints %s made %d tokens and %d members", why, nt-tokens, nm-members)
		}
	}
	refused("from a device signed out since", laptop)
	refused("from another person's device", bobs)
	if err := s.UpdateUser(ctx, adam.ID, UserChanges{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	refused("from a disabled account", phone)
	if err := s.UpdateUser(ctx, adam.ID, UserChanges{Disabled: new(false)}); err != nil {
		t.Fatal(err)
	}
	tokens, _ := counts()
	for name, mint := range mints {
		if err := mint(phone); err != nil {
			t.Errorf("%s from the signed-in phone = %v", name, err)
		}
	}
	if nt, _ := counts(); nt != tokens+len(mints) {
		t.Errorf("the mints from the signed-in phone made %d credentials, want one each (%d)", nt-tokens, len(mints))
	}
}

// Security-critical (D5): signing a device out with its members ends a
// member's calendar link made there, as it ends their sign-in and API key made
// there (and as the owner's password change does). Denied: a link ended by the
// device's sign-out alone, by another device's with its members, another
// owner's member's link recorded on the device (bad data), and the owner's own.
// A member whose sign-in there already ended loses the link without being
// named: a link is no sign-in.
func TestDeviceRevokeWithMembersEndsALinkMadeThere(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	owner := mustUser(t, s, ctx, "adam", "adam-password", RoleUser)
	bob := mustUser(t, s, ctx, "bob", "bob-password", RoleUser)
	_, laptop := mustSession(t, s, ctx, owner.ID, "laptop")
	_, tablet := mustSession(t, s, ctx, owner.ID, "tablet")
	_, phone := mustSession(t, s, ctx, owner.ID, "phone")
	member := func(of *User, name string) Member {
		t.Helper()
		return mustMember(t, s, ctx, of.ID, NewMember{Name: name, Kind: MemberGrownup, Access: MemberAll})
	}
	// A member's link minted from a sign-in on device on, with that sign-in's id
	// (planted: Kit's on adam's laptop is bad data MemberSession refuses).
	linkOn := func(m Member, on int64) (string, int64) {
		t.Helper()
		tok := plantSessionOn(t, s, ctx, m.ID, on)
		_, cred, err := s.ResolveRequest(ctx, tok, Presence{}, KindSession)
		if err != nil {
			t.Fatal(err)
		}
		return mustFeed(t, s, ctx, m.ID, cred.ID), cred.ID
	}
	gus, hal, ivy := member(owner, "Gus"), member(owner, "Hal"), member(owner, "Ivy")
	gusLink, _ := linkOn(gus, laptop)
	halLink, _ := linkOn(hal, phone)
	ivyLink, ivyThere := linkOn(ivy, laptop)
	if err := s.RevokeOwnDevice(ctx, ivy.ID, ivyThere); err != nil {
		t.Fatal(err)
	}
	kitLink, _ := linkOn(member(bob, "Kit"), laptop)
	ownLink := mustFeed(t, s, ctx, owner.ID, laptop)
	live := func(what, link string, want bool) {
		t.Helper()
		_, _, err := s.ResolveRequest(ctx, link, Presence{}, KindFeed)
		wantLive(t, what, want, err)
	}

	if _, err := s.RevokeOwnDeviceAndMembers(ctx, owner.ID, phone, false); err != nil {
		t.Fatal(err)
	}
	live("a member's link after the device's sign-out without its members", halLink, true)
	if _, err := s.RevokeOwnDeviceAndMembers(ctx, owner.ID, tablet, true); err != nil {
		t.Fatal(err)
	}
	live("a member's link made on another device", gusLink, true)

	gone, err := s.RevokeOwnDeviceAndMembers(ctx, owner.ID, laptop, true)
	if err != nil || !slices.Equal(gone, []string{gus.Username}) {
		t.Fatalf("revoke with members = %v, %v; want only the member signed in there", gone, err)
	}
	live("a member's link made on the device", gusLink, false)
	live("the link of a member whose sign-in there had ended", ivyLink, false)
	live("a member's link made on a device signed out without its members", halLink, true)
	live("another owner's member's link", kitLink, true)
	live("the owner's own link", ownLink, true)
}
