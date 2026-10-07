package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// Fixtures the player redesign Phase 1b API tests share (lists, ratings,
// progress edits, your listening): the testdata library scanned, users with a
// session each, and the "Wight only" share.

// The fixture library's books: the Cradle folder holds two parts (one book), the
// Mistborn folder one file (also a book, the folder).
const (
	cradleBook   = "Will Wight/Cradle"
	cradlePart   = "Will Wight/Cradle/01 - Unsouled.m4b"
	mistbornBook = "Brandon Sanderson/Mistborn"
)

// bookURL is a path-addressed route of a library (endpoint "rating",
// "progress", "bookmarks", ...) at path.
func bookURL(libID int64, endpoint, path string) string {
	return "/api/v1/libraries/" + strconv.FormatInt(libID, 10) + "/" + endpoint + "?path=" + url.QueryEscape(path)
}

// newFixtureLibrary creates the "Main" library over testdata/library and scans it.
func newFixtureLibrary(t *testing.T, e *testEnv) *catalog.Library {
	t.Helper()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, err := e.cat.CreateLibrary(context.Background(), catalog.Library{Name: "Main", Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scanFixture(t, e, lib)
	return lib
}

// scanFixture scans lib with the env's own scanner (the server's).
func scanFixture(t *testing.T, e *testEnv, lib *catalog.Library) {
	t.Helper()
	if _, err := e.api.scanner.Scan(context.Background(), *lib); err != nil {
		t.Fatal(err)
	}
}

// newUserWithSession creates a listener (a demo account when demo) signed in on
// one device, and returns its id and session token.
func newUserWithSession(t *testing.T, e *testEnv, name string, demo bool) (int64, string) {
	t.Helper()
	ctx := context.Background()
	var u *auth.User
	var err error
	if demo {
		u, err = e.auth.CreateDemoUser(ctx, name)
	} else {
		u, err = e.auth.CreateUser(ctx, name, name+"-password", auth.RoleUser)
	}
	if err != nil {
		t.Fatal(err)
	}
	tok, err := e.auth.IssueToken(ctx, u.ID, auth.KindSession, name+"'s phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID, tok
}

// grantWightOnly makes the "Wight only" share (the library's "Will Wight" folder),
// grants it to userID and returns its id.
func grantWightOnly(t *testing.T, e *testEnv, libID, userID int64) int64 {
	t.Helper()
	ctx := context.Background()
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "Wight only",
		Paths: []catalog.PathRule{{LibraryID: libID, Path: "Will Wight"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, userID, share.ID); err != nil {
		t.Fatal(err)
	}
	return share.ID
}

// Every list the 1b routes answer is [] when it is empty, never null: the
// player relies on it (no null-normalising on the client).
func TestEmptyListsAreArrays(t *testing.T) {
	l := newListsEnv(t)
	body := func(method, path, tok, in string) string {
		t.Helper()
		resp, b := l.do(t, method, path, tok, in)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s = %d %s", method, path, resp.StatusCode, b)
		}
		return strings.TrimSpace(b)
	}
	exact := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s = %s, want %s", what, got, want)
		}
	}
	has := func(what, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Fatalf("%s = %s, want it to hold %s", what, got, w)
			}
		}
	}

	exact("GET /me/queue", body("GET", "/api/v1/me/queue", l.samTok, ""), `{"queue":[]}`)
	exact("PUT /me/queue (nothing listable)", body("PUT", "/api/v1/me/queue", l.samTok, l.itemsJSON("Nope")), `{"queue":[]}`)
	exact("GET /me/collections", body("GET", "/api/v1/me/collections", l.samTok, ""), `{"collections":[]}`)
	exact("GET /me/ratings", body("GET", "/api/v1/me/ratings", l.samTok, ""), `{"ratings":[]}`)

	// An owner's empty, unshared collection: items, preview and shared_with.
	col := l.createCollection(t, l.samTok, "Empty")
	has("GET /me/collections", body("GET", "/api/v1/me/collections", l.samTok, ""),
		`"shared_with":[]`, `"preview":[]`, `"item_count":0`)
	has("GET /me/collections/{id}", body("GET", colURL(col.ID, ""), l.samTok, ""),
		`"items":[]`, `"shared_with":[]`, `"preview":[]`)

	// A viewer whose access reaches none of the items sees them as empty too.
	shared := l.createCollection(t, l.oliveTok, "Not for kid")
	body("POST", colURL(shared.ID, "/items"), l.oliveTok, l.addJSON(mistbornBook, -1))
	body("PUT", colURL(shared.ID, "/shares"), l.oliveTok, fmt.Sprintf(`{"user_ids":[%d]}`, l.kid))
	has("a viewer's detail", body("GET", colURL(shared.ID, ""), l.kidTok, ""), `"items":[]`, `"preview":[]`)

	// Devices: the caller's own token is always one, so the list is never empty;
	// it is an array all the same.
	has("GET /me/devices", body("GET", "/api/v1/me/devices", l.samTok, ""), `{"devices":[{`)

	// Share targets on a server with no one else to share with.
	e := newTestEnv(t)
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, b := e.do(t, "GET", "/api/v1/me/share-targets", tok, "")
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(b) != `{"users":[]}` {
		t.Fatalf("GET /me/share-targets alone = %d %s, want {\"users\":[]}", resp.StatusCode, b)
	}
}
