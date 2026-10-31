package auth

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// cached reports whether the token cache holds secret's lookup for the current
// generation (the next request is answered from it).
func cached(s *Service, secret string) bool {
	_, ok := s.tokens.Get(s.db.ContentGeneration(), hashSecret(secret))
	return ok
}

// warm resolves secret until its lookup is answered from the cache (the first
// resolve may touch the token, which drops its lookup).
func warm(t *testing.T, s *Service, secret string, kinds ...string) {
	t.Helper()
	for range 3 {
		if _, _, err := s.ResolveRequest(t.Context(), secret, Presence{}, kinds...); err != nil {
			t.Fatal(err)
		}
		if cached(s, secret) {
			return
		}
	}
	t.Fatal("a valid token's lookup was never cached")
}

// Allowed: a session and an API key are answered from the cache while no content
// is written, with the same user and credential the database gives.
func TestTokenCacheAnswersValidTokens(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	sess, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	key, _ := s.IssueToken(ctx, u.ID, KindAPI, "script", 0)
	for _, secret := range []string{sess, key} {
		warm(t, s, secret, KindSession, KindAPI)
		got, cred, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession, KindAPI)
		if err != nil || got.ID != u.ID || cred.ID == 0 {
			t.Fatalf("cached resolve = %+v %+v %v", got, cred, err)
		}
	}
	_, cred, _ := s.ResolveRequest(ctx, key, Presence{}, KindSession, KindAPI)
	if cred.Kind != KindAPI {
		t.Fatalf("cached API key's kind = %q", cred.Kind)
	}
	// A kind the route doesn't take is refused from the cache as from the table.
	if _, _, err := s.ResolveRequest(ctx, key, Presence{}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an API key where only a session is taken = %v", err)
	}
	// A caller changing the user it was handed doesn't change the next one's.
	got, _, _ := s.ResolveRequest(ctx, sess, Presence{}, KindSession)
	got.Role = RoleAdmin
	if again, _, _ := s.ResolveRequest(ctx, sess, Presence{}, KindSession); again.Role != RoleUser {
		t.Fatalf("role after a caller changed its copy = %q", again.Role)
	}
}

// A caller changing what its user's pointers point at (a member's owner, kind,
// access and rule) changes nothing kept either: the resolve that fills the cache
// and each one answered from it hand out their own copies.
func TestTokenCacheKeepsAMembersOwnFields(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	owner := mustUser(t, s, ctx, "adam", "pw-pw-pw-pw", RoleUser)
	kid := mustMember(t, s, ctx, owner.ID, NewMember{Name: "Evie", Kind: MemberKid, Access: MemberCollections})
	secret, err := s.IssueToken(ctx, kid.ID, KindSession, "tablet", 0)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func() *User {
		t.Helper()
		u, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession)
		if err != nil {
			t.Fatal(err)
		}
		if u.ManagedBy == nil || u.ManagedBy.ID != owner.ID || u.MemberKind == nil || *u.MemberKind != MemberKid ||
			u.MemberAccess == nil || *u.MemberAccess != MemberCollections ||
			u.MemberRule == nil || u.MemberRule.Owner != owner.ID || u.MemberRule.Access != MemberCollections {
			t.Fatalf("member resolved as %+v", u)
		}
		// The caller's changes, which the next resolve must not see.
		u.ManagedBy.ID, *u.MemberKind, *u.MemberAccess = 0, MemberGrownup, MemberAll
		u.MemberRule.Owner, u.MemberRule.Access = 0, MemberAll
		return u
	}
	for i := 0; !cached(s, secret); i++ {
		if i == 3 {
			t.Fatal("a valid token's lookup was never cached")
		}
		resolve() // the last of these filled the cache
	}
	resolve() // answered from the cache
	resolve()
}

// Denied: a sign-out, a revoked device and a disabled account refuse the token
// on the very next request, cached or not.
func TestTokenCacheRefusesAtOnce(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)

	sess, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	warm(t, s, sess, KindSession)
	if err := s.RevokeToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, sess, Presence{}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a signed-out session = %v, want ErrInvalidToken", err)
	}

	tablet, _ := s.IssueToken(ctx, u.ID, KindSession, "tablet", 0)
	warm(t, s, tablet, KindSession)
	_, cred, _ := s.ResolveRequest(ctx, tablet, Presence{}, KindSession)
	if err := s.RevokeDevice(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, tablet, Presence{}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a revoked device = %v, want ErrInvalidToken", err)
	}

	key, _ := s.IssueToken(ctx, u.ID, KindAPI, "script", 0)
	warm(t, s, key, KindAPI)
	disabled := true
	if err := s.UpdateUser(ctx, u.ID, UserChanges{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, key, Presence{}, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a disabled account's API key = %v, want ErrInvalidToken", err)
	}
}

