package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
	"github.com/kodestar/audiosilo-server/internal/library"
	"github.com/kodestar/audiosilo-server/internal/media"
	"github.com/kodestar/audiosilo-server/internal/store"
)

type testEnv struct {
	srv      *httptest.Server
	api      *API
	auth     *auth.Service
	cat      *catalog.Catalog
	cfg      *config.Config
	adminID  int64
	authCode string
}

func newTestEnv(t *testing.T) *testEnv {
	return newTestEnvWith(t, nil)
}

// newTestEnvWith builds a test env, optionally mutating the config before the
// handler is constructed (needed for routes registered at build time, e.g. the
// demo root redirect).
func newTestEnvWith(t *testing.T, configure func(*config.Config)) *testEnv {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	authSvc := auth.New(db, time.Now)
	cat := catalog.New(db, time.Now)
	admin, _ := authSvc.CreateUser(ctx, "admin", "admin-password", auth.RoleAdmin)
	code, _ := authSvc.CreateAuthCode(ctx, admin.ID, "test", 0, 0)

	cfg := config.Default(t.TempDir())
	if configure != nil {
		configure(cfg)
	}
	scanner := library.NewScanner(cat, "", slog.Default())
	a := New(cfg, authSvc, cat, scanner, "", slog.Default())
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, api: a, auth: authSvc, cat: cat, cfg: cfg, adminID: admin.ID, authCode: code}
}

