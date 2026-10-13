package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The sibling of the fixture's Cradle folder whose name shares its prefix.
const (
	cradleTwo   = "Will Wight/Cradle 2"
	cradleTwoF  = "Will Wight/Cradle 2/01 - Unsouled.m4b"
	cradlePart2 = "Will Wight/Cradle/02 - Soulsmith.m4b"
)

// castEnv: a copy of the fixture library (Cradle with a sidecar cover, which the
// fixtures' audio has none of) plus a "Cradle 2" sibling (lib), a second library
// over the fixtures (other), sam allowed both (lib through a share the test can
// revoke), kid only the "Will Wight" folder of lib.
type castEnv struct {
	*testEnv
	lib, other int64
	sam, kid   int64
	samTok     string
	kidTok     string
	demoTok    string
	samShare   int64
}

func newCastEnv(t *testing.T) *castEnv {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	root := copyFixtures(t, cradlePart, cradlePart2, mistbornBook+"/01 - The Final Empire.m4b")
	copyFixture(t, root, cradlePart, cradleTwoF)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(cradleBook), "cover.jpg"), testImage(t, 64, 64, false), 0o644); err != nil {
		t.Fatal(err)
	}
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Copy", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	other := newFixtureLibrary(t, e)
	c := &castEnv{testEnv: e, lib: lib.ID, other: other.ID}
	c.sam, c.samTok = newUserWithSession(t, e, "sam", false)
	c.kid, c.kidTok = newUserWithSession(t, e, "kid", false)
	demo, demoTok := newUserWithSession(t, e, "demo_1", true)
	c.demoTok = demoTok
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Everything",
		Paths: []catalog.PathRule{{LibraryID: lib.ID, Path: ""}}})
	if err != nil {
		t.Fatal(err)
	}
	c.samShare = share.ID
	if err := e.cat.GrantShare(ctx, c.sam, share.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{c.sam, demo} {
		if err := e.cat.GrantWholeLibrary(ctx, id, other.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.cat.GrantWholeLibrary(ctx, demo, lib.ID); err != nil {
		t.Fatal(err)
	}
	grantWightOnly(t, e, lib.ID, c.kid)
	return c
}

// mint POSTs /cast/token as tok and returns the status and body.
func (c *castEnv) mint(t *testing.T, tok string, libID int64, path string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"library_id": libID, "path": path})
	resp, b := c.do(t, "POST", "/api/v1/cast/token", tok, string(body))
	return resp.StatusCode, b
}

