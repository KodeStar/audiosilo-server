package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kodestar/audiosilo-server/internal/auth"
	"github.com/kodestar/audiosilo-server/internal/catalog"
)

// listCacheLibrary indexes three books, two under "A" and one under "B", and
// returns the library.
func listCacheLibrary(t *testing.T, e *testEnv) *catalog.Library {
	t.Helper()
	ctx := context.Background()
	lib, err := e.cat.CreateLibrary(ctx, catalog.Library{Name: "Main", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"A/Alpha One.m4b", "A/Alpha Two.m4b", "B/Beta.m4b"} {
		title := strings.TrimSuffix(p[2:], ".m4b")
		b := &catalog.Book{LibraryID: lib.ID, RelPath: p, Title: title, Author: p[:1] + " Author",
			Format: "m4b", Duration: 60, Size: 1000, AddedAt: fmt.Sprintf("2025-01-0%dT00:00:00Z", i+1),
			Files: []catalog.BookFile{{RelPath: p, Duration: 60, Format: "m4b", Size: 1000}}}
		if _, err := e.cat.UpsertBook(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	return lib
}

func (e *testEnv) cacheEntries() int {
	e.api.lists.mu.Lock()
	defer e.api.lists.mu.Unlock()
	return len(e.api.lists.entries)
}

// TestListCacheServesUntilAWrite: a repeated list, recent shelf or search is
// answered byte for byte from the cache, and the first write after it (a
// re-index renaming a book) is in the next answer.
func TestListCacheServesUntilAWrite(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	lib := listCacheLibrary(t, e)
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		fmt.Sprintf("/api/v1/libraries/%d/books?sort=recent&limit=200", lib.ID),
		"/api/v1/books/recent?limit=48",
		"/api/v1/search?q=alpha&limit=50",
	}
	first := map[string]string{}
	for _, p := range paths {
		resp, body := e.do(t, "GET", p, tok, "")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Alpha One") {
			t.Fatalf("GET %s: %d %s", p, resp.StatusCode, body)
		}
		first[p] = body
	}
	if n := e.cacheEntries(); n != len(paths) {
		t.Fatalf("cache entries = %d, want %d", n, len(paths))
	}
	for _, p := range paths {
		resp, body := e.do(t, "GET", p, tok, "")
		if body != first[p] || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("GET %s again: %q (%s), want the first answer %q", p, body, resp.Header.Get("Content-Type"), first[p])
		}
	}
	// The cache's: with every entry's body swapped for a marker, the marker is
	// the answer.
	const marker = "{\"cached\":true}\n"
	e.api.lists.mu.Lock()
	for _, el := range e.api.lists.entries {
		el.Value.(*listEntry).body = []byte(marker)
	}
	e.api.lists.mu.Unlock()
	for _, p := range paths {
		resp, body := e.do(t, "GET", p, tok, "")
		if body != marker || resp.Header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("GET %s with the cache's bodies swapped: %q (%s), want %q", p, body, resp.Header.Get("Content-Type"), marker)
		}
	}

	// A write through the catalog: the next answer has it.
	if _, err := e.cat.UpsertBook(context.Background(), &catalog.Book{LibraryID: lib.ID, RelPath: "A/Alpha One.m4b",
		Title: "Alpha Renamed", Author: "A Author", Format: "m4b", Duration: 60, Size: 1000,
		Files: []catalog.BookFile{{RelPath: "A/Alpha One.m4b", Duration: 60, Format: "m4b", Size: 1000}}}); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		_, body := e.do(t, "GET", p, tok, "")
		if !strings.Contains(body, `"title":"Alpha Renamed"`) || strings.Contains(body, `"title":"Alpha One"`) {
			t.Fatalf("GET %s after the rename: %s", p, body)
		}
	}
}