func (e *testEnv) do(t *testing.T, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestServerInfoPublic(t *testing.T) {
	e := newTestEnv(t)
	resp, body := e.do(t, "GET", "/api/v1/server", "", "")
	if resp.StatusCode != 200 || !strings.Contains(body, "AudioSilo") {
		t.Fatalf("server info: %d %s", resp.StatusCode, body)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	e := newTestEnv(t)
	if resp, _ := e.do(t, "GET", "/api/v1/libraries", "", ""); resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestRedeemExchangeFlow(t *testing.T) {
	e := newTestEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+e.authCode+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("redeem: %d %s", resp.StatusCode, body)
	}
	var redeem struct {
		PairingToken string `json:"pairing_token"`
		PNGDataURI   string `json:"qr_png_data_uri"`
	}
	json.Unmarshal([]byte(body), &redeem)
	if redeem.PairingToken == "" || !strings.HasPrefix(redeem.PNGDataURI, "data:image/png;base64,") {
		t.Fatalf("missing pairing token or QR: %s", body)
	}

	_, body = e.do(t, "POST", "/api/v1/auth/exchange", "",
		`{"pairing_token":"`+redeem.PairingToken+`","device_name":"phone"}`)
	var ex struct {
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(body), &ex)
	if ex.Token == "" {
		t.Fatalf("no session token: %s", body)
	}
	// The session token authenticates /me.
	if resp, _ := e.do(t, "GET", "/api/v1/me", ex.Token, ""); resp.StatusCode != 200 {
		t.Fatalf("me: %d", resp.StatusCode)
	}
	// The pairing token is single-use (revoked on exchange).
	if resp, _ := e.do(t, "POST", "/api/v1/auth/exchange", "",
		`{"pairing_token":"`+redeem.PairingToken+`"}`); resp.StatusCode != 401 {
		t.Fatalf("expected pairing token to be single-use, got %d", resp.StatusCode)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	token, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	path := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/fs?path=../../../etc"
	if resp, _ := e.do(t, "GET", path, token, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for traversal, got %d", resp.StatusCode)
	}
}

func TestBrowseFSAnnotatesIndexedBooks(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	// Index it so the hybrid view has book ids to attach.
	if _, err := library.NewScanner(e.cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	token, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	// A book is its folder (folder = one book), so browsing the series folder shows
	// the "Cradle" directory annotated as a book with metadata.
	_, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10)+"/fs?path=Will%20Wight", token, "")
	var listing struct {
		Entries []struct {
			Name   string `json:"name"`
			IsDir  bool   `json:"is_dir"`
			IsBook bool   `json:"is_book"`
			Title  string `json:"title"`
			Author string `json:"author"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	var cradle *struct {
		Name   string `json:"name"`
		IsDir  bool   `json:"is_dir"`
		IsBook bool   `json:"is_book"`
		Title  string `json:"title"`
		Author string `json:"author"`
	}
	for i := range listing.Entries {
		if listing.Entries[i].Name == "Cradle" {
			cradle = &listing.Entries[i]
		}
	}
	if cradle == nil {
		t.Fatalf("expected a Cradle entry (%s)", body)
	}
	if !cradle.IsDir || !cradle.IsBook || cradle.Title == "" || cradle.Author != "Will Wight" {
		t.Fatalf("Cradle folder not annotated as a book: %+v", *cradle)
	}
}

func TestItemResolvesOnDemand(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	token, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	// No scan has run. Requesting an item by path must index it on the fly.
	path := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/item?path=" +
		url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	resp, body := e.do(t, "GET", path, token, "")
	if resp.StatusCode != 200 {
		t.Fatalf("item: %d %s", resp.StatusCode, body)
	}
	var book struct {
		Title  string `json:"title"`
		Author string `json:"author"`
	}
	json.Unmarshal([]byte(body), &book)
	if book.Title != "Unsouled" || book.Author != "Will Wight" {
		t.Fatalf("unexpected item: %+v (%s)", book, body)
	}

	// A path with no book yields 404 (not a 500).
	bad := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10) + "/item?path=" + url.QueryEscape("Will Wight")
	if resp, _ := e.do(t, "GET", bad, token, ""); resp.StatusCode != 404 {
		t.Fatalf("expected 404 for non-book path, got %d", resp.StatusCode)
	}
}

func TestScopedShareAccess(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if _, err := library.NewScanner(e.cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	// A non-admin user granted only the "Will Wight" subtree.
	kid, _ := e.auth.CreateUser(ctx, "kid", "kid-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Wight only"})
	e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "Will Wight"})
	e.cat.GrantShare(ctx, kid.ID, share.ID)
	token, _ := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "t", 0)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)

	// Browsing the root shows only the granted branch.
	_, body := e.do(t, "GET", libPath+"/fs", token, "")
	if strings.Contains(body, "Brandon Sanderson") || !strings.Contains(body, "Will Wight") {
		t.Fatalf("filtered browse leaked or hid content: %s", body)
	}

	// Can play a book within the grant.
	in := libPath + "/stream?path=" + url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	if resp, _ := e.do(t, "GET", in, token, ""); resp.StatusCode != 200 {
		t.Fatalf("expected to stream a granted book, got %d", resp.StatusCode)
	}

	// Cannot touch content outside the grant.
	out := libPath + "/item?path=" + url.QueryEscape("Brandon Sanderson/Mistborn/01 - The Final Empire.m4b")
	if resp, _ := e.do(t, "GET", out, token, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 outside the grant, got %d", resp.StatusCode)
	}

	// Progress round-trips by path for a granted book.
	prog := libPath + "/progress?path=" + url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	if resp, _ := e.do(t, "PUT", prog, token, `{"position":12.5,"duration":120}`); resp.StatusCode != 200 {
		t.Fatalf("put progress: %d", resp.StatusCode)
	}
	_, body = e.do(t, "GET", prog, token, "")
	if !strings.Contains(body, "12.5") {
		t.Fatalf("progress did not round-trip: %s", body)
	}
}

func TestFavourites(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if _, err := library.NewScanner(e.cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}

	// A non-admin granted only the "Will Wight" subtree.
	kid, _ := e.auth.CreateUser(ctx, "kid", "kid-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Wight only"})
	e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "Will Wight"})
	e.cat.GrantShare(ctx, kid.ID, share.ID)
	token, _ := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "t", 0)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)

	// Favourite a folder within the grant (allowed).
	fav := libPath + "/favourites?path=" + url.QueryEscape("Will Wight/Cradle")
	if resp, b := e.do(t, "POST", fav, token, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("favourite a granted path: %d %s", resp.StatusCode, b)
	}
	// Idempotent: re-favouriting still succeeds.
	if resp, _ := e.do(t, "POST", fav, token, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("re-favourite should be idempotent, got %d", resp.StatusCode)
	}

	// It comes back in the cross-library list.
	_, body := e.do(t, "GET", "/api/v1/me/favourites", token, "")
	if !strings.Contains(body, "Will Wight/Cradle") {
		t.Fatalf("favourite not listed: %s", body)
	}

	// Denied: favouriting a path outside the grant is forbidden and never stored.
	out := libPath + "/favourites?path=" + url.QueryEscape("Brandon Sanderson/Mistborn")
	if resp, _ := e.do(t, "POST", out, token, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 favouriting outside the grant, got %d", resp.StatusCode)
	}
	if _, body := e.do(t, "GET", "/api/v1/me/favourites", token, ""); strings.Contains(body, "Brandon Sanderson") {
		t.Fatalf("forbidden favourite leaked into the list: %s", body)
	}

	// Remove round-trips by path.
	if resp, _ := e.do(t, "DELETE", fav, token, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete favourite: %d", resp.StatusCode)
	}
	if _, body := e.do(t, "GET", "/api/v1/me/favourites", token, ""); strings.Contains(body, "Will Wight/Cradle") {
		t.Fatalf("favourite not removed: %s", body)
	}
}

func TestCreateAuthCodeUsesAndExpiry(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	codePath := "/api/v1/admin/users/" + strconv.FormatInt(member.ID, 10) + "/authcode"

	mintCode := func(body string) string {
		t.Helper()
		resp, b := e.do(t, "POST", codePath, adminTok, body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create authcode: %d %s", resp.StatusCode, b)
		}
		var out struct {
			AuthCode string `json:"auth_code"`
		}
		json.Unmarshal([]byte(b), &out)
		if out.AuthCode == "" {
			t.Fatalf("no auth_code in response: %s", b)
		}
		return out.AuthCode
	}
	redeem := func(code string) int {
		t.Helper()
		resp, _ := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+code+`"}`)
		return resp.StatusCode
	}

	// Explicit 0 = unlimited uses / no expiry must pass through (not clobbered to
	// the bounded default), so the same code redeems repeatedly.
	unlimited := mintCode(`{"max_uses":0,"ttl_days":0}`)
	for i := 0; i < 3; i++ {
		if got := redeem(unlimited); got != http.StatusOK {
			t.Fatalf("unlimited code redeem #%d = %d, want 200", i+1, got)
		}
	}

	// Omitting the fields applies the bounded default (5 uses): the 6th redeem
	// fails because the code is used up.
	bounded := mintCode(`{}`)
	for i := 0; i < defaultAuthCodeMaxUses; i++ {
		if got := redeem(bounded); got != http.StatusOK {
			t.Fatalf("bounded code redeem #%d = %d, want 200", i+1, got)
		}
	}
	if got := redeem(bounded); got != http.StatusUnauthorized {
		t.Fatalf("redeem past the use limit = %d, want 401", got)
	}
}

// TestUpdateUserShortPasswordRejected covers the writeUserError mapping for the
// password-length rule: a too-short password on PATCH /admin/users/{id} must be a
// 400 (validation) carrying the reason, not a generic 500.
func TestUpdateUserShortPasswordRejected(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	path := "/api/v1/admin/users/" + strconv.FormatInt(member.ID, 10)

	resp, body := e.do(t, "PATCH", path, adminTok, `{"password":"short"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password update = %d, want 400 (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(body, "at least") {
		t.Fatalf("expected the password-length reason in the body, got %s", body)
	}

	// A sufficiently long password is accepted.
	if resp, body := e.do(t, "PATCH", path, adminTok, `{"password":"longenough"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("valid password update = %d, want 200 (%s)", resp.StatusCode, body)
	}
}

func TestLoginLockout(t *testing.T) {
	e := newTestEnv(t)
	// Exhaust the failure budget with wrong passwords.
	var last int
	for i := 0; i < 12; i++ {
		resp, _ := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"admin","password":"wrong"}`)
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected lockout (429) after repeated failures, got %d", last)
	}
}

// TestListProgressScopeFiltered covers review finding S4: GET /me/progress must
// not return durable state for paths the caller can no longer access (e.g. after
// a share is narrowed/revoked).
func TestListProgressScopeFiltered(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	if _, err := library.NewScanner(e.cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	kid, _ := e.auth.CreateUser(ctx, "kid", "kid-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "All"})
	e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: ""}) // whole library
	e.cat.GrantShare(ctx, kid.ID, share.ID)
	token, _ := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "t", 0)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)

	wight := url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	sanderson := url.QueryEscape("Brandon Sanderson/Mistborn/01 - The Final Empire.m4b")
	for _, p := range []string{wight, sanderson} {
		if resp, _ := e.do(t, "PUT", libPath+"/progress?path="+p, token, `{"position":5,"duration":100}`); resp.StatusCode != 200 {
			t.Fatalf("put progress %s: %d", p, resp.StatusCode)
		}
	}

	// While the whole-library grant stands, both are returned.
	_, body := e.do(t, "GET", "/api/v1/me/progress", token, "")
	if !strings.Contains(body, "Will Wight") || !strings.Contains(body, "Brandon Sanderson") {
		t.Fatalf("expected both books before narrowing: %s", body)
	}

	// Narrow the grant to the Will Wight subtree; the Sanderson progress must no
	// longer be returned.
	e.cat.RemoveSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: ""})
	e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "Will Wight"})
	_, body = e.do(t, "GET", "/api/v1/me/progress", token, "")
	if !strings.Contains(body, "Will Wight") {
		t.Fatalf("granted book should remain: %s", body)
	}
	if strings.Contains(body, "Brandon Sanderson") {
		t.Fatalf("progress for a revoked path leaked: %s", body)
	}
}

func TestStreamTranscode(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})

	// A non-admin granted only the "Will Wight" subtree.
	kid, _ := e.auth.CreateUser(ctx, "kid", "kid-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "Wight only"})
	e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "Will Wight"})
	e.cat.GrantShare(ctx, kid.ID, share.ID)
	token, _ := e.auth.IssueToken(ctx, kid.ID, auth.KindSession, "t", 0)
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)

	in := libPath + "/stream?transcode=1&path=" + url.QueryEscape("Will Wight/Cradle/01 - Unsouled.m4b")
	out := libPath + "/stream?transcode=1&path=" + url.QueryEscape("Brandon Sanderson/Mistborn/01 - The Final Empire.m4b")

	// Denied: an out-of-scope path is rejected before any transcoding.
	if resp, _ := e.do(t, "GET", out, token, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for out-of-scope transcode, got %d", resp.StatusCode)
	}

	// With ffmpeg disabled, an in-scope transcode is unavailable (503), not served.
	if resp, _ := e.do(t, "GET", in, token, ""); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when ffmpeg disabled, got %d", resp.StatusCode)
	}

	// Allowed: enable ffmpeg and the in-scope path transcodes to MP3.
	if !media.HasFFmpeg("ffmpeg") {
		t.Skip("ffmpeg not available; skipping the positive transcode assertion")
	}
	e.api.ffmpeg = "ffmpeg" // the handler reads this per-request
	resp, _ := e.do(t, "GET", in, token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 transcoding an in-scope book, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("transcode Content-Type = %q, want audio/mpeg", ct)
	}
}

func TestFolderOverrideEndpointRequiresAdmin(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	path := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10) + "/folder-override?path=Some/Folder"

	// A non-admin is rejected by requireAdmin.
	member, _ := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "PUT", path, memberTok, `{"mode":"book"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin folder-override = %d, want 403", resp.StatusCode)
	}

	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if resp, body := e.do(t, "PUT", path, adminTok, `{"mode":"book"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin set override = %d %s, want 200", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "PUT", path, adminTok, `{"mode":"weird"}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode = %d, want 400", resp.StatusCode)
	}
	if ovr, _ := e.cat.FolderOverrides(ctx, lib.ID); ovr["Some/Folder"] != catalog.OverrideBook {
		t.Fatalf("override not persisted: %+v", ovr)
	}
	if resp, _ := e.do(t, "DELETE", path, adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin clear override should be 200")
	}
	if ovr, _ := e.cat.FolderOverrides(ctx, lib.ID); len(ovr) != 0 {
		t.Fatalf("override not cleared: %+v", ovr)
	}
}

// meFlags fetches GET /me and returns the recovery/password capability flags the
// client uses to drive the sign-out recovery warning.
func (e *testEnv) meFlags(t *testing.T, token string) (hasPassword, hasRecovery bool) {
	t.Helper()
	resp, body := e.do(t, "GET", "/api/v1/me", token, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("me: %d %s", resp.StatusCode, body)
	}
	var u struct {
		HasPassword bool `json:"has_password"`
		HasRecovery bool `json:"has_recovery"`
	}
	json.Unmarshal([]byte(body), &u)
	return u.HasPassword, u.HasRecovery
}

// TestSelfServicePassword: a password-less player sets their own password (no
// admin, no current-password challenge) and can then password-login. The admin
// can't clear their own password through the same endpoint (denied path).
func TestSelfServicePassword(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser) // password-less
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)

	if hasPw, _ := e.meFlags(t, memberTok); hasPw {
		t.Fatal("password-less member should report has_password=false")
	}
	if resp, body := e.do(t, "POST", "/api/v1/auth/password", memberTok, `{"password":"longenough"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set own password = %d %s, want 204", resp.StatusCode, body)
	}
	if hasPw, _ := e.meFlags(t, memberTok); !hasPw {
		t.Fatal("member should report has_password=true after setting one")
	}
	// The new password is now a way back in via /auth/login.
	if resp, body := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"member","password":"longenough"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("login with self-set password = %d %s, want 200", resp.StatusCode, body)
	}

	// Denied: an admin may not clear their own password (would lock the console).
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	if resp, body := e.do(t, "POST", "/api/v1/auth/password", adminTok, `{"password":""}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("admin clearing own password = %d %s, want 400", resp.StatusCode, body)
	}
}

// TestSelfServiceRecovery: a player mints a durable recovery code, it redeems
// repeatedly through the normal connect flow, and clearing it removes the flag.
func TestSelfServiceRecovery(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)

	resp, body := e.do(t, "POST", "/api/v1/auth/recovery", memberTok, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("generate recovery = %d %s, want 201", resp.StatusCode, body)
	}
	var out struct {
		RecoveryCode string `json:"recovery_code"`
	}
	json.Unmarshal([]byte(body), &out)
	if out.RecoveryCode == "" {
		t.Fatalf("no recovery_code: %s", body)
	}
	if _, hasRec := e.meFlags(t, memberTok); !hasRec {
		t.Fatal("member should report has_recovery=true")
	}
	// Durable & reusable: redeems through the public connect flow more than once.
	for i := 0; i < 2; i++ {
		if resp, _ := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+out.RecoveryCode+`"}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("recovery redeem #%d failed", i+1)
		}
	}
	if resp, _ := e.do(t, "DELETE", "/api/v1/auth/recovery", memberTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear recovery should be 204")
	}
	if _, hasRec := e.meFlags(t, memberTok); hasRec {
		t.Fatal("member should report has_recovery=false after clearing")
	}
}

// TestRotateAndSupersedeInvite covers the admin "Resend" (rotate-in-place) and
// the one-active-invite-per-user supersede on mint.
func TestRotateAndSupersedeInvite(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	mintPath := "/api/v1/admin/users/" + strconv.FormatInt(member.ID, 10) + "/authcode"

	mint := func() string {
		t.Helper()
		resp, b := e.do(t, "POST", mintPath, adminTok, `{}`)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("mint invite: %d %s", resp.StatusCode, b)
		}
		var out struct {
			AuthCode string `json:"auth_code"`
		}
		json.Unmarshal([]byte(b), &out)
		return out.AuthCode
	}

	// Minting a second invite supersedes the first pending one (no pile-up).
	mint()
	second := mint()
	codes, _ := e.auth.ListAuthCodes(ctx, member.ID)
	if len(codes) != 1 {
		t.Fatalf("expected one pending invite after a second mint, got %d", len(codes))
	}

	// Rotate (Resend): old code dies, new one redeems.
	id := strconv.FormatInt(codes[0].ID, 10)
	resp, b := e.do(t, "POST", "/api/v1/admin/authcodes/"+id+"/rotate", adminTok, `{}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate: %d %s", resp.StatusCode, b)
	}
	var rotated struct {
		AuthCode string `json:"auth_code"`
	}
	json.Unmarshal([]byte(b), &rotated)
	if resp, _ := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+second+`"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("superseded/rotated old code = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/redeem", "", `{"code":"`+rotated.AuthCode+`"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("rotated new code should redeem (200)")
	}

	// A non-admin cannot rotate.
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "POST", "/api/v1/admin/authcodes/"+id+"/rotate", memberTok, `{}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin rotate = %d, want 403", resp.StatusCode)
	}
}

// TestAdminClearsRecovery: an admin can revoke a user's durable recovery code
// (the only lever to kill a leaked one); a non-admin cannot.
func TestAdminClearsRecovery(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	if _, err := e.auth.GenerateRecoveryCode(ctx, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, hasRec := e.meFlags(t, memberTok); !hasRec {
		t.Fatal("member should report has_recovery=true")
	}
	path := "/api/v1/admin/users/" + strconv.FormatInt(member.ID, 10) + "/recovery"
	// Denied: a non-admin cannot revoke a recovery code.
	if resp, _ := e.do(t, "DELETE", path, memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin clear recovery = %d, want 403", resp.StatusCode)
	}
	// Allowed: the admin revokes it.
	if resp, b := e.do(t, "DELETE", path, adminTok, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("admin clear recovery = %d %s, want 204", resp.StatusCode, b)
	}
	if _, hasRec := e.meFlags(t, memberTok); hasRec {
		t.Fatal("recovery should be gone after admin revoke")
	}
}

// TestPasswordChangeRequiresCurrent: a password-less player sets a first password
// with no challenge, but changing an existing password requires the correct
// current one — so a session bearer alone can't silently replace a known password.
func TestPasswordChangeRequiresCurrent(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	member, _ := e.auth.CreateUser(ctx, "member", "", auth.RoleUser) // password-less
	tok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)

	if resp, b := e.do(t, "POST", "/api/v1/auth/password", tok, `{"password":"firstpass1"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first set = %d %s, want 204", resp.StatusCode, b)
	}
	// Empty password is rejected (self-clear would risk lockout).
	if resp, _ := e.do(t, "POST", "/api/v1/auth/password", tok, `{"password":""}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty password = %d, want 400", resp.StatusCode)
	}
	// Change without / with the wrong current password is rejected.
	if resp, _ := e.do(t, "POST", "/api/v1/auth/password", tok, `{"password":"secondpass1"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("change without current = %d, want 401", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/password", tok, `{"password":"secondpass1","current_password":"nope"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("change with wrong current = %d, want 401", resp.StatusCode)
	}
	// Correct current password lets the change through; the new password logs in.
	if resp, b := e.do(t, "POST", "/api/v1/auth/password", tok, `{"password":"secondpass1","current_password":"firstpass1"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("change with correct current = %d %s, want 204", resp.StatusCode, b)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"member","password":"secondpass1"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("login with new password = %d, want 200", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/login", "", `{"username":"member","password":"firstpass1"}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password still works = %d, want 401", resp.StatusCode)
	}
}

// TestShareAdminHandlers covers the share-admin transport layer (only the
// catalog layer was tested before): create with its path loop *and* the
// orphan-cleanup rollback, in-place update, validation, not-found, and the admin
// gate (denied for non-admins).
func TestShareAdminHandlers(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	// Create a share with an initial path rule; the response carries the rule
	// (exercises handleCreateShare's AddSharePath loop on the happy path).
	create := `{"name":"Wight","paths":[{"library_id":` + strconv.FormatInt(lib.ID, 10) + `,"path":"Will Wight"}]}`
	resp, b := e.do(t, "POST", "/api/v1/admin/shares", adminTok, create)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create share = %d %s, want 201", resp.StatusCode, b)
	}
	var created catalog.Share
	json.Unmarshal([]byte(b), &created)
	if created.ID == 0 || len(created.Paths) != 1 || created.Paths[0].Path != "Will Wight" {
		t.Fatalf("created share missing its path rule: %s", b)
	}

	// A nameless share is a 400.
	if resp, _ := e.do(t, "POST", "/api/v1/admin/shares", adminTok, `{"paths":[]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("nameless share = %d, want 400", resp.StatusCode)
	}

	// Rollback: a rule referencing a non-existent library fails the FK insert, so
	// the just-created share row must be deleted, not left orphaned.
	bad := `{"name":"Orphan","paths":[{"library_id":999999,"path":"x"}]}`
	if resp, _ := e.do(t, "POST", "/api/v1/admin/shares", adminTok, bad); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("share with bad path rule = %d, want 500", resp.StatusCode)
	}
	if _, list := e.do(t, "GET", "/api/v1/admin/shares", adminTok, ""); strings.Contains(list, "Orphan") {
		t.Fatalf("rolled-back share leaked into the list: %s", list)
	}

	// Update renames in place (200) and the change is visible on GET.
	idPath := "/api/v1/admin/shares/" + strconv.FormatInt(created.ID, 10)
	if resp, b := e.do(t, "PATCH", idPath, adminTok, `{"name":"Renamed"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("update share = %d %s, want 200", resp.StatusCode, b)
	}
	if _, b := e.do(t, "GET", idPath, adminTok, ""); !strings.Contains(b, "Renamed") {
		t.Fatalf("rename not reflected: %s", b)
	}

	// Updating a non-existent share is a 404.
	if resp, _ := e.do(t, "PATCH", "/api/v1/admin/shares/999999", adminTok, `{"name":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("update missing share = %d, want 404", resp.StatusCode)
	}

	// Denied: a non-admin can neither create nor update shares.
	member, _ := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "POST", "/api/v1/admin/shares", memberTok, `{"name":"x"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin create share = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "PATCH", idPath, memberTok, `{"name":"x"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin update share = %d, want 403", resp.StatusCode)
	}
}

// TestDemoCannotSelfRecover: a throwaway demo session may not mint a durable
// recovery code or set a password (either would outlive the idle reaper).
func TestDemoCannotSelfRecover(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	demo, err := e.auth.CreateDemoUser(ctx, auth.DemoUsernamePrefix+"abc")
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := e.auth.IssueToken(ctx, demo.ID, auth.KindSession, "t", 0)
	if resp, _ := e.do(t, "POST", "/api/v1/auth/recovery", tok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo recovery = %d, want 403", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/auth/password", tok, `{"password":"longenough"}`); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("demo set-password = %d, want 403", resp.StatusCode)
	}
}