// castToken mints for sam on lib's Cradle, wanting 201.
func (c *castEnv) castToken(t *testing.T, path string) string {
	t.Helper()
	status, body := c.mint(t, c.samTok, c.lib, path)
	if status != http.StatusCreated {
		t.Fatalf("mint %s = %d %s", path, status, body)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.Token == "" || len(out.ExpiresAt) != len("2006-01-02T15:04:05.000Z") {
		t.Fatalf("mint body = %s", body)
	}
	return out.Token
}

// media GETs /libraries/{libID}/{endpoint}?path=&token= with no Authorization
// header (as a receiver does), plus extra query.
func (c *castEnv) media(t *testing.T, libID int64, endpoint, path, token, extra string) *http.Response {
	t.Helper()
	resp, _ := c.do(t, "GET", mediaURL(libID, endpoint, path, token)+extra, "", "")
	return resp
}

func TestCastTokenMint(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	// Allowed: the book's path, and a part path (the token then covers the book).
	status, body := c.mint(t, c.samTok, c.lib, cradleBook)
	var raw map[string]json.RawMessage
	if status != http.StatusCreated || json.Unmarshal([]byte(body), &raw) != nil {
		t.Fatalf("mint = %d %s", status, body)
	}
	if keys := slices.Sorted(maps.Keys(raw)); strings.Join(keys, ",") != "expires_at,token" {
		t.Fatalf("mint keys = %v", keys)
	}
	part := c.castToken(t, cradlePart)
	if resp := c.media(t, c.lib, "stream", cradlePart2, part, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a part-path token on the book's other part = %d", resp.StatusCode)
	}
	// Allowed: a demo account can cast (it can play).
	if status, body := c.mint(t, c.demoTok, c.lib, cradleBook); status != http.StatusCreated {
		t.Fatalf("demo mint = %d %s", status, body)
	}

	// Denied, as the media routes answer: out of scope 403, no such book 404, no
	// such library 403 (a member has no scope there; an admin would get 404), a
	// bad body 400, anonymous 401.
	for _, d := range []struct {
		tok    string
		lib    int64
		path   string
		status int
	}{
		{c.kidTok, c.lib, mistbornBook, http.StatusForbidden},
		{c.kidTok, c.other, cradleBook, http.StatusForbidden},
		{c.samTok, c.lib, "Nobody/Nothing", http.StatusNotFound},
		{c.samTok, 999, cradleBook, http.StatusForbidden},
		{c.samTok, c.lib, "", http.StatusBadRequest},
		{"", c.lib, cradleBook, http.StatusUnauthorized},
	} {
		if status, body := c.mint(t, d.tok, d.lib, d.path); status != d.status {
			t.Errorf("mint(%d, %q) = %d %s; want %d", d.lib, d.path, status, body, d.status)
		}
	}
	for _, bad := range []string{`{"library_id":1,"path":"x","extra":1}`, `nope`} {
		if resp, body := c.do(t, "POST", "/api/v1/cast/token", c.samTok, bad); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("mint body %s = %d %s; want 400", bad, resp.StatusCode, body)
		}
	}
	// Denied: an API key mints no credential (denyAPIKey), not even a cast token.
	_, key := c.mintAPIKey(t, c.samTok, "script")
	if status, body := c.mint(t, key, c.lib, cradleBook); status != http.StatusForbidden {
		t.Fatalf("API-key mint = %d %s; want 403", status, body)
	}
	// Denied: a cast token is no bearer credential, so it can't mint another.
	if status, _ := c.mint(t, part, c.lib, cradleBook); status != http.StatusUnauthorized {
		t.Fatalf("mint with a cast token = %d; want 401", status)
	}
}

