package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// Security-critical: covers is all that keeps a cast token to its one book.
func TestCastGrantCovers(t *testing.T) {
	t.Parallel()
	g := castGrant{libraryID: sql.NullInt64{Int64: 7, Valid: true}, path: "Author/Book"}
	allowed := []string{"Author/Book", "Author/Book/01.mp3", "Author/Book/CD1/01.mp3"}
	for _, p := range allowed {
		if !g.covers(bookMedia{7, p}) {
			t.Errorf("covers(7, %q) = false, want true", p)
		}
	}
	denied := []bookMedia{
		{7, "Author/Book 2"},        // a sibling sharing the prefix
		{7, "Author/Book 2/01.mp3"}, // ... and a file in it
		{7, "Author/Bookish.mp3"},   // a file sharing the prefix
		{7, "Author"},               // the folder above
		{7, "Author/Other/01.mp3"},  // another book
		{7, ""},                     // the library root
		{8, "Author/Book"},          // the same path in another library
		{8, "Author/Book/01.mp3"},   // ... and a file there
		{7, "Author/Other.mp3"},     // "Author/Book/../Other.mp3" as the api cleans it first
	}
	for _, d := range denied {
		if g.covers(d) {
			t.Errorf("covers(%d, %q) = true, want false", d.libraryID, d.path)
		}
	}
	if (castGrant{libraryID: sql.NullInt64{Int64: 7, Valid: true}}).covers(bookMedia{7, "anything"}) {
		t.Error("a grant without a path covers nothing")
	}
	if (castGrant{path: "Author/Book"}).covers(bookMedia{0, "Author/Book"}) {
		t.Error("a token without a grant covers nothing")
	}
}

