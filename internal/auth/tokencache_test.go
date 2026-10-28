package auth

import (
	"errors"
	"testing"
	"time"
)

// cached reports whether the token cache holds secret's lookup for the current
// generation (the next request is answered from it).
func cached(s *Service, secret string) bool {
	_, ok := s.tokens.Get(s.db.Generation(), hashSecret(secret))
	return ok
}

// warm resolves secret until its lookup is answered from the cache (the first
// resolve may touch the token, a write, which leaves its lookup unused).
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

// Allowed: a session and an API key are answered from the cache while nothing is
// written, with the same user and credential the database gives.
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