// Security-critical: a cast token reaches its own book's cover and stream as
// ?token=, and nothing else.
func TestCastTokenMediaAuth(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	tok := c.castToken(t, cradleBook)

	// Allowed: the book's files and its cover (any size), as ?token= with no header.
	resp := c.media(t, c.lib, "stream", cradlePart, tok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d", resp.StatusCode)
	}
	// The receiver loads it from another site: the resource policy lets it.
	if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != "cross-origin" {
		t.Fatalf("cast stream CORP = %q, want cross-origin", got)
	}
	if resp := c.media(t, c.lib, "stream", cradlePart2, tok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the other part = %d", resp.StatusCode)
	}
	for _, extra := range []string{"", "&size=160"} {
		resp := c.media(t, c.lib, "cover", cradleBook, tok, extra)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
			t.Fatalf("cover%s = %d, CORP %q; want 200 cross-origin", extra, resp.StatusCode, resp.Header.Get("Cross-Origin-Resource-Policy"))
		}
	}
	// Allowed: a transcode request authenticates too (no ffmpeg here: 503 after auth).
	if resp := c.media(t, c.lib, "stream", cradlePart, tok, "&transcode=1&t=30"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("transcode = %d; want 503 (authenticated, no ffmpeg)", resp.StatusCode)
	}

	// Denied: another book, a sibling sharing the prefix, a traversal out of the
	// book, the same path in another library.
	for _, d := range []struct {
		lib      int64
		endpoint string
		path     string
	}{
		{c.lib, "stream", mistbornBook + "/01 - The Final Empire.m4b"},
		{c.lib, "cover", mistbornBook},
		{c.lib, "stream", cradleTwoF},
		{c.lib, "cover", cradleTwo},
		{c.lib, "stream", cradleBook + "/../Cradle 2/01 - Unsouled.m4b"},
		{c.lib, "cover", "Will Wight"},
		{c.other, "stream", cradlePart},
		{c.other, "cover", cradleBook},
	} {
		if resp := c.media(t, d.lib, d.endpoint, d.path, tok, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %d %q = %d; want 401", d.endpoint, d.lib, d.path, resp.StatusCode)
		}
	}
	// Denied: any other route, even one that takes ?token= (a community cover)
	// and the book's own non-media routes.
	for _, endpoint := range []string{"meta/cover", "item", "chapters", "progress"} {
		if resp := c.media(t, c.lib, endpoint, cradleBook, tok, "&url=x&size=160"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with a cast token = %d; want 401", endpoint, resp.StatusCode)
		}
	}
	// Denied: as a bearer header, on the media routes or anywhere else.
	if resp, _ := c.do(t, "GET", bookURL(c.lib, "stream", cradlePart), tok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cast token as a header on stream = %d", resp.StatusCode)
	}
	if resp, _ := c.do(t, "GET", "/api/v1/me", tok, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cast token as a header on /me = %d", resp.StatusCode)
	}
	// ... nor beside a header naming another credential, a bearer one or not (a
	// basic-auth proxy's): only a request with no Authorization header at all.
	if resp, _ := c.do(t, "GET", mediaURL(c.lib, "stream", cradlePart, tok), "not-a-token", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cast token beside a bad header = %d", resp.StatusCode)
	}
	basic := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("sam:secret"))}
	if resp, _ := c.doHeaders(t, "GET", mediaURL(c.lib, "stream", cradlePart, tok), "", "", basic); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cast token beside a Basic header = %d; want 401", resp.StatusCode)
	}
	// Denied: a pairing token is never a credential, here either.
	pairing, err := c.auth.IssueToken(context.Background(), c.sam, auth.KindPairing, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ endpoint, path string }{{"stream", cradlePart}, {"cover", cradleBook}} {
		if resp := c.media(t, c.lib, m.endpoint, m.path, pairing, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("a pairing token on %s = %d; want 401", m.endpoint, resp.StatusCode)
		}
	}

	// The session still works as before, with the same-site resource policy.
	resp, _ = c.do(t, "GET", mediaURL(c.lib, "stream", cradlePart, c.samTok), "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cross-Origin-Resource-Policy") != "same-site" {
		t.Fatalf("session ?token= stream = %d, CORP %q", resp.StatusCode, resp.Header.Get("Cross-Origin-Resource-Policy"))
	}
}

// Security-critical: a folder book's folder can hold another book, and a cast
// token for the outer book never reaches the nested one (the grant's path rule
// alone would: castOwnBook asks the index which book a path is).
func TestCastTokenStaysOutOfNestedBooks(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	const (
		outer      = "A/Series"
		outerFile  = outer + "/00 - Prequel.m4b"
		nested     = outer + "/Book 2"
		nestedFile = nested + "/01 - Second.m4b"
	)
	root := t.TempDir()
	copyFixture(t, root, cradlePart, outerFile)
	copyFixture(t, root, cradlePart2, nestedFile)
	for _, dir := range []string{outer, nested} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(dir), "cover.jpg"), testImage(t, 64, 64, false), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Nested", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	c := &castEnv{testEnv: e, lib: lib.ID}
	c.sam, c.samTok = newUserWithSession(t, e, "sam", false)
	if err := e.cat.GrantWholeLibrary(ctx, c.sam, lib.ID); err != nil {
		t.Fatal(err)
	}
	// Both folders are books (the precondition this test is about).
	for _, p := range []string{outer, nested} {
		if resp, body := c.do(t, "GET", bookURL(lib.ID, "item", p), c.samTok, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("item %q = %d %s; want a book", p, resp.StatusCode, body)
		}
	}
	tok := c.castToken(t, outer)

	// Allowed: the outer book's own file and cover.
	for _, m := range []struct{ endpoint, path string }{{"stream", outerFile}, {"cover", outer}} {
		if resp := c.media(t, lib.ID, m.endpoint, m.path, tok, ""); resp.StatusCode != http.StatusOK {
			t.Errorf("outer %s = %d; want 200", m.endpoint, resp.StatusCode)
		}
	}
	// Denied: the nested book's file and cover, though both lie below the
	// grant's path; a file the index places in no book; the session still may.
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(outer), "stray.m4b"), []byte("not indexed"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ endpoint, path string }{{"stream", nestedFile}, {"cover", nested}, {"stream", outer + "/stray.m4b"}} {
		if resp := c.media(t, lib.ID, m.endpoint, m.path, tok, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %q with the outer book's cast token = %d; want 401", m.endpoint, m.path, resp.StatusCode)
		}
	}
	if resp := c.media(t, lib.ID, "stream", nestedFile, c.samTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("the session on the nested book = %d", resp.StatusCode)
	}
}