// TestListCacheKeepsCallersApart: an answer cached for one caller is never
// another's: a listener granted only "B" sees only B's books on the routes an
// admin's answers are cached for, and the admin's stay whole.
func TestListCacheKeepsCallersApart(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	lib := listCacheLibrary(t, e)
	admin, listener, userID := adminAndMember(t, e)
	share, err := e.cat.CreateShare(ctx, catalog.Share{Name: "B only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, userID, share.ID); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		fmt.Sprintf("/api/v1/libraries/%d/books?limit=50", lib.ID),
		"/api/v1/books/recent?limit=48",
		"/api/v1/search?q=a&limit=50", // every title and author has a word starting "a"
	}
	for _, p := range paths {
		_, body := e.do(t, "GET", p, admin, "")
		if !strings.Contains(body, "Alpha One") || !strings.Contains(body, "Beta") {
			t.Fatalf("admin GET %s: %s", p, body)
		}
	}
	for range 2 { // the second time from the cache
		for _, p := range paths {
			_, body := e.do(t, "GET", p, listener, "")
			if strings.Contains(body, "Alpha") || !strings.Contains(body, "Beta") {
				t.Fatalf("listener GET %s: %s, want only B's book", p, body)
			}
		}
	}
	for _, p := range paths {
		if _, body := e.do(t, "GET", p, admin, ""); !strings.Contains(body, "Alpha One") {
			t.Fatalf("admin GET %s after the listener's: %s", p, body)
		}
	}

	// Revoking the share is a write: the listener's next answer is empty.
	if err := e.cat.RevokeShare(ctx, userID, share.ID); err != nil {
		t.Fatal(err)
	}
	_, body := e.do(t, "GET", "/api/v1/search?q=a&limit=50", listener, "")
	if strings.Contains(body, "Beta") {
		t.Fatalf("listener search after the revoke: %s", body)
	}
}

// TestListCacheKeyedOnQuery: answers to other queries of one route are other
// entries (a cursor, a sort, a limit).
func TestListCacheKeyedOnQuery(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	lib := listCacheLibrary(t, e)
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
	if err != nil {
		t.Fatal(err)
	}
	base := fmt.Sprintf("/api/v1/libraries/%d/books", lib.ID)
	var page struct {
		Books []struct {
			Title string `json:"title"`
		} `json:"books"`
		Next string `json:"next_cursor"`
	}
	_, body := e.do(t, "GET", base+"?sort=title&limit=2", tok, "")
	if err := json.Unmarshal([]byte(body), &page); err != nil || len(page.Books) != 2 || page.Next == "" {
		t.Fatalf("page 1: %s (%v)", body, err)
	}
	_, body = e.do(t, "GET", base+"?sort=title&limit=2&cursor="+url.QueryEscape(page.Next), tok, "")
	if !strings.Contains(body, "Beta") || strings.Contains(body, "Alpha One") {
		t.Fatalf("page 2: %s", body)
	}
	_, body = e.do(t, "GET", base+"?sort=recent&limit=1", tok, "")
	if !strings.Contains(body, "Beta") || strings.Contains(body, "Alpha") {
		t.Fatalf("recent, limit 1: %s", body)
	}
}

// TestListCacheGenerations: the cache moves forward only (a request that read
// an older generation neither reads nor clears), drops an older generation's
// entries, and holds at most listCacheBytes.
func TestListCacheGenerations(t *testing.T) {
	t.Parallel()
	c := newListCache()
	k := func(s string) [sha256.Size]byte { return sha256.Sum256([]byte(s)) }
	c.put(5, k("a"), []byte("five"))
	if _, ok := c.get(4, k("a")); ok {
		t.Fatal("an older generation read a newer entry")
	}
	c.put(4, k("b"), []byte("four"))
	if b, ok := c.get(5, k("a")); !ok || string(b) != "five" {
		t.Fatalf("generation 5's entry after an older request: %q, %v", b, ok)
	}
	if _, ok := c.get(5, k("b")); ok {
		t.Fatal("an older generation's answer was stored")
	}
	if _, ok := c.get(6, k("a")); ok {
		t.Fatal("a newer generation read an older entry")
	}
	if len(c.entries) != 0 || c.size != 0 {
		t.Fatalf("entries after moving on: %d (%d bytes)", len(c.entries), c.size)
	}

	big := make([]byte, listCacheMaxEntry)
	for i := range listCacheBytes/listCacheMaxEntry + 1 {
		c.put(6, k(fmt.Sprint(i)), big)
	}
	if c.size > listCacheBytes {
		t.Fatalf("size %d over the bound %d", c.size, listCacheBytes)
	}
	if _, ok := c.get(6, k("0")); ok {
		t.Fatal("the least recently used entry was kept past the bound")
	}
	c.put(6, k("huge"), make([]byte, listCacheMaxEntry+1))
	if _, ok := c.get(6, k("huge")); ok {
		t.Fatal("an answer over listCacheMaxEntry was kept")
	}

	// Tiny answers count their entry too, so a flood of them stays inside the
	// bound in entries as well as in bytes.
	for i := range listCacheBytes/listEntryCost + 1 {
		c.put(7, k(fmt.Sprint(i)), []byte("{}\n"))
	}
	if n := len(c.entries); c.size > listCacheBytes || n > listCacheBytes/listEntryCost {
		t.Fatalf("%d tiny entries, %d bytes: over the bound %d", n, c.size, listCacheBytes)
	}
}

// TestListCacheKeyExact: scopes that differ only in bytes that aren't UTF-8 (a
// path a scanner move wrote as the disk spells it) are other keys, as JSON would
// not have made them, and so are other paths and queries.
func TestListCacheKeyExact(t *testing.T) {
	t.Parallel()
	req := func(target string) *http.Request { return httptest.NewRequest("GET", target, nil) }
	in := func(p string) []catalog.Scope { return []catalog.Scope{{LibraryID: 1, Paths: []string{p}}} }
	recent := req("/api/v1/books/recent?limit=48")
	keys := map[[sha256.Size]byte]string{}
	for name, key := range map[string][sha256.Size]byte{
		"latin-1 e acute":   listCacheKey(recent, in("Caf\xe9")),
		"latin-1 e grave":   listCacheKey(recent, in("Caf\xe8")),
		"U+FFFD":            listCacheKey(recent, in("Caf�")),
		"whole library":     listCacheKey(recent, []catalog.Scope{{LibraryID: 1, AllowAll: true}}),
		"another limit":     listCacheKey(req("/api/v1/books/recent?limit=50"), in("Caf\xe9")),
		"another library's": listCacheKey(req("/api/v1/libraries/2/books?limit=48"), in("Caf\xe9")),
		"one library's":     listCacheKey(req("/api/v1/libraries/1/books?limit=48"), in("Caf\xe9")),
	} {
		if other, dup := keys[key]; dup {
			t.Fatalf("%s and %s share a key", name, other)
		}
		keys[key] = name
	}
	if listCacheKey(recent, in("Caf\xe9")) != listCacheKey(req("/api/v1/books/recent?limit=48"), in("Caf\xe9")) {
		t.Fatal("the same request and scopes made two keys")
	}
}
