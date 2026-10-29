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

// contentGenEnv is a library of three books, the admin's session, and the
// cached routes warmed with every entry's body swapped for a marker.
type contentGenEnv struct {
	*testEnv
	lib   *catalog.Library
	tok   string
	paths []string
}

const contentGenMarker = "{\"cached\":true}\n"

func newContentGenEnv(t *testing.T) *contentGenEnv {
	t.Helper()
	e := newTestEnv(t)
	lib := listCacheLibrary(t, e)
	tok, err := e.auth.IssueToken(context.Background(), e.adminID, auth.KindSession, "phone", 0)
	if err != nil {
		t.Fatal(err)
	}
	return &contentGenEnv{testEnv: e, lib: lib, tok: tok, paths: []string{
		fmt.Sprintf("/api/v1/libraries/%d/books?limit=50", lib.ID),
		fmt.Sprintf("/api/v1/libraries/%d/authors", lib.ID),
		"/api/v1/books/recent?limit=48",
		"/api/v1/search?q=alpha&limit=50",
		"/api/v1/admin/books",
		"/api/v1/admin/books/facets",
		"/api/v1/admin/issues",
	}}
}

// warm requests every cached route and swaps the cached bodies for the marker.
func (c *contentGenEnv) warm(t *testing.T) {
	t.Helper()
	for _, p := range c.paths {
		if resp, body := c.do(t, "GET", p, c.tok, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", p, resp.StatusCode, body)
		}
	}
	if n := c.cacheEntries(); n != len(c.paths) {
		t.Fatalf("cache entries = %d, want %d", n, len(c.paths))
	}
	c.cachedBodies(contentGenMarker)
}

// answeredFromCache reports, per route, whether the cache answered it.
func (c *contentGenEnv) answeredFromCache(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range c.paths {
		_, body := c.do(t, "GET", p, c.tok, "")
		out[p] = body == contentGenMarker
	}
	return out
}

func (c *contentGenEnv) must(t *testing.T, method, path, body string) {
	t.Helper()
	resp, out := c.do(t, method, path, c.tok, body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, out)
	}
}

