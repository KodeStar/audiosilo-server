package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

const meBookPath = "Will Wight/Cradle/01 - Unsouled.m4b"

// meEnv builds an env with a scanned library and returns the library path prefix
// plus a granted (whole-library) non-admin session token.
func meEnv(t *testing.T) (e *testEnv, libPath, token string, libID int64) {
	t.Helper()
	e = newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	user, _ := e.auth.CreateUser(ctx, "reader", "reader-password", auth.RoleUser)
	if err := e.cat.GrantWholeLibrary(ctx, user.ID, lib.ID); err != nil {
		t.Fatal(err)
	}
	token, _ = e.auth.IssueToken(ctx, user.ID, auth.KindSession, "t", 0)
	return e, "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10), token, lib.ID
}

// TestBookmarksRoundTrip: a user adds, lists, then deletes a bookmark by id.
func TestBookmarksRoundTrip(t *testing.T) {
	e, libPath, token, _ := meEnv(t)
	bmURL := libPath + "/bookmarks?path=" + url.QueryEscape(meBookPath)

	resp, body := e.do(t, "POST", bmURL, token, `{"position":42.0,"note":"good bit"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add bookmark = %d %s, want 201", resp.StatusCode, body)
	}
	var added catalog.Bookmark
	if err := json.Unmarshal([]byte(body), &added); err != nil {
		t.Fatalf("decode add: %v (%s)", err, body)
	}
	if added.ID == 0 || added.Position != 42.0 || added.Note != "good bit" {
		t.Fatalf("unexpected added bookmark: %+v (%s)", added, body)
	}

	_, body = e.do(t, "GET", bmURL, token, "")
	var listed struct {
		Bookmarks []catalog.Bookmark `json:"bookmarks"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	if len(listed.Bookmarks) != 1 || listed.Bookmarks[0].ID != added.ID {
		t.Fatalf("bookmark not listed: %s", body)
	}

	del := "/api/v1/bookmarks/" + strconv.FormatInt(added.ID, 10)
	if resp, _ := e.do(t, "DELETE", del, token, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete bookmark = %d, want 204", resp.StatusCode)
	}
	if _, body := e.do(t, "GET", bmURL, token, ""); json.Valid([]byte(body)) {
		var after struct {
			Bookmarks []catalog.Bookmark `json:"bookmarks"`
		}
		json.Unmarshal([]byte(body), &after)
		if len(after.Bookmarks) != 0 {
			t.Fatalf("bookmark not removed: %s", body)
		}
	}
}

// TestNotesRoundTrip: a user adds, lists, then deletes a note by id.
func TestNotesRoundTrip(t *testing.T) {
	e, libPath, token, _ := meEnv(t)
	noteURL := libPath + "/notes?path=" + url.QueryEscape(meBookPath)

	resp, body := e.do(t, "POST", noteURL, token, `{"position":10.0,"body":"a thought"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add note = %d %s, want 201", resp.StatusCode, body)
	}
	var added catalog.Note
	if err := json.Unmarshal([]byte(body), &added); err != nil {
		t.Fatalf("decode add: %v (%s)", err, body)
	}
	if added.ID == 0 || added.Body != "a thought" {
		t.Fatalf("unexpected added note: %+v (%s)", added, body)
	}

	_, body = e.do(t, "GET", noteURL, token, "")
	var listed struct {
		Notes []catalog.Note `json:"notes"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	if len(listed.Notes) != 1 || listed.Notes[0].ID != added.ID {
		t.Fatalf("note not listed: %s", body)
	}

	del := "/api/v1/notes/" + strconv.FormatInt(added.ID, 10)
	if resp, _ := e.do(t, "DELETE", del, token, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete note = %d, want 204", resp.StatusCode)
	}
	_, body = e.do(t, "GET", noteURL, token, "")
	var after struct {
		Notes []catalog.Note `json:"notes"`
	}
	json.Unmarshal([]byte(body), &after)
	if len(after.Notes) != 0 {
		t.Fatalf("note not removed: %s", body)
	}
}

// TestHistoryRoundTrip: a user records a listening span and lists it back.
func TestHistoryRoundTrip(t *testing.T) {
	e, libPath, token, _ := meEnv(t)
	histURL := libPath + "/history?path=" + url.QueryEscape(meBookPath)

	if resp, body := e.do(t, "POST", histURL, token,
		`{"from_pos":0,"to_pos":30,"started_at":"2026-01-01T00:00:00Z","ended_at":"2026-01-01T00:00:30Z"}`); resp.StatusCode != http.StatusCreated {
		t.Fatalf("add history = %d %s, want 201", resp.StatusCode, body)
	}

	_, body := e.do(t, "GET", histURL, token, "")
	var listed struct {
		History []catalog.History `json:"history"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	if len(listed.History) != 1 {
		t.Fatalf("expected one history span, got %d (%s)", len(listed.History), body)
	}
	if listed.History[0].To != 30 {
		t.Fatalf("history span not round-tripped: %+v (%s)", listed.History[0], body)
	}

	// It also surfaces in the cross-book /me/history feed.
	if _, body := e.do(t, "GET", "/api/v1/me/history", token, ""); !json.Valid([]byte(body)) {
		t.Fatalf("me/history not JSON: %s", body)
	}
}

// TestBookmarkDeleteOwnershipScoped is the security-critical denied test: user B
// cannot delete user A's bookmark by id (DeleteBookmark scopes by `AND user_id =
// ?`), and the same holds for notes.
func TestBookmarkDeleteOwnershipScoped(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	root, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "library"))
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: root})
	libPath := "/api/v1/libraries/" + strconv.FormatInt(lib.ID, 10)

	// Two non-admin users, each granted the whole library.
	userA, _ := e.auth.CreateUser(ctx, "alice", "alice-password", auth.RoleUser)
	userB, _ := e.auth.CreateUser(ctx, "bob", "bob-password", auth.RoleUser)
	if err := e.cat.GrantWholeLibrary(ctx, userA.ID, lib.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantWholeLibrary(ctx, userB.ID, lib.ID); err != nil {
		t.Fatal(err)
	}
	tokA, _ := e.auth.IssueToken(ctx, userA.ID, auth.KindSession, "t", 0)
	tokB, _ := e.auth.IssueToken(ctx, userB.ID, auth.KindSession, "t", 0)

	// --- Bookmark ownership ---
	bmURL := libPath + "/bookmarks?path=" + url.QueryEscape(meBookPath)
	_, body := e.do(t, "POST", bmURL, tokA, `{"position":5,"note":"alice's"}`)
	var bm catalog.Bookmark
	if err := json.Unmarshal([]byte(body), &bm); err != nil || bm.ID == 0 {
		t.Fatalf("alice add bookmark failed: %v (%s)", err, body)
	}

	// B's delete by id is scoped to B's rows, so it affects 0 rows (handler still
	// returns 204 — the SQL is a no-op, not an error).
	delBM := "/api/v1/bookmarks/" + strconv.FormatInt(bm.ID, 10)
	e.do(t, "DELETE", delBM, tokB, "")

	// A's bookmark must still exist.
	if items, err := e.cat.ListBookmarks(ctx, userA.ID, catalog.Ref{LibraryID: lib.ID, Path: meBookPath}); err != nil || len(items) != 1 {
		t.Fatalf("B's cross-user delete removed A's bookmark: items=%d err=%v", len(items), err)
	}
	// A can delete their own.
	if resp, _ := e.do(t, "DELETE", delBM, tokA, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("owner delete = %d, want 204", resp.StatusCode)
	}
	if items, _ := e.cat.ListBookmarks(ctx, userA.ID, catalog.Ref{LibraryID: lib.ID, Path: meBookPath}); len(items) != 0 {
		t.Fatalf("owner delete did not remove the bookmark: %d", len(items))
	}

	// --- Note ownership (same idea) ---
	noteURL := libPath + "/notes?path=" + url.QueryEscape(meBookPath)
	_, body = e.do(t, "POST", noteURL, tokA, `{"position":5,"body":"alice's note"}`)
	var nt catalog.Note
	if err := json.Unmarshal([]byte(body), &nt); err != nil || nt.ID == 0 {
		t.Fatalf("alice add note failed: %v (%s)", err, body)
	}
	delNote := "/api/v1/notes/" + strconv.FormatInt(nt.ID, 10)
	e.do(t, "DELETE", delNote, tokB, "")
	if items, err := e.cat.ListNotes(ctx, userA.ID, catalog.Ref{LibraryID: lib.ID, Path: meBookPath}); err != nil || len(items) != 1 {
		t.Fatalf("B's cross-user delete removed A's note: items=%d err=%v", len(items), err)
	}
}
