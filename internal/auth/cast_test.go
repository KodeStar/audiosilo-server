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
	sess, id := mustSession(t, s, ctx, u.ID, "phone")
	return u, sess, id
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
	// Minted beside it and never used.
	idle, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if want := now.UTC().Add(castTokenTTL).Truncate(time.Second).Format(millisUTC); expires != want || len(expires) != len(millisUTC) {
		t.Fatalf("expires_at = %q, want %q (fixed-width ms UTC)", expires, want)
	}
	// Only the hash is stored, as a tokens row of kind cast with its grant.
	var stored, path string
	if err := s.db.QueryRowContext(ctx,
		`SELECT t.token_hash, g.rel_path FROM tokens t JOIN cast_grants g ON g.token_id = t.id WHERE t.kind = 'cast' ORDER BY t.id`).
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
	if err := s.UpdateUser(ctx, u.ID, UserChanges{Disabled: new(true)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled user's token = %v, want ErrInvalidToken", err)
	}
	if err := s.UpdateUser(ctx, u.ID, UserChanges{Disabled: new(false)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); err != nil {
		t.Fatalf("re-enabled user's token = %v", err)
	}

	// Denied: expired, castTokenTTL after its mint with no use since (the
	// expiry is in whole seconds). Allowed: a use just before its expiry, which
	// moves it on (castExpiry, TestCastTokenExpirySlidesWhileInUse).
	*now = now.Add(castTokenTTL - time.Second)
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); err != nil {
		t.Fatalf("a token just before its expiry = %v", err)
	}
	*now = now.Add(2 * time.Second)
	if _, _, err := resolveCast(s, ctx, idle, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token = %v, want ErrInvalidToken", err)
	}
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); err != nil {
		t.Fatalf("a token used just before its expiry = %v", err)
	}
	*now = now.Add(castTokenTTL)
	if _, _, err := resolveCast(s, ctx, secret, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("a token castTokenTTL after its last use = %v, want ErrInvalidToken", err)
	}

	// A mint deletes the expired cast tokens (anyone's) and their grants.
	if _, _, err := s.IssueCastToken(ctx, u.ID, sessID, 3, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM tokens WHERE token_hash IN (?, ?)) + (SELECT COUNT(*) FROM cast_grants)`,
		hashSecret(secret), hashSecret(idle)).Scan(&n); err != nil || n != 1 {
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

// A device keeps at most maxCastTokens: a mint past it drops its least recently
// used (here its oldest, as none is used), and never another person's.
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

// Past the cap the least recently used token goes, not the oldest minted: a
// sender re-minting never ends the token its receiver is playing with.
// Allowed: the first token minted, in use since, survives a mint at the cap.
// Denied: the idle token minted after it is evicted and refused.
func TestCastTokenCapEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	sam, _, phone := castSender(t, s, ctx, "sam")
	playing, _, err := s.IssueCastToken(ctx, sam.ID, phone, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	var idle []string
	for range maxCastTokens - 1 {
		sec, _, err := s.IssueCastToken(ctx, sam.ID, phone, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, sec)
	}
	// The receiver keeps reading the book: its token's last_seen moves past
	// every other token's mint.
	*now = now.Add(10 * time.Minute)
	if _, _, err := resolveCast(s, ctx, playing, "Author/Book/01.mp3"); err != nil {
		t.Fatalf("the playing token = %v", err)
	}
	*now = now.Add(time.Minute)
	newest, _, err := s.IssueCastToken(ctx, sam.ID, phone, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveCast(s, ctx, playing, "Author/Book/02.mp3"); err != nil {
		t.Fatalf("the token in use was evicted by a mint at the cap: %v", err)
	}
	if _, _, err := resolveCast(s, ctx, newest, "Author/Book"); err != nil {
		t.Fatalf("the new token = %v", err)
	}
	if _, _, err := resolveCast(s, ctx, idle[0], "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the least recently used token = %v, want ErrInvalidToken", err)
	}
	for i, sec := range idle[1:] {
		if _, _, err := resolveCast(s, ctx, sec, "Author/Book"); err != nil {
			t.Fatalf("idle token %d = %v", i+1, err)
		}
	}
}

// A mint never deletes the token it returns, whatever the clock says: after the
// clock steps back, every other token's time is ahead of the new one's mint.
// Allowed: the new cast and editor tokens resolve. Denied: one of the others
// goes in their place, so each use still keeps its cap.
func TestCastTokenMintSurvivesClockStepBack(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	sam, _, phone := castSender(t, s, ctx, "sam")
	for range maxCastTokens {
		if _, _, err := s.IssueCastToken(ctx, sam.ID, phone, 3, "Author/Book"); err != nil {
			t.Fatal(err)
		}
	}
	for range maxEditorTokens {
		if _, _, err := s.IssueEditorToken(ctx, sam.ID, phone, 3, "Author/Book"); err != nil {
			t.Fatal(err)
		}
	}
	*now = now.Add(-time.Hour)
	for _, c := range []struct {
		name         string
		issue        func(context.Context, int64, int64, int64, string) (string, string, error)
		editor, keep int
	}{
		{"cast", s.IssueCastToken, 0, maxCastTokens},
		{"editor", s.IssueEditorToken, 1, maxEditorTokens},
	} {
		sec, _, err := c.issue(ctx, sam.ID, phone, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveCast(s, ctx, sec, "Author/Book"); err != nil {
			t.Fatalf("the %s token just minted, after the clock stepped back = %v", c.name, err)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens t JOIN cast_grants g ON g.token_id = t.id
			WHERE t.user_id = ? AND t.kind = 'cast' AND g.editor = ?`, sam.ID, c.editor).Scan(&n); err != nil || n != c.keep {
			t.Fatalf("sam's %s tokens = %d, %v; want %d", c.name, n, err, c.keep)
		}
	}
}

