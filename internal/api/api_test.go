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
	"github.com/kodestar/audiosilo-server/internal/store"
)

type testEnv struct {
	srv      *httptest.Server
	auth     *auth.Service
	cat      *catalog.Catalog
	adminID  int64
	authCode string
}

func newTestEnv(t *testing.T) *testEnv {
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
	scanner := library.NewScanner(cat, "", slog.Default())
	a := New(cfg, authSvc, cat, scanner, slog.Default())
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return &testEnv{srv: srv, auth: authSvc, cat: cat, adminID: admin.ID, authCode: code}
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
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root, Layout: config.LayoutBooksInFolder})
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
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root, Layout: config.LayoutBooksInFolder})
	// Index it so the hybrid view has book ids to attach.
	if _, err := library.NewScanner(e.cat, "", slog.Default()).Scan(ctx, *lib); err != nil {
		t.Fatal(err)
	}
	token, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)

	_, body := e.do(t, "GET", "/api/v1/libraries/"+strconv.FormatInt(lib.ID, 10)+"/fs?path=Will%20Wight/Cradle", token, "")
	var listing struct {
		Entries []struct {
			Name   string `json:"name"`
			IsBook bool   `json:"is_book"`
			Title  string `json:"title"`
			Author string `json:"author"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if len(listing.Entries) != 2 {
		t.Fatalf("expected 2 book files, got %d (%s)", len(listing.Entries), body)
	}
	// Every audio entry is annotated as a book with metadata; the client acts on
	// it by its path.
	for _, e := range listing.Entries {
		if !e.IsBook || e.Title == "" || e.Author != "Will Wight" {
			t.Fatalf("entry not annotated with book: %+v", e)
		}
	}
}

func TestItemResolvesOnDemand(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root, Layout: config.LayoutBooksInFolder})
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
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root, Layout: config.LayoutBooksInFolder})
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
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root, Layout: config.LayoutBooksInFolder})
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
