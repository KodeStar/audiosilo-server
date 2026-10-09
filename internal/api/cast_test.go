package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// The sibling of the fixture's Cradle folder whose name shares its prefix.
const (
	cradleTwo   = "Will Wight/Cradle 2"
	cradleTwoF  = "Will Wight/Cradle 2/01 - Unsouled.m4b"
	cradlePart2 = "Will Wight/Cradle/02 - Soulsmith.m4b"
)

// castEnv: a copy of the fixture library plus a "Cradle 2" sibling (lib), a
// second library over the fixtures (other), sam allowed both (lib through a share
// the test can revoke), kid only the "Will Wight" folder of lib.
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
	fixtures, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	root := t.TempDir()
	for _, rel := range []string{cradlePart, cradlePart2, mistbornBook + "/01 - The Final Empire.m4b", cradleTwoF} {
		src := rel
		if rel == cradleTwoF {
			src = cradlePart
		}
		data, err := os.ReadFile(filepath.Join(fixtures, filepath.FromSlash(src)))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			t.Fatal(err)
		}
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
	u := "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/" + endpoint + "?path=" + url.QueryEscape(path) +
		"&token=" + url.QueryEscape(token) + extra
	resp, _ := c.do(t, "GET", u, "", "")
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
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if strings.Join(keys, ",") != "expires_at,token" {
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
	// such library 404, a bad body 400, anonymous 401.
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
	// Denied: an API key can't mint a credential that would outlive its revocation.
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
		if resp := c.media(t, c.lib, "cover", cradleBook, tok, extra); resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			t.Fatalf("cover%s = %d", extra, resp.StatusCode)
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
	// ... nor beside a header naming another credential.
	u := bookURL(c.lib, "stream", cradlePart) + "&token=" + url.QueryEscape(tok)
	if resp, _ := c.do(t, "GET", u, "not-a-token", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cast token beside a bad header = %d", resp.StatusCode)
	}

	// The session still works as before, with the same-site resource policy.
	resp, _ = c.do(t, "GET", bookURL(c.lib, "stream", cradlePart)+"&token="+url.QueryEscape(c.samTok), "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cross-Origin-Resource-Policy") != "same-site" {
		t.Fatalf("session ?token= stream = %d, CORP %q", resp.StatusCode, resp.Header.Get("Cross-Origin-Resource-Policy"))
	}
}

// A cast token stops working when its user is disabled (and works again when
// enabled), when the person loses the book, and when it expires.
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

	if _, err := c.db.ExecContext(ctx, `UPDATE cast_tokens SET expires_at = '2020-01-01T00:00:00.000Z'`); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusUnauthorized, "expired")

	// A deleted account's tokens go with it.
	tok = c.castToken(t, cradleBook)
	ok(http.StatusOK, "new token")
	if err := c.auth.DeleteUser(ctx, c.sam); err != nil {
		t.Fatal(err)
	}
	ok(http.StatusUnauthorized, "deleted user")
}

// The media rate limiter keys a cast token apart from the tokens table's ids.
func TestMediaBucket(t *testing.T) {
	t.Parallel()
	if a, b := mediaBucket(auth.Credential{ID: 5, Kind: auth.KindSession}), mediaBucket(auth.Credential{ID: 5, Kind: auth.KindCast}); a == b {
		t.Fatalf("session and cast id 5 share bucket %q", a)
	}
	if got := mediaBucket(auth.Credential{ID: 5, Kind: auth.KindAPI}); got != "5" {
		t.Fatalf("api bucket = %q, want the token id as before", got)
	}
}

func TestCastCapability(t *testing.T) {
	t.Parallel()
	if !serverCapabilities(t, newTestEnv(t))["cast"] {
		t.Fatal("cast capability is off")
	}
}