// Denied: a token past its expiry is refused though its lookup is cached and
// nothing was written.
func TestTokenCacheChecksExpiry(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	sess, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", time.Hour)
	warm(t, s, sess, KindSession)
	*now = now.Add(2 * time.Hour)
	if !cached(s, sess) {
		t.Fatal("the lookup should still be cached: nothing was written")
	}
	if _, _, err := s.ResolveRequest(ctx, sess, Presence{}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an expired session = %v, want ErrInvalidToken", err)
	}
}

// Cast and pairing tokens are never cached: a cast token's grant and parent are
// read on every request, so its book and its sender's sign-out apply as before.
func TestTokenCacheSkipsCastAndPairing(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, sess, sessID := castSender(t, s, ctx, "sam")
	cast, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, err := resolveCast(s, ctx, cast, "Author/Book"); err != nil {
			t.Fatal(err)
		}
	}
	if cached(s, cast) {
		t.Fatal("a cast token's lookup was cached")
	}
	if _, _, err := resolveCast(s, ctx, cast, "Author/Other"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a cast token on another book = %v", err)
	}
	if err := s.RevokeToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, cast, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a cast token of a signed-out sender = %v", err)
	}

	pairing, _ := s.IssueToken(ctx, u.ID, KindPairing, "", time.Minute)
	if _, err := s.ResolveToken(ctx, pairing, KindPairing); err != nil {
		t.Fatal(err)
	}
	if cached(s, pairing) {
		t.Fatal("a pairing token's lookup was cached")
	}
}

// The cache is bounded (store.GenMap's generations are its own test).
func TestTokenCacheBounded(t *testing.T) {
	t.Parallel()
	s := New(nil, nil)
	for i := range maxCachedTokens + 1 {
		s.tokens.Put(3, string(rune(i+1000)), cachedToken{})
	}
	if n := s.tokens.Len(); n > maxCachedTokens {
		t.Fatalf("cache holds %d lookups, bound %d", n, maxCachedTokens)
	}
}

// Allowed (server#258): a token's last-seen touch is user state. It leaves the
// content generation, so every other token's lookup stays cached, and drops
// only the touched token's, whose next lookup reads the new address and is
// cached again without another touch.
func TestTokenCacheSurvivesTouches(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	phone, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	tablet, _ := s.IssueToken(ctx, u.ID, KindSession, "tablet", 0)
	warm(t, s, phone, KindSession)
	warm(t, s, tablet, KindSession)
	gen := s.db.ContentGeneration()

	away := Presence{IP: "203.0.113.9"}
	if _, _, err := s.ResolveRequest(ctx, phone, away, KindSession); err != nil {
		t.Fatal(err)
	}
	if s.db.ContentGeneration() != gen {
		t.Fatalf("a touch moved the content generation %d -> %d", gen, s.db.ContentGeneration())
	}
	if !cached(s, tablet) || cached(s, phone) {
		t.Fatalf("after the phone's touch: tablet cached %v (want true), phone cached %v (want false)",
			cached(s, tablet), cached(s, phone))
	}
	var ip string
	if err := s.db.QueryRowContext(ctx, `SELECT last_ip FROM tokens WHERE token_hash = ?`, hashSecret(phone)).Scan(&ip); err != nil || ip != away.IP {
		t.Fatalf("stored address = %q, %v; want %q", ip, err, away.IP)
	}
	// The next request reads the touched row (nothing new to write) and is
	// cached; the one after is answered from the cache, still with no touch.
	for range 2 {
		if _, _, err := s.ResolveRequest(ctx, phone, away, KindSession); err != nil {
			t.Fatal(err)
		}
		if !cached(s, phone) {
			t.Fatal("the touched token's lookup was not cached again")
		}
	}
	if s.db.ContentGeneration() != gen {
		t.Fatal("resolves after the touch moved the content generation")
	}
}

