package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// server#291: the path cap (catalog.MaxPathBytes) comes first, whatever the
// library, and covers the routes that take a path outside pathInScope.
// Denied: one byte over is 400 "path is too long" on an unknown library (not
// its 404), on the admin's folder override, browse, a list's remove and a
// cast token, and a sync event's invalid however its library resolves.
// Allowed: a path at the cap gets each route's ordinary answer.
func TestPathCapComesFirst(t *testing.T) {
	t.Parallel()
	s := newSyncEnv(t)
	ctx := context.Background()
	tok, _ := s.auth.IssueToken(ctx, s.adminID, auth.KindSession, "phone", 0)
	atCap := "Long/" + strings.Repeat("a", catalog.MaxPathBytes-len("Long/"))
	over := atCap + "b"
	const unknownLib = 999999

	// An unknown library: over the cap is the 400, at it the library's 404.
	if resp, raw := s.do(t, "GET", bookURL(unknownLib, "item", over), tok, ""); !tooLong(resp, raw) {
		t.Fatalf("item over the cap on an unknown library = %d %s, want 400", resp.StatusCode, raw)
	}
	if resp, raw := s.do(t, "GET", bookURL(unknownLib, "item", atCap), tok, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("item at the cap on an unknown library = %d %s, want 404", resp.StatusCode, raw)
	}

	// A sync event over the cap is invalid even on a library the batch can't
	// resolve; one at the cap there is the library's not_found.
	code, a, raw := s.post(t, tok, syncBody("phone",
		listenJSON("over", unknownLib, over, 0, 60, "0"),
		listenJSON("cap", unknownLib, atCap, 0, 60, "0")))
	if code != http.StatusOK || len(a.Results) != 2 {
		t.Fatalf("sync = %d %s", code, raw)
	}
	if r := a.Results[0]; r.Status != "rejected" || r.Reason != "invalid" {
		t.Fatalf("over the cap on an unknown library: %+v, want rejected invalid", r)
	}
	if r := a.Results[1]; r.Status != "rejected" || r.Reason != "not_found" {
		t.Fatalf("at the cap on an unknown library: %+v, want rejected not_found", r)
	}

	lib := fmt.Sprint(s.lib.ID)
	q := func(p string) string { return "?path=" + url.QueryEscape(p) }
	// The admin's routes check the cap before the library too.
	for _, req := range []struct{ method, route, body string }{
		{"PUT", "folder-override", `{"mode":"book"}`},
		{"DELETE", "folder-override", ""},
	} {
		u := fmt.Sprintf("/api/v1/admin/libraries/%d/%s%s", unknownLib, req.route, q(over))
		if resp, raw := s.do(t, req.method, u, tok, req.body); !tooLong(resp, raw) {
			t.Fatalf("%s %s over the cap on an unknown library = %d %s, want 400", req.method, req.route, resp.StatusCode, raw)
		}
	}
	for _, req := range []struct{ method, url, body string }{
		{"PUT", "/api/v1/admin/libraries/" + lib + "/folder-override", `{"mode":"book"}`},
		{"DELETE", "/api/v1/admin/libraries/" + lib + "/folder-override", ""},
		{"GET", "/api/v1/libraries/" + lib + "/fs", ""},
		{"DELETE", "/api/v1/me/queue", ""},
	} {
		sep := q(over)
		if req.url == "/api/v1/me/queue" {
			sep = "?library_id=" + lib + "&path=" + url.QueryEscape(over)
		}
		if resp, raw := s.do(t, req.method, req.url+sep, tok, req.body); !tooLong(resp, raw) {
			t.Fatalf("%s %s over the cap = %d %s, want 400", req.method, req.url, resp.StatusCode, raw)
		}
		sep = q(atCap)
		if req.url == "/api/v1/me/queue" {
			sep = "?library_id=" + lib + "&path=" + url.QueryEscape(atCap)
		}
		if resp, raw := s.do(t, req.method, req.url+sep, tok, req.body); tooLong(resp, raw) || resp.StatusCode >= 500 {
			t.Fatalf("%s %s at the cap = %d %s, want its ordinary answer", req.method, req.url, resp.StatusCode, raw)
		}
	}
	body := func(p string) string { return fmt.Sprintf(`{"library_id":%d,"path":%q}`, s.lib.ID, p) }
	if resp, raw := s.do(t, "POST", "/api/v1/cast/token", tok, body(over)); !tooLong(resp, raw) {
		t.Fatalf("cast token over the cap = %d %s, want 400", resp.StatusCode, raw)
	}
	if resp, raw := s.do(t, "POST", "/api/v1/cast/token", tok, `{"library_id":999999,"path":"`+over+`"}`); !tooLong(resp, raw) {
		t.Fatalf("cast token over the cap on an unknown library = %d %s, want 400", resp.StatusCode, raw)
	}
	if resp, raw := s.do(t, "POST", "/api/v1/cast/token", tok, body(atCap)); tooLong(resp, raw) || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cast token at the cap = %d %s, want 404 (no book)", resp.StatusCode, raw)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM folder_overrides WHERE length(CAST(path AS BLOB)) > ?`,
		catalog.MaxPathBytes).Scan(&n); err != nil || n != 0 {
		t.Fatalf("folder overrides over the cap: %d, %v", n, err)
	}
}

