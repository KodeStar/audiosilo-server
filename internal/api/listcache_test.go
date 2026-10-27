package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
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
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
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
	admin, _ := e.auth.IssueToken(ctx, e.adminID, auth.KindSession, "t", 0)
	user, _ := e.auth.CreateUser(ctx, "listener", "listener-password", auth.RoleUser)
	share, _ := e.cat.CreateShare(ctx, catalog.Share{Name: "B only"})
	if err := e.cat.AddSharePath(ctx, share.ID, catalog.PathRule{LibraryID: lib.ID, Path: "B"}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.GrantShare(ctx, user.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	listener, _ := e.auth.IssueToken(ctx, user.ID, auth.KindSession, "t", 0)
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
	if err := e.cat.RevokeShare(ctx, user.ID, share.ID); err != nil {
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
	tok, _ := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "t", 0)
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
}