// Denied (server#258): a lookup that read a token's row before a touch landed
// never caches that row after the touch dropped it. Each round empties the
// cache (a content write), then races a request naming a new app version (a
// touch) with header-less ones (covers, which read the row); once all are done,
// a cached row is the stored one, or covers would carry the old app.
func TestTokenCacheDropsARowReadBeforeATouch(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, _ := s.CreateUser(ctx, "u", "pw-pw-pw-pw", RoleUser)
	tok, _ := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	hash := hashSecret(tok)
	stored := func() ClientInfo {
		var c ClientInfo
		if err := s.db.QueryRowContext(ctx, `SELECT client_app, client_version, client_platform FROM tokens WHERE token_hash = ?`,
			hash).Scan(&c.App, &c.Version, &c.Platform); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for i := range 100 {
		if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled = disabled WHERE 0`); err != nil {
			t.Fatal(err)
		}
		app := ClientInfo{App: "AudioSilo", Version: fmt.Sprintf("2.%d.0", i%2), Platform: "ios"}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for j := range 9 {
			p := Presence{IP: "10.0.0.1"}
			if j == 0 {
				p.Client = app
			}
			wg.Go(func() {
				<-start
				_, _, _ = s.ResolveRequest(ctx, tok, p, KindSession)
			})
		}
		close(start)
		wg.Wait()
		if c, ok := s.tokens.Get(s.db.ContentGeneration(), hash); ok && c.row.client != stored() {
			t.Fatalf("round %d: cached app %+v, stored %+v", i, c.row.client, stored())
		}
	}
}

// Denied (server#258): with the cache warm through touches, a sign-out, a
// demotion and a password change still reach the very next request.
func TestTokenCacheRefusesAfterTouches(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	admin, _ := s.CreateUser(ctx, "boss", "pw-pw-pw-pw", RoleAdmin)
	_, _ = s.CreateUser(ctx, "keeper", "pw-pw-pw-pw", RoleAdmin) // the demotion's other admin
	sess, _ := s.IssueToken(ctx, admin.ID, KindSession, "phone", 0)
	other, _ := s.IssueToken(ctx, admin.ID, KindSession, "laptop", 0)
	for _, ip := range []string{"10.0.0.1", "10.0.0.2"} { // two touches
		if _, _, err := s.ResolveRequest(ctx, sess, Presence{IP: ip}, KindSession); err != nil {
			t.Fatal(err)
		}
	}
	warm(t, s, other, KindSession)

	// A demotion: the next lookup has the new role.
	role := RoleUser
	if err := s.UpdateUser(ctx, admin.ID, UserChanges{Role: &role}); err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.ResolveRequest(ctx, other, Presence{}, KindSession); err != nil || got.Role != RoleUser {
		t.Fatalf("after the demotion: %+v, %v; want role user", got, err)
	}
	// A password change empties the cache: the lookups are read again, and
	// the session it ended (every one but the caller's, sess) is refused at once.
	_, kept, err := s.ResolveRequest(ctx, sess, Presence{IP: "10.0.0.2"}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	warm(t, s, other, KindSession)
	pw := "new-pw-new-pw"
	if err := s.SetPassword(ctx, admin.ID, pw, kept.ID); err != nil {
		t.Fatal(err)
	}
	if cached(s, other) {
		t.Fatal("a password change left a lookup cached")
	}
	if _, _, err := s.ResolveRequest(ctx, other, Presence{}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a session the password change ended = %v, want ErrInvalidToken", err)
	}
	other, _ = s.IssueToken(ctx, admin.ID, KindSession, "tablet", 0)
	if _, err := s.Authenticate(ctx, "boss", "pw-pw-pw-pw"); err == nil {
		t.Fatal("the old password still signs in")
	}
	if _, err := s.Authenticate(ctx, "boss", pw); err != nil {
		t.Fatalf("the new password = %v", err)
	}
	// A sign-out after the touches is refused at once.
	warm(t, s, sess, KindSession)
	if err := s.RevokeToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveRequest(ctx, sess, Presence{IP: "10.0.0.2"}, KindSession); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a signed-out session after touches = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.ResolveRequest(ctx, other, Presence{}, KindSession); err != nil {
		t.Fatalf("the other session after the sign-out = %v, want it valid", err)
	}
}
