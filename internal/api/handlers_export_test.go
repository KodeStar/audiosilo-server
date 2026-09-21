package api

import (
	"context"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// TestExportLibrary covers the allowed+denied pair for
// GET /admin/libraries/{id}/export: an admin gets the importable envelope with
// download headers; a non-admin and an anonymous caller are refused; an unknown
// library 404s with the usual {error} envelope.
func TestExportLibrary(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	lib, _ := e.cat.CreateLibrary(ctx, catalog.Library{Name: "My Books", Root: t.TempDir()})
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{
		LibraryID: lib.ID, RelPath: "Lee Child/Die Trying", IsFolder: true,
		Title: "Die Trying", Author: "Lee Child", Narrator: "Dick Hill",
		Series: "Jack Reacher", SeriesIndex: 2, Duration: 36720, ASIN: "B0TESTASIN",
		Format: "mp3", Size: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	adminTok, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	path := "/api/v1/admin/libraries/" + strconv.FormatInt(lib.ID, 10) + "/export"

	// Allowed: the admin downloads the file.
	resp, body := e.do(t, "GET", path, adminTok, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export = %d %s, want 200", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	disp, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		t.Fatalf("Content-Disposition %q: %v", resp.Header.Get("Content-Disposition"), err)
	}
	if disp != "attachment" {
		t.Errorf("disposition = %q, want attachment", disp)
	}
	if !strings.HasPrefix(params["filename"], "audiosilo-my-books-") ||
		!strings.HasSuffix(params["filename"], ".json") {
		t.Errorf("filename = %q, want audiosilo-my-books-<date>.json", params["filename"])
	}

	// Round-trip the envelope: exactly the shape the metadata site imports.
	var out struct {
		Format        string `json:"format"`
		Version       int    `json:"version"`
		Source        string `json:"source"`
		ServerVersion string `json:"server_version"`
		Library       struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"library"`
		ExportedAt string `json:"exported_at"`
		Books      []struct {
			Title          string   `json:"title"`
			Authors        []string `json:"authors"`
			Narrators      []string `json:"narrators"`
			Series         string   `json:"series"`
			SeriesPosition string   `json:"series_position"`
			ASIN           string   `json:"asin"`
			RuntimeMin     int      `json:"runtime_min"`
		} `json:"books"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Format != "audiosilo-books" || out.Version != 1 || out.Source != "audiosilo-server" {
		t.Fatalf("envelope = %+v", out)
	}
	if out.ServerVersion != Version {
		t.Errorf("server_version = %q, want %q", out.ServerVersion, Version)
	}
	if out.Library.ID != lib.ID || out.Library.Name != "My Books" || out.ExportedAt == "" {
		t.Errorf("library/exported_at = %+v %q", out.Library, out.ExportedAt)
	}
	if len(out.Books) != 1 {
		t.Fatalf("books = %d, want 1: %s", len(out.Books), body)
	}
	b := out.Books[0]
	if b.Title != "Die Trying" || b.Series != "Jack Reacher" || b.SeriesPosition != "2" ||
		b.ASIN != "B0TESTASIN" || b.RuntimeMin != 612 {
		t.Errorf("book = %+v", b)
	}
	if len(b.Authors) != 1 || b.Authors[0] != "Lee Child" ||
		len(b.Narrators) != 1 || b.Narrators[0] != "Dick Hill" {
		t.Errorf("contributors = %v / %v", b.Authors, b.Narrators)
	}
	// Nothing about the filesystem leaves the server.
	for _, leak := range []string{"Lee Child/Die Trying", "rel_path", "4242", `"size"`} {
		if strings.Contains(body, leak) {
			t.Errorf("export leaks %q: %s", leak, body)
		}
	}

	// Denied: a non-admin session is refused by requireAdmin.
	member, _ := e.auth.CreateUser(ctx, "member", "member-password", auth.RoleUser)
	memberTok, _ := e.auth.IssueToken(ctx, member.ID, auth.KindSession, "t", 0)
	if resp, body := e.do(t, "GET", path, memberTok, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin export = %d %s, want 403", resp.StatusCode, body)
	}
	// Denied: no credential at all.
	if resp, body := e.do(t, "GET", path, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous export = %d %s, want 401", resp.StatusCode, body)
	}
	// Denied: a token in the query string is not accepted (export is not a media
	// route, so the ?token= fallback must not apply).
	if resp, _ := e.do(t, "GET", path+"?token="+adminTok, "", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("query-token export = %d, want 401", resp.StatusCode)
	}

	// Unknown library: the usual {error} envelope.
	resp, body = e.do(t, "GET", "/api/v1/admin/libraries/9999/export", adminTok, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown library = %d %s, want 404", resp.StatusCode, body)
	}
	var errEnv struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &errEnv); err != nil || errEnv.Error == "" {
		t.Errorf("404 body = %s, want an {error} envelope", body)
	}

	// A non-numeric id is a 400, not a 404.
	if resp, _ := e.do(t, "GET", "/api/v1/admin/libraries/abc/export", adminTok, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", resp.StatusCode)
	}
}

// TestServerInfoExportCapability checks the additive capability flag clients gate
// the Export affordance on.
func TestServerInfoExportCapability(t *testing.T) {
	e := newTestEnv(t)
	_, body := e.do(t, "GET", "/api/v1/server", "", "")
	var out struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if !out.Capabilities["export"] {
		t.Fatalf("capabilities.export = false, want true: %s", body)
	}
}