// castSender is a user signed in on one device: the session that mints.
func castSender(t *testing.T, s *Service, ctx context.Context, name string) (*User, string, int64) {
	t.Helper()
	u, err := s.CreateUser(ctx, name, "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, cred, err := s.ResolveRequest(ctx, sess, Presence{}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	return u, sess, cred.ID
}

// resolveCast resolves secret as a book media request on (3, path), the way the
// api's book media routes do.
func resolveCast(s *Service, ctx context.Context, secret, path string) (*User, Credential, error) {
	return s.ResolveCastRequest(ctx, secret, Presence{}, 3, path, KindSession, KindAPI, KindCast)
}

func TestCastTokenLifecycle(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	u, sess, sessID := castSender(t, s, ctx, "sam")
	secret, expires, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if want := now.UTC().Add(castTokenTTL).Truncate(time.Second).Format(castTime); expires != want || len(expires) != len(castTime) {
		t.Fatalf("expires_at = %q, want %q (fixed-width ms UTC)", expires, want)
	}
	// Only the hash is stored, as a tokens row of kind cast with its grant.
	var stored, path string
	if err := s.db.QueryRowContext(ctx,
		`SELECT t.token_hash, g.rel_path FROM tokens t JOIN cast_grants g ON g.token_id = t.id WHERE t.kind = 'cast'`).
		Scan(&stored, &path); err != nil || stored != hashSecret(secret) || strings.Contains(stored, secret) || path != "Author/Book" {
		t.Fatalf("stored %q %q, %v; want only the secret's hash", stored, path, err)
	}

	// Allowed: the token resolves to its user on its book's media, as a cast
	// credential acting for the sender's device.
	got, cred, err := resolveCast(s, ctx, secret, "Author/Book/01.mp3")
	if err != nil || got.ID != u.ID || got.Role != RoleUser || cred.Kind != KindCast || cred.ID == sessID || cred.DeviceID != sessID {
		t.Fatalf("resolve = %+v %+v %v", got, cred, err)
	}
	// ... and the session still resolves through the same entry point, as itself.
	if _, cred, err := resolveCast(s, ctx, sess, "Author/Book"); err != nil || cred.Kind != KindSession || cred.DeviceID != sessID {
		t.Fatalf("session on book media = %+v %v", cred, err)
	}

	// Denied: another book, an unknown secret, an empty one.
	for _, bad := range []struct{ secret, path string }{
		{secret, "Author/Book 2"}, {secret, ""}, {"", "Author/Book"}, {"nope", "Author/Book"},
	} {
		if _, _, err := resolveCast(s, ctx, bad.secret, bad.path); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("resolve(%q, %q) = %v, want ErrInvalidToken", bad.secret, bad.path, err)
		}
	}
	// ... where the route doesn't list KindCast.
	if _, _, err := s.ResolveCastRequest(ctx, secret, Presence{}, 3, "Author/Book", KindSession, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cast token where cast isn't accepted = %v, want ErrInvalidToken", err)
	}
	// ... and through ResolveRequest, even listing KindCast: it knows no path.
	if _, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession, KindAPI, KindCast); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cast token as a session = %v, want ErrInvalidToken", err)
	}

	// Denied: a disabled user; allowed again once enabled.
	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled user's token = %v, want ErrInvalidToken", err)
	}
	if err := s.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); err != nil {
		t.Fatalf("re-enabled user's token = %v", err)
	}

	// Denied: expired (allowed just before; the expiry is in whole seconds).
	*now = now.Add(castTokenTTL - time.Second)
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); err != nil {
		t.Fatalf("a token just before its expiry = %v", err)
	}
	*now = now.Add(2 * time.Second)
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token = %v, want ErrInvalidToken", err)
	}

	// A mint deletes the expired cast tokens (anyone's) and their grants.
	if _, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM tokens WHERE token_hash = ?) + (SELECT COUNT(*) FROM cast_grants)`, hashSecret(secret)).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("expired token rows + grants after a mint = %d, %v; want the new grant only", n, err)
	}

	// Deleting the account deletes its tokens and their grants.
	live, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteUser(ctx, u.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, live, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("deleted user's token = %v, want ErrInvalidToken", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM tokens WHERE kind = 'cast') + (SELECT COUNT(*) FROM cast_grants)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cast tokens after the account's delete = %d, %v", n, err)
	}
}

// A cast token lives only as long as the sender's token that minted it: signing
// that device out ends it, whichever way, while another device's leave it be.
func TestCastTokenDiesWithItsSender(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, sess, sessID := castSender(t, s, ctx, "sam")
	other, err := s.IssueToken(ctx, u.ID, KindSession, "tablet", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, otherCred, err := s.ResolveRequest(ctx, other, Presence{}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(parent int64) string {
		t.Helper()
		secret, _, err := s.IssueCastToken(ctx, u.ID, parent, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	fromPhone, fromTablet := mint(sessID), mint(otherCred.ID)

	// Denied: the phone signs out (POST /auth/logout).
	if err := s.RevokeToken(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, fromPhone, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cast token of a signed-out sender = %v, want ErrInvalidToken", err)
	}
	// Allowed: the tablet's is untouched.
	if _, _, err := resolveCast(s, ctx, fromTablet, "Author/Book"); err != nil {
		t.Fatalf("another device's cast token = %v", err)
	}
	// Denied: the tablet signed out from "My devices".
	if err := s.RevokeOwnDevice(ctx, u.ID, otherCred.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, fromTablet, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cast token of a device signed out from My devices = %v, want ErrInvalidToken", err)
	}
}

// A cast token is never a device: the device lists leave it out and a device
// sign-out can't name it (like a pairing token).
func TestCastTokenIsNoDevice(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, _, sessID := castSender(t, s, ctx, "sam")
	secret, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	_, cred, err := resolveCast(s, ctx, secret, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []int64{0, u.ID} {
		devices, err := s.ListDevices(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		if len(devices) != 1 || devices[0].ID != sessID {
			t.Fatalf("devices(%d) = %+v; want the session only", owner, devices)
		}
	}
	if err := s.RevokeOwnDevice(ctx, u.ID, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevokeOwnDevice(cast) = %v, want ErrNotFound", err)
	}
	if err := s.RevokeDevice(ctx, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RevokeDevice(cast) = %v, want ErrNotFound", err)
	}
}

// A person keeps at most maxCastTokens: a mint past it drops the oldest, and
// never another person's.
func TestCastTokenCap(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam, _, samSess := castSender(t, s, ctx, "sam")
	ann, _, annSess := castSender(t, s, ctx, "ann")
	annTok, _, err := s.IssueCastToken(ctx, ann.ID, annSess, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for range maxCastTokens + 2 {
		sec, _, err := s.IssueCastToken(ctx, sam.ID, samSess, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, sec)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ? AND kind = 'cast'`, sam.ID).Scan(&n); err != nil || n != maxCastTokens {
		t.Fatalf("sam's cast tokens = %d, %v; want %d", n, err, maxCastTokens)
	}
	for i, sec := range secrets {
		_, _, err := resolveCast(s, ctx, sec, "Author/Book")
		if i < 2 && !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("oldest token %d still works: %v", i, err)
		}
		if i >= 2 && err != nil {
			t.Fatalf("token %d = %v", i, err)
		}
	}
	if _, _, err := resolveCast(s, ctx, annTok, "Author/Book"); err != nil {
		t.Fatalf("another person's token was dropped: %v", err)
	}
}

