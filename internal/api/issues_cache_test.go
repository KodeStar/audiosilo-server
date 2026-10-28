package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/catalog"
	"github.com/kodestar/audiosilo-server/internal/config"
)

// TestIssuesSummaryCachedUntilAWrite (server#245): the Health summary and the
// admin book lists are answered from the list cache while nothing is written,
// and a fix (here an ignore, then a re-index) is in the very next answer. A
// member is still refused, cached answer or not.
func TestIssuesSummaryCachedUntilAWrite(t *testing.T) {
	t.Parallel()
	e := newTestEnvWith(t, func(c *config.Config) { c.Metadata.Enabled = false })
	adminTok, memberTok, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	ctx := context.Background()
	lib, _ := e.cat.GetLibrary(ctx, libID)
	if err := os.WriteFile(filepath.Join(lib.Root, "x.m4b"), []byte("x"), 0o644); err != nil {
		t.Fatal(err) // online: an empty root with books indexed reads as unmounted
	}
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: libID, RelPath: "AC3/Book", Title: "AC3", Codec: "ac3"}); err != nil {
		t.Fatal(err)
	}
	count := func(kind string) (open, ignored int) {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/admin/issues", adminTok, "")
		var got issuesResp
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("issues = %d %s", resp.StatusCode, body)
		}
		for _, c := range got.Categories {
			if c.Kind == kind {
				return c.Count, c.Ignored
			}
		}
		t.Fatalf("no %s category in %s", kind, body)
		return 0, 0
	}
	const list = "/api/v1/admin/books?issue=transcode&sort=added&order=desc&limit=50"
	if n, _ := count(catalog.IssueTranscode); n != 1 {
		t.Fatalf("transcode = %d, want 1", n)
	}
	if _, body := e.do(t, "GET", list, adminTok, ""); !strings.Contains(body, `"path":"AC3/Book"`) {
		t.Fatalf("transcode list = %s", body)
	}
	if n := e.cacheEntries(); n != 2 {
		t.Fatalf("cache entries = %d, want 2 (the summary and the list)", n)
	}

	// Repeated: the cache answers both, and still refuses a member (whose
	// first request writes its token's last_seen, so it goes last).
	const marker = "{\"cached\":true}\n"
	e.cachedBodies(marker)
	paths := []string{"/api/v1/admin/issues", list}
	for _, p := range paths {
		resp, body := e.do(t, "GET", p, adminTok, "")
		if body != marker || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("GET %s again = %q, want the cached answer", p, body)
		}
	}
	for _, p := range paths {
		if resp, body := e.do(t, "GET", p, memberTok, ""); resp.StatusCode != http.StatusForbidden || body == marker {
			t.Fatalf("member GET %s = %d %q, want 403", p, resp.StatusCode, body)
		}
	}

	// Ignoring the book is a write: the next summary and list have it.
	ignore := `{"kind":"transcode","books":[{"library_id":` + strconv.FormatInt(libID, 10) + `,"path":"AC3/Book"}]}`
	if resp, body := e.do(t, "POST", "/api/v1/admin/issues/ignore", adminTok, ignore); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ignore = %d %s", resp.StatusCode, body)
	}
	if n, ig := count(catalog.IssueTranscode); n != 0 || ig != 1 {
		t.Fatalf("after the ignore: transcode = %d (ignored %d), want 0 (1)", n, ig)
	}
	if _, body := e.do(t, "GET", list, adminTok, ""); body == marker || strings.Contains(body, "AC3/Book") {
		t.Fatalf("after the ignore the list = %s", body)
	}

	// A fix through a re-index (the codec now plays): gone from the count at once.
	if resp, _ := e.do(t, "DELETE", "/api/v1/admin/issues/ignore", adminTok, ignore); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unignore = %d", resp.StatusCode)
	}
	if n, _ := count(catalog.IssueTranscode); n != 1 {
		t.Fatalf("after the unignore: transcode = %d, want 1", n)
	}
	e.cachedBodies(marker)
	if _, err := e.cat.UpsertBook(ctx, &catalog.Book{LibraryID: libID, RelPath: "AC3/Book", Title: "AC3", Codec: "aac"}); err != nil {
		t.Fatal(err)
	}
	if n, ig := count(catalog.IssueTranscode); n != 0 || ig != 0 {
		t.Fatalf("after the fix: transcode = %d (ignored %d), want 0", n, ig)
	}
}

// TestIssuesCacheKey: what the summary is computed from besides the database
// is in its key, so a library going offline (a probe, no write) or community
// metadata turning off is never answered from an older summary.
func TestIssuesCacheKey(t *testing.T) {
	t.Parallel()
	req, _ := http.NewRequest("GET", "/api/v1/admin/issues", nil)
	all := catalog.IssueKinds
	without := []string{catalog.IssueScanError, catalog.IssueTranscode}
	nas := []offlineLibrary{{LibraryID: 2, Name: "NAS", Root: "/mnt/nas", Books: 4}}
	keys := map[[32]byte]string{}
	for name, k := range map[string][32]byte{
		"online":      listCacheKey(req, nil, all, []offlineLibrary{}),
		"offline":     listCacheKey(req, nil, all, nas),
		"more books":  listCacheKey(req, nil, all, []offlineLibrary{{LibraryID: 2, Name: "NAS", Root: "/mnt/nas", Books: 5}}),
		"fewer kinds": listCacheKey(req, nil, without, []offlineLibrary{}),
		"a list":      listCacheKey(req, nil),
	} {
		if other, dup := keys[k]; dup {
			t.Fatalf("%s and %s share a key", name, other)
		}
		keys[k] = name
	}
	if listCacheKey(req, nil, all, nas) != listCacheKey(req, nil, slices.Clone(all), slices.Clone(nas)) {
		t.Fatal("the same inputs give different keys")
	}
}

// TestAdminFacetsCachedUntilAWrite: Library > Books' facet counts are kept in
// the list cache too, and a book indexed after them is counted at once; of its
// pages only the first is kept.
func TestAdminFacetsCachedUntilAWrite(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	adminTok, _, _ := adminAndMember(t, e)
	libID, _ := seedCatalog(t, e)
	total := func() int {
		t.Helper()
		resp, body := e.do(t, "GET", "/api/v1/admin/books/facets", adminTok, "")
		var got catalog.BookFacets
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("facets = %d %s", resp.StatusCode, body)
		}
		return got.Total
	}
	if n := total(); n != 2 {
		t.Fatalf("total = %d, want 2", n)
	}
	if n := e.cacheEntries(); n != 1 {
		t.Fatalf("cache entries = %d, want 1", n)
	}
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: libID, RelPath: "New/Book", Title: "New"}); err != nil {
		t.Fatal(err)
	}
	if n := total(); n != 3 {
		t.Fatalf("total after a new book = %d, want 3", n)
	}

	// A first page is kept; a later (cursor) page isn't, so one admin
	// scrolling every page can't push the player's lists out.
	_, body := e.do(t, "GET", "/api/v1/admin/books?limit=1", adminTok, "")
	var page catalog.AdminPage
	if err := json.Unmarshal([]byte(body), &page); err != nil || page.NextCursor == "" {
		t.Fatalf("first page = %s", body)
	}
	before := e.cacheEntries()
	if resp, body := e.do(t, "GET", "/api/v1/admin/books?limit=1&cursor="+page.NextCursor, adminTok, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("second page = %d %s", resp.StatusCode, body)
	}
	if n := e.cacheEntries(); n != before {
		t.Fatalf("cache entries after a cursor page = %d, want %d", n, before)
	}
}