// TestListeningKeepsTheListCacheWarm (server#258): progress saves (with their
// listening sessions), bookmarks, history, ratings, the queue and the token's
// own last-seen touch between two list requests leave every cached answer in
// place: the second request is a hit, and the content generation never moved.
func TestListeningKeepsTheListCacheWarm(t *testing.T) {
	t.Parallel()
	c := newContentGenEnv(t)
	at := func(route, path string) string {
		return fmt.Sprintf("/api/v1/libraries/%d/%s?path=%s", c.lib.ID, route, url.QueryEscape(path))
	}
	// The book's first save makes its progress row: content (Health's
	// duplicates count each copy's listeners), so it is made before the warm-up.
	c.must(t, "PUT", at("progress", "A/Alpha One.m4b"), `{"position":1,"duration":60}`)
	c.warm(t)
	gen := c.db.ContentGeneration()

	for i := 2; i < 6; i++ {
		c.must(t, "PUT", at("progress", "A/Alpha One.m4b"), fmt.Sprintf(`{"position":%d,"duration":60}`, i))
	}
	c.must(t, "POST", at("bookmarks", "A/Alpha One.m4b"), `{"position":3,"note":"here"}`)
	c.must(t, "POST", at("history", "A/Alpha One.m4b"), `{"from_pos":1,"to_pos":5}`)
	c.must(t, "PUT", at("rating", "A/Alpha One.m4b"), `{"rating":4}`)
	c.must(t, "POST", "/api/v1/me/queue", fmt.Sprintf(`{"library_id":%d,"path":"B/Beta.m4b"}`, c.lib.ID))
	// A request from a new app touches the token (its app and address).
	resp, _ := c.doHeaders(t, "GET", "/api/v1/me/progress", c.tok, "", map[string]string{"X-AudioSilo-Client": "web/9.9.9 (web)"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /me/progress from a new app: %d", resp.StatusCode)
	}

	if got := c.db.ContentGeneration(); got != gen {
		t.Errorf("listening moved the content generation %d -> %d", gen, got)
	}
	for p, hit := range c.answeredFromCache(t) {
		if !hit {
			t.Errorf("GET %s after listening was not answered from the cache", p)
		}
	}
	// Listening still shows where it is per user (uncached): the last save won.
	if _, body := c.do(t, "GET", at("progress", "A/Alpha One.m4b"), c.tok, ""); !strings.Contains(body, `"position":5`) {
		t.Fatalf("progress after the saves: %s", body)
	}
}

// TestContentWritesEmptyTheListCache (server#258): each content write between
// two list requests (a re-index renaming a book, a book's edit, a first
// progress row, a grant, a collection) is a miss whose answer is fresh.
func TestContentWritesEmptyTheListCache(t *testing.T) {
	t.Parallel()
	c := newContentGenEnv(t)
	ctx := context.Background()
	member, err := c.auth.CreateUser(ctx, "sam", "sam-password", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	share, err := c.cat.CreateShare(ctx, catalog.Share{Name: "B"})
	if err != nil {
		t.Fatal(err)
	}
	writes := []struct {
		name  string
		write func() error
		fresh string // in the next answer to the first route
	}{
		{"re-index", func() error {
			_, err := c.cat.UpsertBook(ctx, &catalog.Book{LibraryID: c.lib.ID, RelPath: "A/Alpha One.m4b",
				Title: "Alpha Renamed", Author: "A Author", Format: "m4b", Duration: 60, Size: 1000,
				Files: []catalog.BookFile{{RelPath: "A/Alpha One.m4b", Duration: 60, Format: "m4b", Size: 1000}}})
			return err
		}, `"title":"Alpha Renamed"`},
		{"book edit", func() error {
			return c.cat.EditBook(ctx, c.lib.ID, "B/Beta.m4b", catalog.BookEdit{Set: map[string]string{"title": "Beta Edited"}})
		}, `"title":"Beta Edited"`},
		{"first progress row", func() error {
			resp, _ := c.do(t, "PUT", fmt.Sprintf("/api/v1/libraries/%d/progress?path=%s", c.lib.ID, url.QueryEscape("B/Beta.m4b")),
				c.tok, `{"position":1,"duration":60}`)
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("progress PUT %d", resp.StatusCode)
			}
			return nil
		}, `"books"`},
		{"grant", func() error { return c.cat.GrantShare(ctx, member.ID, share.ID) }, `"books"`},
		{"collection", func() error {
			_, err := c.cat.CreateCollection(ctx, member.ID, "Mine", "")
			return err
		}, `"books"`},
	}
	for _, w := range writes {
		c.warm(t)
		if err := w.write(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		for p, hit := range c.answeredFromCache(t) {
			if hit {
				t.Errorf("GET %s after the %s was answered from the cache", p, w.name)
			}
		}
		if _, body := c.do(t, "GET", c.paths[0], c.tok, ""); !strings.Contains(body, w.fresh) {
			t.Errorf("GET %s after the %s: %s, want %s", c.paths[0], w.name, body, w.fresh)
		}
	}
}

// TestGrantChangeReachesAWarmCache (server#258): a listener saving progress
// keeps their list cached, and removing their grant still empties it at once
// (denied), as granting it again shows the books again (allowed).
func TestGrantChangeReachesAWarmCache(t *testing.T) {
	t.Parallel()
	e := newTestEnv(t)
	ctx := context.Background()
	lib := listCacheLibrary(t, e)
	_, listener, userID := adminAndMember(t, e)
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
	list := fmt.Sprintf("/api/v1/libraries/%d/books?limit=50", lib.ID)
	save := func(pos int) {
		t.Helper()
		resp, body := e.do(t, "PUT", fmt.Sprintf("/api/v1/libraries/%d/progress?path=%s", lib.ID, url.QueryEscape("B/Beta.m4b")),
			listener, fmt.Sprintf(`{"position":%d,"duration":60}`, pos))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("progress PUT: %d %s", resp.StatusCode, body)
		}
	}
	save(1)
	if _, body := e.do(t, "GET", list, listener, ""); !strings.Contains(body, "Beta") {
		t.Fatalf("listener's list: %s", body)
	}
	e.cachedBodies(contentGenMarker)
	save(2)
	if _, body := e.do(t, "GET", list, listener, ""); body != contentGenMarker {
		t.Fatalf("listener's list after a save: %q, want the cached answer", body)
	}

	if err := e.cat.RevokeShare(ctx, userID, share.ID); err != nil {
		t.Fatal(err)
	}
	if resp, body := e.do(t, "GET", list, listener, ""); body == contentGenMarker || strings.Contains(body, "Beta") {
		t.Fatalf("listener's list after the revoke: %d %s", resp.StatusCode, body)
	}
	if err := e.cat.GrantShare(ctx, userID, share.ID); err != nil {
		t.Fatal(err)
	}
	if _, body := e.do(t, "GET", list, listener, ""); !strings.Contains(body, "Beta") {
		t.Fatalf("listener's list after the grant: %s", body)
	}
}
