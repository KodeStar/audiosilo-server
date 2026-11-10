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
	tooLong := func(resp *http.Response, raw string) bool {
		return resp.StatusCode == http.StatusBadRequest && strings.Contains(raw, "path is too long")
	}

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
		{"PUT", "enrichment", `{"asin":"B000000001"}`},
	} {
		u := fmt.Sprintf("/api/v1/admin/libraries/%d/%s%s", unknownLib, req.route, q(over))
		if resp, raw := s.do(t, req.method, u, tok, req.body); !tooLong(resp, raw) {
			t.Fatalf("%s %s over the cap on an unknown library = %d %s, want 400", req.method, req.route, resp.StatusCode, raw)
		}
	}
	for _, req := range []struct{ method, url, body string }{
		{"PUT", "/api/v1/admin/libraries/" + lib + "/folder-override", `{"mode":"book"}`},
		{"DELETE", "/api/v1/admin/libraries/" + lib + "/folder-override", ""},
		{"PUT", "/api/v1/admin/libraries/" + lib + "/enrichment", `{"asin":"B000000001"}`},
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