// A receiver's transcode (a cast token's ?transcode=1) marks the SENDER's
// session transcoded: handleStream notes it under Credential.DeviceID, the
// token that minted the cast token, whose progress saves make the session.
func TestCastTranscodeMarksTheSender(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	ctx := context.Background()
	tok := c.castToken(t, cradleBook)
	// The mark is noted before ffmpeg runs, so a missing binary (a 500) still shows it.
	c.api.ffmpeg = filepath.Join(t.TempDir(), "no-ffmpeg")
	if resp := c.media(t, c.lib, "stream", cradlePart, tok, "&transcode=1&t=30"); resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("cast transcode = %d; want it past auth and the semaphore", resp.StatusCode)
	}
	_, sender, err := c.auth.ResolveRequest(ctx, c.samTok, auth.Presence{}, auth.KindSession)
	if err != nil {
		t.Fatal(err)
	}
	_, cast, err := c.auth.ResolveCastRequest(ctx, tok, auth.Presence{}, c.lib, cradleBook, auth.KindCast)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if !c.api.streams.Transcoding(sender.ID, c.lib, cradleBook, now) {
		t.Fatal("the sender's session is not marked transcoded")
	}
	if c.api.streams.Transcoding(cast.ID, c.lib, cradleBook, now) {
		t.Fatal("the mark went to the cast token, not its sender")
	}
}

// A cast token stops working when its user is disabled (and works again when
// enabled), when the person loses the book, when it expires, when the device
// that minted it signs out and when the account goes.
func TestCastTokenRevocation(t *testing.T) {
	t.Parallel()
	c := newCastEnv(t)
	ctx := context.Background()
	tok := c.castToken(t, cradleBook)
	ok := func(want int, what string) {
		t.Helper()
		if resp := c.media(t, c.lib, "stream", cradlePart, tok, ""); resp.StatusCode != want {
			t.Fatalf("%s: stream = %d; want %d", what, resp.StatusCode, want)
		}
	}
	ok(http.StatusOK, "fresh")

	if err := c.auth.SetDisabled(ctx, c.sam, true); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusUnauthorized, "disabled user")
	if err := c.auth.SetDisabled(ctx, c.sam, false); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusOK, "re-enabled user")

	// The person's current scope decides, as for every request.
	if err := c.cat.RevokeShare(ctx, c.sam, c.samShare); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusForbidden, "revoked scope")
	if err := c.cat.GrantShare(ctx, c.sam, c.samShare); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusOK, "scope back")

	if _, err := c.db.ExecContext(ctx, `UPDATE tokens SET expires_at = '2020-01-01T00:00:00Z' WHERE kind = 'cast'`); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusUnauthorized, "expired")

	// Signing out the device that minted it ends it.
	tok = c.castToken(t, cradleBook)
	ok(http.StatusOK, "new token")
	if resp, body := c.do(t, "POST", "/api/v1/auth/logout", c.samTok, ""); resp.StatusCode >= 300 {
		t.Fatalf("logout = %d %s", resp.StatusCode, body)
	}
	ok(http.StatusUnauthorized, "sender signed out")

	// A deleted account's tokens go with it.
	tablet, err := c.auth.IssueToken(ctx, c.sam, auth.KindSession, "tablet", 0)
	if err != nil {
		t.Fatal(err)
	}
	c.samTok = tablet
	tok = c.castToken(t, cradleBook)
	ok(http.StatusOK, "token from another device")
	if _, err := c.auth.DeleteUser(ctx, c.sam, nil); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusUnauthorized, "deleted user")
}

func TestCastCapability(t *testing.T) {
	t.Parallel()
	if !serverCapabilities(t, newTestEnv(t))["cast"] {
		t.Fatal("cast capability is off")
	}
}