// Security-critical: ResolveCastRequest cleans the path before the grant's prefix
// rule judges it, so a traversal out of the book never passes, whoever calls.
func TestCastTokenPathCleaned(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	u, _, sessID := castSender(t, s, ctx, "sam")
	secret, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	// Allowed: paths that clean to the book or a file of it.
	for _, p := range []string{"Author/Book/./01.mp3", "/Author/Book/01.mp3", "Author//Book/CD1/../01.mp3", "Author/Book/"} {
		if _, _, err := resolveCast(s, ctx, secret, p); err != nil {
			t.Errorf("resolve(%q) = %v, want allowed", p, err)
		}
	}
	// Denied: paths that clean to another book, the folder above or the root.
	for _, p := range []string{"Author/Book/../Book 2/01.mp3", "Author/Book/../../Other/x.m4b", "Author/Book/..", "Author/Book/../../.."} {
		if _, _, err := resolveCast(s, ctx, secret, p); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("resolve(%q) = %v, want ErrInvalidToken", p, err)
		}
	}
}

// A dead cast token (its sender signed out, or past its 24 h) goes with its grant
// at the daily retention, and the receiver's address with it; a live one stays.
func TestPruneCastTokens(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	u, _, phoneID := castSender(t, s, ctx, "sam")
	tablet, err := s.IssueToken(ctx, u.ID, KindSession, "tablet", 0)
	if err != nil {
		t.Fatal(err)
	}
	_, tabletCred, err := s.ResolveRequest(ctx, tablet, Presence{}, KindSession)
	if err != nil {
		t.Fatal(err)
	}
	mint := func(parent int64) string {
		t.Helper()
		secret, _, err := s.IssueCastToken(ctx, u.ID, parent, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	live, fromTablet := mint(phoneID), mint(tabletCred.ID)
	// The receiver plays: its address lands on the cast token's own row.
	if _, _, err := s.ResolveCastRequest(ctx, fromTablet, Presence{IP: "203.0.113.9"}, 3, "Author/Book", KindCast); err != nil {
		t.Fatal(err)
	}
	rows := func(secret string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT (SELECT COUNT(*) FROM tokens WHERE token_hash = ?1)
			      + (SELECT COUNT(*) FROM cast_grants g JOIN tokens t ON t.id = g.token_id WHERE t.token_hash = ?1)`,
			hashSecret(secret)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	prune := func() {
		t.Helper()
		if err := s.PruneCastTokens(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Allowed: live tokens stay.
	prune()
	if rows(live) != 2 || rows(fromTablet) != 2 {
		t.Fatalf("live cast tokens pruned: %d, %d rows", rows(live), rows(fromTablet))
	}
	// Denied: the tablet signs out, and its cast token goes, address and grant.
	if err := s.RevokeOwnDevice(ctx, u.ID, tabletCred.ID); err != nil {
		t.Fatal(err)
	}
	prune()
	if rows(fromTablet) != 0 || rows(live) != 2 {
		t.Fatalf("after its sender's sign-out: %d rows of its cast token, %d of the live one", rows(fromTablet), rows(live))
	}
	// Denied: past its 24 h the phone's goes too.
	*now = now.Add(castTokenTTL + time.Second)
	prune()
	if rows(live) != 0 {
		t.Fatalf("an expired cast token outlived the retention: %d rows", rows(live))
	}
}