// The cap counts per sending device (server#216): a run of casts on one device
// never ends the cast another device of the same person is playing. Allowed:
// device A's one token outlives device B minting 25. Denied: B's own oldest,
// past B's cap.
func TestCastTokenCapPerDevice(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam, _, phone := castSender(t, s, ctx, "sam")
	_, tablet := mustSession(t, s, ctx, sam.ID, "tablet")
	playing, _, err := s.IssueCastToken(ctx, sam.ID, phone, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	var fromTablet []string
	for range 25 {
		sec, _, err := s.IssueCastToken(ctx, sam.ID, tablet, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		fromTablet = append(fromTablet, sec)
	}
	if _, _, err := resolveCast(s, ctx, playing, "Author/Book/01.mp3"); err != nil {
		t.Fatalf("the phone's cast ended by the tablet's mints: %v", err)
	}
	for i, sec := range fromTablet {
		_, _, err := resolveCast(s, ctx, sec, "Author/Book")
		if i < 25-maxCastTokens && !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("the tablet's oldest token %d still works: %v", i, err)
		}
		if i >= 25-maxCastTokens && err != nil {
			t.Fatalf("the tablet's token %d = %v", i, err)
		}
	}
}

// Over all of a person's devices at most maxCastTokensPerPerson stay: past it
// the person's least recently used go (here the oldest, as none is used),
// whichever device minted them.
func TestCastTokenCapPerPerson(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam, _, first := castSender(t, s, ctx, "sam")
	devices := []int64{first}
	for len(devices)*maxCastTokens <= maxCastTokensPerPerson {
		_, d := mustSession(t, s, ctx, sam.ID, "device")
		devices = append(devices, d)
	}
	var oldest string
	for _, d := range devices {
		for range maxCastTokens {
			sec, _, err := s.IssueCastToken(ctx, sam.ID, d, 3, "Author/Book")
			if err != nil {
				t.Fatal(err)
			}
			if oldest == "" {
				oldest = sec
			}
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens WHERE user_id = ? AND kind = 'cast'`, sam.ID).Scan(&n); err != nil || n != maxCastTokensPerPerson {
		t.Fatalf("sam's cast tokens = %d, %v; want %d", n, err, maxCastTokensPerPerson)
	}
	if _, _, err := resolveCast(s, ctx, oldest, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the person's oldest token past the backstop = %v, want ErrInvalidToken", err)
	}
}

// The console's chapter editor mints editor tokens: the same one-book token, on
// a shorter life and a cap of its own, so editing many books never drops a cast
// token (a receiver mid-book), and a run of casts never drops the editor's.
func TestEditorTokensCountApartFromCasts(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	sam, _, samSess := castSender(t, s, ctx, "sam")
	cast, _, err := s.IssueCastToken(ctx, sam.ID, samSess, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	var editors []string
	for range maxEditorTokens + 2 {
		sec, expires, err := s.IssueEditorToken(ctx, sam.ID, samSess, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		if want := now.UTC().Add(editorTokenTTL).Truncate(time.Second).Format(millisUTC); expires != want {
			t.Fatalf("editor token expires %q, want %q", expires, want)
		}
		editors = append(editors, sec)
	}
	// Allowed: the cast token outlives every editor mint past the editor's cap.
	if _, _, err := resolveCast(s, ctx, cast, "Author/Book/01.mp3"); err != nil {
		t.Fatalf("editor mints dropped a cast token: %v", err)
	}
	// Denied: the editor's own oldest, past its cap. An editor token reads what a
	// cast token reads (its book) and nothing else.
	for i, sec := range editors {
		_, _, err := resolveCast(s, ctx, sec, "Author/Book/01.mp3")
		if i < 2 && !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("oldest editor token %d still works: %v", i, err)
		}
		if i >= 2 && err != nil {
			t.Fatalf("editor token %d = %v", i, err)
		}
	}
	newest := editors[len(editors)-1]
	if _, _, err := resolveCast(s, ctx, newest, "Author/Book 2/01.mp3"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an editor token on another book = %v, want ErrInvalidToken", err)
	}
	if _, _, err := s.ResolveRequest(ctx, newest, Presence{}, KindSession, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an editor token as a session = %v, want ErrInvalidToken", err)
	}
	// The other way: casts past their cap leave the editor's tokens alone.
	for range maxCastTokens + 1 {
		if _, _, err := s.IssueCastToken(ctx, sam.ID, samSess, 3, "Author/Book"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := resolveCast(s, ctx, newest, "Author/Book"); err != nil {
		t.Fatalf("cast mints dropped an editor token: %v", err)
	}
	if _, _, err := resolveCast(s, ctx, cast, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("the oldest cast token outlived its own cap: %v", err)
	}
	// Denied: past its shorter life (a cast token minted with it still works).
	late, _, err := s.IssueCastToken(ctx, sam.ID, samSess, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(editorTokenTTL + time.Second)
	if _, _, err := resolveCast(s, ctx, newest, "Author/Book"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("an editor token past its life = %v, want ErrInvalidToken", err)
	}
	if _, _, err := resolveCast(s, ctx, late, "Author/Book"); err != nil {
		t.Fatalf("a cast token four hours old = %v", err)
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

// castTokenID is the tokens row id of cast token secret.
func castTokenID(t *testing.T, s *Service, ctx context.Context, secret string) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM tokens WHERE token_hash = ? AND kind = 'cast'`, hashSecret(secret)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// server#446: a cast token in use outlives its first castTokenTTL, each use
// (a request, or KeepCastToken while a long response runs) moving its expiry to
// castTokenTTL from then, never past castTokenMaxLife after its mint. Allowed:
// a token used once a day lives into its third day, and one kept in use by a
// long response alone. Denied: past the bound however it is used, a day after
// its last use, an expired token kept (never revived), and an editor token,
// whose life is fixed.
func TestCastTokenExpirySlidesWhileInUse(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	sam, _, phone := castSender(t, s, ctx, "sam")
	mint := func(issue func(context.Context, int64, int64, int64, string) (string, string, error)) string {
		t.Helper()
		sec, _, err := issue(ctx, sam.ID, phone, 3, "Author/Book")
		if err != nil {
			t.Fatal(err)
		}
		return sec
	}
	start := *now
	at := func(d time.Duration) { *now = start.Add(d) }
	playing, kept, idle := mint(s.IssueCastToken), mint(s.IssueCastToken), mint(s.IssueCastToken)
	editor := mint(s.IssueEditorToken)
	use := func(what, secret string, live bool) {
		t.Helper()
		_, _, err := resolveCast(s, ctx, secret, "Author/Book/01.mp3")
		wantLive(t, what, live, err)
	}
	keep := func(secret string) {
		t.Helper()
		if err := s.KeepCastToken(ctx, castTokenID(t, s, ctx, secret)); err != nil {
			t.Fatal(err)
		}
	}

	at(3 * time.Hour)
	use("the editor token in use", editor, true)
	at(editorTokenTTL + time.Second)
	use("the editor token past its fixed life", editor, false)

	// A use and a keep that move the expiry are user state (store's tokens
	// rule): the content caches stay warm.
	at(23 * time.Hour)
	gen := s.db.ContentGeneration()
	use("the playing token on its first day", playing, true)
	keep(kept)
	if s.db.ContentGeneration() != gen {
		t.Fatalf("moving a cast token's expiry moved the content generation %d -> %d", gen, s.db.ContentGeneration())
	}
	at(25 * time.Hour)
	use("the idle token a day after its mint", idle, false)
	use("the playing token past its first expiry", playing, true)
	use("the token a long response kept in use", kept, true)
	keep(idle)
	use("the expired token after a keep", idle, false)

	at(48 * time.Hour)
	use("the playing token on its second day", playing, true)
	at(71 * time.Hour)
	use("the playing token on its third day", playing, true)
	at(castTokenMaxLife - time.Second)
	use("the playing token just inside its bound", playing, true)
	at(castTokenMaxLife + time.Second)
	use("the playing token past castTokenMaxLife", playing, false)
	use("the kept token a day after its last use", kept, false)
}

// server#446: a request touches a cast token at most once a CastTouchInterval
// (other tokens once a touchInterval), and in milliseconds, so a token a
// receiver reads from never ranks behind one minted seconds before its last
// use, nor one minted in the same second before its first. Allowed: the token
// in use survives a mint at the cap both ways. Denied: the least recently used
// idle token goes instead.
func TestCastTokenInUseOutranksTheRecentMints(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name            string
		usedAtMint      bool
		mintIdle, useAt time.Duration
	}{
		{"within the minute", true, 5 * time.Second, 30 * time.Second},
		{"within the second", false, 100 * time.Millisecond, 500 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s, ctx, now := newTestServiceWithClock(t)
			sam, _, phone := castSender(t, s, ctx, "sam")
			start := now.Truncate(time.Second).Add(time.Second)
			*now = start
			mint := func() string {
				t.Helper()
				sec, _, err := s.IssueCastToken(ctx, sam.ID, phone, 3, "Author/Book")
				if err != nil {
					t.Fatal(err)
				}
				return sec
			}
			playing := mint()
			if c.usedAtMint {
				if _, _, err := resolveCast(s, ctx, playing, "Author/Book/01.mp3"); err != nil {
					t.Fatal(err)
				}
			}
			*now = start.Add(c.mintIdle)
			var idle []string
			for range maxCastTokens - 1 {
				idle = append(idle, mint())
			}
			*now = start.Add(c.useAt)
			if _, _, err := resolveCast(s, ctx, playing, "Author/Book/02.mp3"); err != nil {
				t.Fatal(err)
			}
			*now = now.Add(c.mintIdle / 5)
			newest := mint()
			_, _, err := resolveCast(s, ctx, playing, "Author/Book/03.mp3")
			wantLive(t, "the token in use after a mint at the cap", true, err)
			_, _, err = resolveCast(s, ctx, newest, "Author/Book")
			wantLive(t, "the new token", true, err)
			_, _, err = resolveCast(s, ctx, idle[0], "Author/Book")
			wantLive(t, "the least recently used idle token", false, err)
		})
	}
}