// server#359: the cap holds where a path enters, on every route. A ?path= over
// it is 400 before any route or credential check reads it (capPathQuery): the
// cast token's book, a folder listing, a request with no token. Body paths
// over it are 400 on the routes that took them uncapped (a share's rules, the
// console's selections of books), nothing stored, and a list add's body is
// bounded. At the cap each route gives its ordinary answer.
func TestPathCapWhereThePathEnters(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	lib := newFixtureLibrary(t, e)
	tok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "console", 0)
	atCap := "Long/" + strings.Repeat("a", catalog.MaxPathBytes-len("Long/"))
	over := atCap + "b"
	q := url.QueryEscape

	// A cast token's request: authenticate cleans and resolves ?path= before
	// the route; over the cap it never gets there.
	resp, raw := e.do(t, "POST", "/api/v1/cast/token", tok, fmt.Sprintf(`{"library_id":%d,"path":%q}`, lib.ID, mistbornBook))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("cast token: %d %s", resp.StatusCode, raw)
	}
	var cast struct{ Token string }
	decodeInto(t, raw, &cast)
	for _, u := range []string{
		fmt.Sprintf("/api/v1/libraries/%d/stream?token=%s&path=%s", lib.ID, cast.Token, q(over)),
		fmt.Sprintf("/api/v1/libraries/%d/item?path=%s", lib.ID, q(over)), // no token at all
	} {
		if resp, raw := e.do(t, "GET", u, "", ""); !tooLong(resp, raw) {
			t.Fatalf("GET %.60s... over the cap = %d %s, want 400", u, resp.StatusCode, raw)
		}
	}
	if resp, raw := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/item?path=%s", lib.ID, q(atCap)), "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("item at the cap with no token = %d %s, want 401", resp.StatusCode, raw)
	}
	// A route that reads ?path= itself, outside pathInScope: the folder picker.
	if resp, raw := e.do(t, "GET", "/api/v1/admin/fs/dirs?path=/"+q(over), tok, ""); !tooLong(resp, raw) {
		t.Fatalf("folder picker over the cap = %d %s, want 400", resp.StatusCode, raw)
	}
	// A second ?path= is checked too.
	if resp, raw := e.do(t, "GET", fmt.Sprintf("/api/v1/libraries/%d/fs?path=%s&path=%s", lib.ID, q(mistbornBook), q(over)), tok, ""); !tooLong(resp, raw) {
		t.Fatalf("second ?path= over the cap = %d %s, want 400", resp.StatusCode, raw)
	}

	// A share's rules: on create, add and remove.
	resp, raw = e.do(t, "POST", "/api/v1/admin/shares", tok, `{"name":"Kids"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create share: %d %s", resp.StatusCode, raw)
	}
	var share struct{ ID int64 }
	decodeInto(t, raw, &share)
	ref := func(p string) string { return fmt.Sprintf(`{"library_id":%d,"path":%q}`, lib.ID, p) }
	paths := fmt.Sprintf("/api/v1/admin/shares/%d/paths", share.ID)
	for _, req := range []struct{ method, url, body string }{
		{"POST", "/api/v1/admin/shares", `{"name":"Long","paths":[` + ref(over) + `]}`},
		{"POST", paths, ref(over)},
		{"POST", paths, `{"rules":[` + ref(mistbornBook) + `,` + ref(over) + `]}`},
		{"DELETE", paths, ref(over)},
	} {
		if resp, raw := e.do(t, req.method, req.url, tok, req.body); !tooLong(resp, raw) {
			t.Fatalf("%s %s over the cap = %d %s, want 400", req.method, req.url, resp.StatusCode, raw)
		}
	}
	var n int
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM share_paths`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("share rules after the refusals: %d, %v; want none", n, err)
	}
	if resp, raw := e.do(t, "POST", paths, tok, ref(atCap)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("share rule at the cap = %d %s, want 204", resp.StatusCode, raw)
	}

	// The console's selections of books.
	for _, req := range []struct{ url, body string }{
		{"/api/v1/admin/books/bulk", `{"books":[` + ref(mistbornBook) + `,` + ref(over) + `],"set":{"genre":"Fantasy"}}`},
		{"/api/v1/admin/covers", `{"books":[` + ref(over) + `],"size":96}`},
		{"/api/v1/admin/issues/ignore", `{"kind":"no_cover","books":[` + ref(over) + `]}`},
	} {
		if resp, raw := e.do(t, "POST", req.url, tok, req.body); !tooLong(resp, raw) {
			t.Fatalf("POST %s over the cap = %d %s, want 400", req.url, resp.StatusCode, raw)
		}
		at := strings.ReplaceAll(req.body, over, atCap)
		if resp, raw := e.do(t, "POST", req.url, tok, at); tooLong(resp, raw) || resp.StatusCode >= 500 {
			t.Fatalf("POST %s at the cap = %d %s, want its ordinary answer", req.url, resp.StatusCode, raw)
		}
	}
	if err := e.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_ignores WHERE length(CAST(path AS BLOB)) > ?`,
		catalog.MaxPathBytes).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ignores over the cap: %d, %v", n, err)
	}

	// A list add's body is bounded: a path at the cap gets the route's answer
	// (no book there), a body padded past the bound is refused unread.
	if resp, raw := e.do(t, "POST", "/api/v1/me/queue", tok, ref(atCap)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("queue add at the cap = %d %s, want 404", resp.StatusCode, raw)
	}
	padded := fmt.Sprintf(`{"library_id":%d,"path":%q%s}`, lib.ID, mistbornBook, strings.Repeat(" ", pathBodyMax))
	if resp, raw := e.do(t, "POST", "/api/v1/me/queue", tok, padded); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("queue add past the body bound = %d %s, want 400", resp.StatusCode, raw)
	}
}

// tooLong reports whether a response is the path cap's 400.
func tooLong(resp *http.Response, raw string) bool {
	return resp.StatusCode == http.StatusBadRequest && strings.Contains(raw, msgPathTooLong)
}
