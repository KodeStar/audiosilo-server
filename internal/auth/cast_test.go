package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Security-critical: Covers is all that keeps a cast token to its one book.
func TestCastGrantCovers(t *testing.T) {
	t.Parallel()
	g := CastGrant{ID: 1, LibraryID: 7, Path: "Author/Book"}
	allowed := []string{"Author/Book", "Author/Book/01.mp3", "Author/Book/CD1/01.mp3"}
	for _, p := range allowed {
		if !g.Covers(7, p) {
			t.Errorf("Covers(7, %q) = false, want true", p)
		}
	}
	denied := []struct {
		lib  int64
		path string
	}{
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
		if g.Covers(d.lib, d.path) {
			t.Errorf("Covers(%d, %q) = true, want false", d.lib, d.path)
		}
	}
	if (CastGrant{LibraryID: 7}).Covers(7, "anything") {
		t.Error("a grant without a path covers nothing")
	}
}

func TestCastTokenLifecycle(t *testing.T) {
	t.Parallel()
	s, ctx, now := newTestServiceWithClock(t)
	u, err := s.CreateUser(ctx, "sam", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	secret, expires, err := s.IssueCastToken(ctx, u.ID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(CastTokenTTL).UTC().Format(castTime); expires != want || len(expires) != len(castTime) {
		t.Fatalf("expires_at = %q, want %q (fixed-width ms UTC)", expires, want)
	}
	// Only the hash is stored.
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM cast_tokens`).Scan(&stored); err != nil ||
		stored != hashSecret(secret) || strings.Contains(stored, secret) {
		t.Fatalf("stored %q, %v; want only the secret's hash", stored, err)
	}

	// Allowed: the token resolves to its user and book.
	got, grant, err := s.ResolveCastToken(ctx, secret)
	if err != nil || got.ID != u.ID || got.Role != RoleUser || grant.LibraryID != 3 || grant.Path != "Author/Book" {
		t.Fatalf("resolve = %+v %+v %v", got, grant, err)
	}

	// Denied: an unknown secret, an empty one, a session token's secret.
	sess, err := s.IssueToken(ctx, u.ID, KindSession, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "nope", sess} {
		if _, _, err := s.ResolveCastToken(ctx, bad); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("resolve(%q) = %v, want ErrInvalidToken", bad, err)
		}
	}
	// ... and a cast token is never a session or API credential.
	if _, _, err := s.ResolveRequest(ctx, secret, Presence{}, KindSession, KindAPI); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("cast token as a session = %v, want ErrInvalidToken", err)
	}

	// Denied: a disabled user; allowed again once enabled.
	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveCastToken(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("disabled user's token = %v, want ErrInvalidToken", err)
	}
	if err := s.SetDisabled(ctx, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveCastToken(ctx, secret); err != nil {
		t.Fatalf("re-enabled user's token = %v", err)
	}

	// Denied: expired (allowed just before).
	*now = now.Add(CastTokenTTL - time.Millisecond)
	if _, _, err := s.ResolveCastToken(ctx, secret); err != nil {
		t.Fatalf("a token just before its expiry = %v", err)
	}
	*now = now.Add(time.Millisecond)
	if _, _, err := s.ResolveCastToken(ctx, secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token = %v, want ErrInvalidToken", err)
	}

	// A mint deletes the expired tokens (anyone's).
	if _, _, err := s.IssueCastToken(ctx, u.ID, 3, "Author/Book"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cast_tokens WHERE token_hash = ?`, hashSecret(secret)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("expired token rows after a mint = %d, %v", n, err)
	}

	// Deleting the account deletes its tokens.
	live, _, err := s.IssueCastToken(ctx, u.ID, 3, "Author/Book")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ResolveCastToken(ctx, live); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("deleted user's token = %v, want ErrInvalidToken", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cast_tokens`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cast tokens after the account's delete = %d, %v", n, err)
	}
}

// A person keeps at most MaxCastTokens: a mint past it drops the oldest, and
// never another person's.
func TestCastTokenCap(t *testing.T) {
	t.Parallel()
	s, ctx := newTestService(t)
	sam, err := s.CreateUser(ctx, "sam", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	ann, err := s.CreateUser(ctx, "ann", "", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	annTok, _, err := s.IssueCastToken(ctx, ann.ID, 1, "B")
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for range MaxCastTokens + 2 {
		sec, _, err := s.IssueCastToken(ctx, sam.ID, 1, "A")
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, sec)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cast_tokens WHERE user_id = ?`, sam.ID).Scan(&n); err != nil || n != MaxCastTokens {
		t.Fatalf("sam's cast tokens = %d, %v; want %d", n, err, MaxCastTokens)
	}
	for i, sec := range secrets {
		_, _, err := s.ResolveCastToken(ctx, sec)
		if i < 2 && !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("oldest token %d still works: %v", i, err)
		}
		if i >= 2 && err != nil {
			t.Fatalf("token %d = %v", i, err)
		}
	}
	if _, _, err := s.ResolveCastToken(ctx, annTok); err != nil {
		t.Fatalf("another person's token was dropped: %v", err)
	}
}
